// Command onyx-api is the HTTP/2 gateway (docs/design/02): REST + WebSocket,
// sessions, rate limiting, static UI. It authenticates clients and forwards
// orchestration to onyx-core over a local unix socket — it never talks to the
// data plane directly.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/innotelinc/onyx/services/vault"
	releaseversion "github.com/innotelinc/onyx/services/version"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

var version = releaseversion.Version

func main() {
	var (
		listen       = flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
		socketDir    = flag.String("socket-dir", "/run/onyx", "directory for onyx unix sockets")
		coreSock     = flag.String("core-socket", "", "onyx-core socket (default: <socket-dir>/onyx-core.sock)")
		snapdSock    = flag.String("snapd-socket", "", "onyx-snapd socket (default: <socket-dir>/onyx-snapd.sock)")
		backupdSock  = flag.String("backupd-socket", "", "onyx-backupd socket (default: <socket-dir>/onyx-backupd.sock)")
		vmmSock      = flag.String("vmm-socket", "", "onyx-vmm socket (default: <socket-dir>/onyx-vmm.sock)")
		appdSock     = flag.String("appd-socket", "", "onyx-appd socket (default: <socket-dir>/onyx-appd.sock)")
		aiSock       = flag.String("ai-socket", "", "onyx-ai socket (default: <socket-dir>/onyx-ai.sock)")
		objstoreSock = flag.String("objectstore-socket", "", "onyx-objectstore socket (default: <socket-dir>/onyx-objectstore.sock)")
		stateDir     = flag.String("state-dir", "/var/lib/onyx/api", "API metadata state directory")
		filesRoot    = flag.String("files-root", "/mnt/onyx", "root directory exposed by the read-only file explorer")
	)
	flag.Parse()

	if *coreSock == "" {
		*coreSock = absSocketPath(*socketDir, "onyx-core.sock")
	}
	if *snapdSock == "" {
		*snapdSock = absSocketPath(*socketDir, "onyx-snapd.sock")
	}
	if *backupdSock == "" {
		*backupdSock = absSocketPath(*socketDir, "onyx-backupd.sock")
	}
	if *vmmSock == "" {
		*vmmSock = absSocketPath(*socketDir, "onyx-vmm.sock")
	}
	if *appdSock == "" {
		*appdSock = absSocketPath(*socketDir, "onyx-appd.sock")
	}
	if *aiSock == "" {
		*aiSock = absSocketPath(*socketDir, "onyx-ai.sock")
	}
	if *objstoreSock == "" {
		*objstoreSock = absSocketPath(*socketDir, "onyx-objectstore.sock")
	}
	if err := os.MkdirAll(*socketDir, 0o750); err != nil {
		fatal("create socket dir", err)
	}
	users, err := newUserStore(*stateDir)
	if err != nil {
		fatal("load user metadata", err)
	}
	scrub, err := newScrubStore(*stateDir)
	if err != nil {
		fatal("load scrub schedules", err)
	}

	coreConn, err := grpc.NewClient(
		"unix://"+*coreSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fatal("dial core", err)
	}
	defer coreConn.Close()

	snapdConn, err := grpc.NewClient(
		"unix://"+*snapdSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fatal("dial snapd", err)
	}
	defer snapdConn.Close()

	backupdConn, err := grpc.NewClient(
		"unix://"+*backupdSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fatal("dial backupd", err)
	}
	defer backupdConn.Close()

	// Platform services (v0.4) are reached directly by the gateway, the same
	// way snapd/backupd already are: their contracts are self-contained
	// management surfaces, while core owns the storage path and the audit
	// trail. Each dial is lazy, so an undeployed service surfaces as a 503 on
	// its own routes rather than a startup failure.
	vmmConn, err := grpc.NewClient(
		"unix://"+*vmmSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fatal("dial vmm", err)
	}
	defer vmmConn.Close()

	appdConn, err := grpc.NewClient(
		"unix://"+*appdSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fatal("dial appd", err)
	}
	defer appdConn.Close()

	aiConn, err := grpc.NewClient(
		"unix://"+*aiSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fatal("dial ai", err)
	}
	defer aiConn.Close()

	objstoreConn, err := grpc.NewClient(
		"unix://"+*objstoreSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fatal("dial objectstore", err)
	}
	defer objstoreConn.Close()

	core := onyxv1.NewCoreClient(coreConn)
	coreShares := onyxv1.NewCoreSharesClient(coreConn)

	srv := &server{
		core: core, coreShares: coreShares,
		snapd:   onyxv1.NewSnapdClient(snapdConn),
		backupd: onyxv1.NewBackupdClient(backupdConn),
		vmm:     onyxv1.NewVmmClient(vmmConn),
		appd:    onyxv1.NewAppdClient(appdConn),
		ai:      onyxv1.NewAiClient(aiConn),
		objects: onyxv1.NewObjectStoreClient(objstoreConn),
		users:   users, scrub: scrub, filesRoot: *filesRoot, authentik: authentikFromEnv(),
		deviceTrust: loadDeviceTrustConfig(), version: version,
	}
	srv.registerRoutes()

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           withCORS(withRequestID(withLogging(srv))),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	slog.Info("onyx-api listening", "addr", *listen, "core", *coreSock)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal("serve", err)
	}
}

// absSocketPath absolutizes the socket path — gRPC's unix:// target parser
// treats any leading path segment as an authority, so relative paths break.
func absSocketPath(dir, name string) string {
	p := filepath.Join(dir, name)
	if filepath.IsAbs(p) {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func fatal(what string, err error) {
	slog.Error(what, "error", err)
	fmt.Fprintf(os.Stderr, "%s: %v\n", what, err)
	os.Exit(1)
}

// server is the HTTP handler; it forwards orchestration to onyx-core and
// management calls to the owning service.
type server struct {
	core        onyxv1.CoreClient
	coreShares  onyxv1.CoreSharesClient
	snapd       onyxv1.SnapdClient
	backupd     onyxv1.BackupdClient
	vmm         onyxv1.VmmClient
	appd        onyxv1.AppdClient
	ai          onyxv1.AiClient
	objects     onyxv1.ObjectStoreClient
	users       *userStore
	scrub       *scrubStore
	filesRoot   string
	deviceTrust *deviceTrustConfig
	// authentik is the identity provider the Users page reads and writes. It is
	// disabled (and says why) when the deployment has no AUTHENTIK_URL/token.
	authentik *authentikClient
	version   string
	mux       *http.ServeMux
}

func (s *server) registerRoutes() {
	mux := http.NewServeMux()
	// v1 catalog (docs/design/06#5-endpoint-catalog) — skeleton subset.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/system/version", s.handleVersion)
	mux.HandleFunc("GET /api/v1/system/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/files", s.handleFiles)
	mux.HandleFunc("GET /api/v1/files/meta", s.handleFileMeta)
	mux.HandleFunc("GET /api/v1/files/size", s.handleFileSize)
	mux.HandleFunc("GET /api/v1/files/archive", s.handleFileArchive)
	mux.HandleFunc("GET /api/v1/files/content", s.handleFileContent)
	mux.HandleFunc("GET /api/v1/storage/remotes", s.handleRcloneRemotes)
	mux.HandleFunc("GET /api/v1/storage/providers", s.handleRcloneProviders)
	// Cloud/remote storage setup (docs/design/05#5): the Shares page writes
	// remotes into the shared rclone config, probes them, and clones a storage
	// folder out to them.
	mux.HandleFunc("POST /api/v1/storage/remotes", s.handleCreateRemote)
	mux.HandleFunc("DELETE /api/v1/storage/remotes/{name}", s.handleDeleteRemote)
	mux.HandleFunc("POST /api/v1/storage/remotes/{name}/check", s.handleCheckRemote)
	mux.HandleFunc("POST /api/v1/storage/remotes/{name}/token", s.handleSetRemoteToken)
	mux.HandleFunc("POST /api/v1/storage/remotes/{name}/oauth/start", s.handleOAuthStart)
	// The provider redirects the operator's browser here, so it answers a page.
	mux.HandleFunc("GET /api/v1/storage/oauth/callback", s.handleOAuthCallback)
	mux.HandleFunc("POST /api/v1/storage/clone", s.handleCloneToRemote)
	mux.HandleFunc("POST /api/v1/files/upload", s.handleFileUpload)
	mux.HandleFunc("POST /api/v1/files/mkdir", s.handleFileMkdir)
	mux.HandleFunc("POST /api/v1/files/rename", s.handleFileRename)
	mux.HandleFunc("POST /api/v1/files/delete", s.handleFileDelete)
	mux.HandleFunc("GET /api/v1/files/trash", s.handleTrash)
	// Mounted-pool reality behind the Files page's storage card: capacity per
	// pool plus the reason nothing shows when the API cannot see a mount.
	mux.HandleFunc("GET /api/v1/storage/overview", s.handleStorageOverview)
	mux.HandleFunc("DELETE /api/v1/files/trash", s.handleEmptyTrash)
	mux.HandleFunc("GET /api/v1/pools", s.handlePools)
	mux.HandleFunc("POST /api/v1/pools", s.handleCreatePool)
	mux.HandleFunc("GET /api/v1/pools/{name}", s.handlePool)
	mux.HandleFunc("DELETE /api/v1/pools/{name}", s.handleDeletePool)
	mux.HandleFunc("GET /api/v1/shares", s.handleShares)
	mux.HandleFunc("POST /api/v1/shares", s.handleCreateShare)
	mux.HandleFunc("GET /api/v1/shares/{name}", s.handleShare)
	mux.HandleFunc("DELETE /api/v1/shares/{name}", s.handleDeleteShare)
	mux.HandleFunc("GET /api/v1/devices", s.handleDevices)
	// Device trust fleet view (docs/design/11 §10.3). Registered before the
	// {name} route so Go 1.22 pattern precedence picks the literal segment.
	mux.HandleFunc("GET /api/v1/devices/trust", s.handleDevicesTrust)
	mux.HandleFunc("GET /api/v1/devices/{name}", s.handleDevice)
	mux.HandleFunc("POST /api/v1/devices/{name}/attach", s.handleDeviceAttach)
	mux.HandleFunc("POST /api/v1/devices/{name}/detach", s.handleDeviceDetach)
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/events/stream", s.handleEventsStream)
	// v0.3 Obsidian safety surfaces: snapshot lifecycle and backup jobs.
	mux.HandleFunc("GET /api/v1/snapshots", s.handleSnapshots)
	mux.HandleFunc("POST /api/v1/snapshots", s.handleCreateSnapshot)
	mux.HandleFunc("DELETE /api/v1/snapshots/{id}", s.handleDeleteSnapshot)
	mux.HandleFunc("POST /api/v1/snapshots/{id}/rollback", s.handleRollbackSnapshot)
	mux.HandleFunc("GET /api/v1/backup-jobs", s.handleBackupJobs)
	mux.HandleFunc("POST /api/v1/backup-jobs", s.handleCreateBackupJob)
	mux.HandleFunc("DELETE /api/v1/backup-jobs/{id}", s.handleDeleteBackupJob)
	mux.HandleFunc("POST /api/v1/backup-jobs/{id}/run", s.handleRunBackup)
	mux.HandleFunc("GET /api/v1/backup-jobs/{id}/history", s.handleBackupHistory)
	mux.HandleFunc("POST /api/v1/backups/{id}/restore", s.handleRestoreBackup)
	mux.HandleFunc("GET /api/v1/backup-report", s.handleBackupReport)
	// v0.4 Jade platform surfaces (docs/design/11 §6.3-§6.6). Each forwards to
	// the service that owns the resource; the gateway only validates input and
	// maps the gRPC status onto the error envelope.
	mux.HandleFunc("GET /api/v1/apps", s.handleApps)
	// The store catalog and the installed inventory are the same onyx-appd
	// list seen through different filters (docs/design/06#5).
	mux.HandleFunc("GET /api/v1/app-store", s.handleApps)
	mux.HandleFunc("POST /api/v1/apps", s.handleInstallApp)
	mux.HandleFunc("DELETE /api/v1/apps/{id}", s.handleUninstallApp)
	mux.HandleFunc("GET /api/v1/containers", s.handleContainers)
	mux.HandleFunc("POST /api/v1/containers/{id}/start", s.handleContainerStart)
	mux.HandleFunc("POST /api/v1/containers/{id}/stop", s.handleContainerStop)
	mux.HandleFunc("POST /api/v1/containers/{id}/restart", s.handleContainerRestart)
	mux.HandleFunc("GET /api/v1/vms", s.handleVMs)
	mux.HandleFunc("POST /api/v1/vms", s.handleCreateVM)
	mux.HandleFunc("POST /api/v1/vms/{id}/start", s.handleVMStart)
	mux.HandleFunc("POST /api/v1/vms/{id}/stop", s.handleVMStop)
	mux.HandleFunc("DELETE /api/v1/vms/{id}", s.handleDeleteVM)
	mux.HandleFunc("GET /api/v1/buckets", s.handleBuckets)
	mux.HandleFunc("POST /api/v1/buckets", s.handleCreateBucket)
	mux.HandleFunc("DELETE /api/v1/buckets/{name}", s.handleDeleteBucket)
	mux.HandleFunc("POST /api/v1/buckets/{name}/sync", s.handleSyncBucket)
	mux.HandleFunc("GET /api/v1/ai/advisor", s.handleAdvisor)
	mux.HandleFunc("GET /api/v1/ai/backup-advice", s.handleBackupAdvice)
	// v0.2 Flint access metadata. Authentication remains delegated to Authentik;
	// this store contains ONYX roles and share grants only, never passwords.
	mux.HandleFunc("GET /api/v1/users", s.handleUsers)
	mux.HandleFunc("POST /api/v1/users", s.handleCreateUser)
	mux.HandleFunc("GET /api/v1/users/{id}", s.handleUser)
	mux.HandleFunc("PATCH /api/v1/users/{id}", s.handleUpdateUser)
	mux.HandleFunc("DELETE /api/v1/users/{id}", s.handleDeleteUser)
	mux.HandleFunc("GET /api/v1/users/{id}/permissions", s.handleUserPermissions)
	mux.HandleFunc("PUT /api/v1/users/{id}/permissions", s.handleSetUserPermissions)
	mux.HandleFunc("GET /api/v1/pools/{name}/scrub-schedule", s.handleScrubSchedule)
	mux.HandleFunc("PUT /api/v1/pools/{name}/scrub-schedule", s.handleSetScrubSchedule)
	mux.HandleFunc("POST /api/v1/pools/{name}/scrub", s.handleRunScrub)
	s.mux = mux
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *server) handleVersion(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.core.SystemStatus(ctx, &onyxv1.SystemStatusRequest{})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     resp.CoreVersion,
		"api_version": releaseversion.APIVersion,
		"codename":    releaseversion.Codename,
		"commit":      releaseversion.Commit,
	})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.core.SystemStatus(ctx, &onyxv1.SystemStatusRequest{})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	out := protoMessage(resp)
	// SecretOps health: "ok" | "not-configured" | "error: <detail>". Cerulean
	// Vault is the platform's only secret store, so this is the one SecretOps
	// field the status reports (docs/stack.md).
	out["vault"] = vault.New(vault.ConfigFromEnv()).Status(ctx)
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handlePools(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.core.ListPools(ctx, &onyxv1.ListPoolsRequest{})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleShares(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.coreShares.ListShares(ctx, &onyxv1.ListSharesRequest{})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

// createShareBody is the wire form of a share-create request. Protocols use
// friendly lowercase names ("smb", "nfs", "ftp", "sftp", "webdav", "rsync")
// rather than proto enum values.
type createShareBody struct {
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	Comment   string   `json:"comment"`
	Readonly  bool     `json:"readonly"`
	Protocols []string `json:"protocols"`
}

func (s *server) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	var body createShareBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid JSON body: " + err.Error()})
		return
	}
	req := &onyxv1.CreateShareRequest{
		Name:     body.Name,
		Path:     body.Path,
		Comment:  body.Comment,
		Readonly: body.Readonly,
	}
	for _, p := range body.Protocols {
		// "smb" -> SHARE_PROTOCOL_SMB; the generated enum table keeps this in
		// step with the contract as protocols are added (docs/design/05#6).
		v, ok := onyxv1.ShareProtocol_value["SHARE_PROTOCOL_"+strings.ToUpper(strings.TrimSpace(p))]
		proto := onyxv1.ShareProtocol(v)
		if !ok || proto == onyxv1.ShareProtocol_SHARE_PROTOCOL_UNSPECIFIED {
			writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: fmt.Sprintf("unknown protocol %q (expected smb, nfs, ftp, sftp, webdav or rsync)", p)})
			return
		}
		req.Protocols = append(req.Protocols, proto)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.coreShares.CreateShare(ctx, req)
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, protoMessage(resp))
}

func (s *server) handleShare(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.coreShares.GetShare(ctx, &onyxv1.GetShareRequest{Name: r.PathValue("name")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleDeleteShare(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := s.coreShares.DeleteShare(ctx, &onyxv1.DeleteShareRequest{Name: r.PathValue("name")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("name")})
}

// handlePool serves GET /api/v1/pools/{name}; storaged's not_found propagates
// as a 404 via writeGRPCError (docs/design/06#2-error-model).
type createPoolBody struct {
	Device string `json:"device"`
	Name   string `json:"name"`
	FsType string `json:"fs_type"`
	// Force unmounts busy mounts and overwrites existing filesystem
	// signatures. Defaults to true: creating a pool already means erasing the
	// disk, and the data plane refuses to format a busy device anyway.
	Force     *bool  `json:"force"`
	AutoMount *bool  `json:"auto_mount"`
	MountName string `json:"mount_name"`
}

func (s *server) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	var body createPoolBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid JSON body: " + err.Error()})
		return
	}
	if body.Device == "" || body.Name == "" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "device and name are required"})
		return
	}
	fsType := strings.ToLower(body.FsType)
	if fsType != "" && fsType != "btrfs" && fsType != "ext4" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "fs_type must be btrfs or ext4"})
		return
	}
	autoMount := true
	if body.AutoMount != nil {
		autoMount = *body.AutoMount
	}
	force := true
	if body.Force != nil {
		force = *body.Force
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	pool, err := s.core.CreatePool(ctx, &onyxv1.CreatePoolRequest{Device: body.Device, Name: body.Name, FsType: fsType, Force: force, AutoMount: autoMount, MountName: body.MountName})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, protoMessage(pool))
}

func (s *server) handlePool(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.core.GetPool(ctx, &onyxv1.GetPoolRequest{Name: r.PathValue("name")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

// handleDeletePool serves DELETE /api/v1/pools/{name} — forget a pool record.
//
// The pool's mount is released and its registry row dropped; the filesystem on
// the device is deliberately left alone, so the pool can be re-created or
// imported again. This is what clears a stale record: a pool whose disk was
// re-formatted, relabelled or pulled otherwise stays listed as an offline
// duplicate of the pool that replaced it, which is exactly how one disk comes to
// be reported three times.
func (s *server) handleDeletePool(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	name := r.PathValue("name")
	if name == "" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "pool name is required"})
		return
	}
	resp, err := s.core.DeletePool(ctx, &onyxv1.DeletePoolRequest{Name: name})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

// --- devices ---

// handleDevices serves GET /api/v1/devices — every block device the data
// plane has spotted, including mounted hotplug drives and recent removals.
func (s *server) handleDevices(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.core.ListDevices(ctx, &onyxv1.ListDevicesRequest{})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleDevice(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.core.GetDevice(ctx, &onyxv1.GetDeviceRequest{Name: r.PathValue("name")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

// handleDeviceAttach mounts the device and exposes it as a share. storaged's
// failed_precondition maps to 409-conflict territory; not_found to 404.
func (s *server) handleDeviceAttach(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := s.core.MountDevice(ctx, &onyxv1.MountDeviceRequest{Name: r.PathValue("name")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleDeviceDetach(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := s.core.UnmountDevice(ctx, &onyxv1.UnmountDeviceRequest{Name: r.PathValue("name")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

// --- events (audit trail + SSE live stream) ---

// handleEvents serves GET /api/v1/events — the device audit trail. Query
// params: limit (default 100), after_id (page forward from an event id),
// kname (filter to one device).
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	req := &onyxv1.ListEventsRequest{}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "limit must be a number"})
			return
		}
		req.Limit = uint32(n)
	}
	if v := r.URL.Query().Get("after_id"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "after_id must be a number"})
			return
		}
		req.AfterId = n
	}
	req.Kname = r.URL.Query().Get("kname")

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.core.ListEvents(ctx, req)
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

// handleEventsStream serves GET /api/v1/events/stream — Server-Sent Events
// (text/event-stream) tailing the live device event stream from
// onyx-storaged via onyx-core. One gRPC upstream per HTTP connection; when
// the client disconnects, the request context cancels the whole chain.
func (s *server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: "streaming unsupported by this connection"})
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stream, err := s.core.WatchDevices(ctx, &onyxv1.WatchDevicesRequest{})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	// Flush an initial comment so buffering proxies release the headers now.
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()

	for {
		ev, err := stream.Recv()
		if err != nil {
			return // client gone (ctx cancelled) or upstream closed; both are terminal
		}
		b, err := protojson.Marshal(ev)
		if err != nil {
			slog.Warn("events stream: marshal", "error", err)
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
}

// writeGRPCError maps a gRPC status to the HTTP error envelope
// (docs/design/06#2-error-model).
func (s *server) writeGRPCError(w http.ResponseWriter, r *http.Request, err error) {
	st, ok := status.FromError(err)
	if !ok {
		st = status.Convert(err)
	}
	code, httpStatus, retryable := "internal", http.StatusInternalServerError, false
	switch st.Code() {
	case codes.Unavailable, codes.Aborted:
		code, httpStatus, retryable = "service_unavailable", http.StatusServiceUnavailable, true
	case codes.DeadlineExceeded:
		code, httpStatus, retryable = "deadline_exceeded", http.StatusGatewayTimeout, true
	case codes.NotFound:
		code, httpStatus = "not_found", http.StatusNotFound
	case codes.InvalidArgument:
		code, httpStatus = "invalid_argument", http.StatusBadRequest
	case codes.PermissionDenied:
		code, httpStatus = "permission_denied", http.StatusForbidden
	case codes.Unauthenticated:
		code, httpStatus = "unauthenticated", http.StatusUnauthorized
	case codes.AlreadyExists:
		code, httpStatus = "already_exists", http.StatusConflict
	case codes.FailedPrecondition:
		code, httpStatus = "failed_precondition", http.StatusConflict
	}
	writeEnvelope(w, httpStatus, apiError{
		Code:      code,
		Message:   st.Message(),
		RequestID: rIDFrom(r),
		Retryable: retryable,
	})
}

// --- middleware ---

type ctxKey string

const rIDKey ctxKey = "onyx_request_id"

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "https://app.onyx.innotel.us" || origin == "http://localhost:2080" || origin == "http://127.0.0.1:2080" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, X-Onyx-Request-ID")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rID := "req_" + randHex(12)
		ctx := context.WithValue(r.Context(), rIDKey, rID)
		w.Header().Set("X-Onyx-Request-ID", rID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func rIDFrom(r *http.Request) string {
	if v, ok := r.Context().Value(rIDKey).(string); ok {
		return v
	}
	return ""
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start), "req_id", rIDFrom(r))
	})
}

// --- JSON plumbing ---

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	Retryable bool   `json:"retryable"`
}

func writeEnvelope(w http.ResponseWriter, httpStatus int, e apiError) {
	writeJSON(w, httpStatus, map[string]any{"error": e})
}

func writeJSON(w http.ResponseWriter, httpStatus int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(v)
}

// protoMessage renders a proto message as its JSON mapping (camelCase fields,
// enum names) — stable wire format for clients, per docs/design/06.
func protoMessage(m proto.Message) map[string]any {
	b, err := protojson.MarshalOptions{UseProtoNames: false}.Marshal(m)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
