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

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

const version = "0.1.0-dev"

func main() {
	var (
		listen    = flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
		socketDir = flag.String("socket-dir", "/run/onyx", "directory for onyx unix sockets")
		coreSock  = flag.String("core-socket", "", "onyx-core socket (default: <socket-dir>/onyx-core.sock)")
	)
	flag.Parse()

	if *coreSock == "" {
		*coreSock = absSocketPath(*socketDir, "onyx-core.sock")
	}
	if err := os.MkdirAll(*socketDir, 0o750); err != nil {
		fatal("create socket dir", err)
	}

	coreConn, err := grpc.NewClient(
		"unix://"+*coreSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fatal("dial core", err)
	}
	defer coreConn.Close()

	core := onyxv1.NewCoreClient(coreConn)
	coreShares := onyxv1.NewCoreSharesClient(coreConn)

	srv := &server{core: core, coreShares: coreShares, deviceTrust: loadDeviceTrustConfig(), version: version}
	srv.registerRoutes()

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           withRequestID(withLogging(srv)),
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

// server is the HTTP handler; it forwards to onyx-core.
type server struct {
	core        onyxv1.CoreClient
	coreShares  onyxv1.CoreSharesClient
	deviceTrust *deviceTrustConfig
	version     string
	mux         *http.ServeMux
}

func (s *server) registerRoutes() {
	mux := http.NewServeMux()
	// v1 catalog (docs/design/06#5-endpoint-catalog) — skeleton subset.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/system/version", s.handleVersion)
	mux.HandleFunc("GET /api/v1/system/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/pools", s.handlePools)
	mux.HandleFunc("GET /api/v1/pools/{name}", s.handlePool)
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
		"api_version": "v1",
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
	writeJSON(w, http.StatusOK, protoMessage(resp))
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
// friendly names ("smb", "nfs") rather than proto enum values.
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
		switch strings.ToLower(p) {
		case "smb":
			req.Protocols = append(req.Protocols, onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB)
		case "nfs":
			req.Protocols = append(req.Protocols, onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS)
		default:
			writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: fmt.Sprintf("unknown protocol %q (expected smb or nfs)", p)})
			return
		}
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