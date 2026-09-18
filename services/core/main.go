// Command onyx-core is the control-plane orchestrator (docs/design/04): policy,
// service registry, audit trail. It listens on a unix socket and reaches the
// filesystem only through data-plane services such as onyx-storaged.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
	releaseversion "github.com/innotelinc/onyx/services/version"
)

var version = releaseversion.Version

func main() {
	var (
		socketDir      = flag.String("socket-dir", "/run/onyx", "directory for onyx unix sockets")
		stateDir       = flag.String("state-dir", "/var/lib/onyx/core", "service state directory (SQLite)")
		storagedSock   = flag.String("storaged-socket", "", "onyx-storaged socket (default: <socket-dir>/onyx-storaged.sock)")
		privdSock      = flag.String("privd-socket", "", "onyx-privd socket (default: <socket-dir>/onyx-privd.sock)")
		sharedSock     = flag.String("shared-socket", "", "onyx-shared socket (default: <socket-dir>/onyx-shared.sock)")
		snapdSock      = flag.String("snapd-socket", "", "onyx-snapd socket (default: <socket-dir>/onyx-snapd.sock)")
		backupdSock    = flag.String("backupd-socket", "", "onyx-backupd socket (default: <socket-dir>/onyx-backupd.sock)")
		vmmSock        = flag.String("vmm-socket", "", "onyx-vmm socket (default: <socket-dir>/onyx-vmm.sock)")
		appdSock       = flag.String("appd-socket", "", "onyx-appd socket (default: <socket-dir>/onyx-appd.sock)")
		aiSock         = flag.String("ai-socket", "", "onyx-ai socket (default: <socket-dir>/onyx-ai.sock)")
		objstoreSock   = flag.String("objectstore-socket", "", "onyx-objectstore socket (default: <socket-dir>/onyx-objectstore.sock)")
		reconcileEvery = flag.Duration("device-reconcile-interval", 2*time.Second, "how often shares are reconciled with the hotplug device list")
		mountRoot      = flag.String("device-mount-root", "/mnt/onyx", "only drives mounted under this root become auto shares")
	)
	flag.Parse()

	if err := os.MkdirAll(*socketDir, 0o750); err != nil {
		fatal("create socket dir", err)
	}
	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		fatal("create state dir", err)
	}
	if *storagedSock == "" {
		*storagedSock = absSocketPath(*socketDir, "onyx-storaged.sock")
	}
	if *privdSock == "" {
		*privdSock = absSocketPath(*socketDir, "onyx-privd.sock")
	}
	if *sharedSock == "" {
		*sharedSock = absSocketPath(*socketDir, "onyx-shared.sock")
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

	db, err := openDB(*stateDir)
	if err != nil {
		fatal("initialize database", err)
	}
	defer db.Close()

	storagedConn, err := grpc.NewClient(
		"unix://"+*storagedSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fatal("dial storaged", err)
	}
	defer storagedConn.Close()

	privdConn, err := grpc.NewClient(
		"unix://"+*privdSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fatal("dial privd", err)
	}
	defer privdConn.Close()

	sharedConn, err := grpc.NewClient(
		"unix://"+*sharedSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fatal("dial shared", err)
	}
	defer sharedConn.Close()

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

	sharedClient := onyxv1.NewSharedClient(sharedConn)
	privdClient := onyxv1.NewPrivdClient(privdConn)
	applier := newConfigApplier(db, sharedClient, privdClient)

	// Platform services (v0.4): dialed eagerly like the storage path so health
	// covers them, but only health is used here — the gateway calls their gRPC
	// contract directly. gRPC clients are lazy, so a service that is not
	// deployed yet just reports UNKNOWN instead of failing startup.
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

	gs := grpc.NewServer()
	srv := &server{
		db:             db,
		storaged:       onyxv1.NewStoragedClient(storagedConn),
		storagedHealth: onyxv1.NewHealthClient(storagedConn),
		sharedHealth:   onyxv1.NewHealthClient(sharedConn),
		privdHealth:    onyxv1.NewHealthClient(privdConn),
		snapdHealth:    onyxv1.NewHealthClient(snapdConn),
		backupdHealth:  onyxv1.NewHealthClient(backupdConn),
		vmmHealth:      onyxv1.NewHealthClient(vmmConn),
		appdHealth:     onyxv1.NewHealthClient(appdConn),
		aiHealth:       onyxv1.NewHealthClient(aiConn),
		objstoreHealth: onyxv1.NewHealthClient(objstoreConn),
		config:         applier,
	}
	onyxv1.RegisterHealthServer(gs, srv)
	onyxv1.RegisterCoreServer(gs, srv)
	onyxv1.RegisterCoreSharesServer(gs, srv)

	// The hotplug reconciler turns mounted devices into shares automatically
	// and removes them on detach; it runs for the lifetime of core.
	reconcilerCtx, stopReconciler := context.WithCancel(context.Background())
	rc := &deviceReconciler{
		db:        db,
		storaged:  onyxv1.NewStoragedClient(storagedConn),
		config:    applier,
		mountRoot: *mountRoot,
	}
	go rc.run(reconcilerCtx, *reconcileEvery)

	// Bring the daemon config in sync with whatever shares already exist on
	// boot (privd/shared may not be healthy yet — the reconcile loop retries
	// if this first pass fails).
	go func() {
		if err := applier.apply(context.Background()); err != nil {
			slog.Warn("initial daemon config apply deferred", "error", err)
		}
	}()

	sock := absSocketPath(*socketDir, "onyx-core.sock")
	_ = os.Remove(sock) // stale socket from a previous run
	lis, err := net.Listen("unix", sock)
	if err != nil {
		fatal("listen", err)
	}

	slog.Info("onyx-core listening", "socket", sock, "pid", os.Getpid())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("shutting down")
		stopReconciler()
		gs.GracefulStop()
	}()

	if err := gs.Serve(lis); err != nil {
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
