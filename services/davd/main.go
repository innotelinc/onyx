// Command onyx-davd is the WebDAV endpoint for WebDAV-enabled shares
// (docs/design/05#6-WebDAV). It reads the davd.conf onyx-shared renders,
// serves each share as a WebDAV collection, and takes no part in rendering or
// applying config: onyx-core writes the file, onyx-privd reloads this daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
	releaseversion "github.com/innotelinc/onyx/services/version"
)

var version = releaseversion.Version

// server implements Health for the WebDAV daemon. The WebDAV surface itself is
// HTTP, not gRPC, so Health is all this socket carries — it is what onyx-core's
// SystemStatus reports.
type server struct {
	onyxv1.UnimplementedHealthServer
}

var _ onyxv1.HealthServer = (*server)(nil)

func (s *server) Check(_ context.Context, _ *onyxv1.HealthCheckRequest) (*onyxv1.HealthCheckResponse, error) {
	return &onyxv1.HealthCheckResponse{
		Status:  onyxv1.HealthCheckResponse_SERVING,
		Version: version,
	}, nil
}

func main() {
	var (
		socketDir  = flag.String("socket-dir", "/run/onyx", "directory for onyx unix sockets")
		configPath = flag.String("config", defaultConfigPath, "rendered davd.conf written by onyx-core")
		listen     = flag.String("listen", "", "override the listen address from davd.conf")
	)
	flag.Parse()

	if err := os.MkdirAll(*socketDir, 0o750); err != nil {
		fatal("create socket dir", err)
	}

	cfg, err := loadConfig(*configPath)
	switch {
	case errors.Is(err, errNoConfig):
		// Nothing to serve *yet* is not a failure. onyx-core renders davd.conf
		// when the first share enables WebDAV; the systemd unit is gated on that
		// file, but a container restart-looping until an operator creates a
		// share is not a useful answer. Serve an empty table on the default
		// (loopback, gateway-auth) listener and pick the file up when it lands.
		slog.Warn("no share configuration yet: serving an empty WebDAV table",
			"config", *configPath, "hint", "onyx-core writes it when a share enables WebDAV")
		cfg = defaultConfig()
	case err != nil:
		fatal("load "+*configPath, err)
	}
	if err := applyListenOverride(cfg, *listen); err != nil {
		fatal("invalid --listen", err)
	}
	slog.Info("onyx-davd configuration loaded",
		"config", *configPath, "listen", cfg.Listen, "auth", cfg.Auth, "shares", len(cfg.Shares))

	// Refusals go into onyx-core's access audit trail, beside the grant that
	// caused them (docs/design/08#2).
	recordDenial = auditDenials(*socketDir)

	active := &reloadable{}
	active.swap(newHandler(cfg), cfg.Shares)
	boundListen := cfg.Listen

	// Watch the rendered config: the first share that enables WebDAV is served
	// without a restart, and a later change (a share, or a grant) is applied
	// even where nothing can signal this process. onyx-privd reloads it through
	// systemd on an appliance, but a container has no systemd, and the file
	// onyx-core wrote is right there. SIGHUP stays supported for an explicit
	// reload.
	//
	// The baseline is what was just loaded, captured here rather than inside the
	// watcher: a revision written between the load and the watcher's first tick
	// would otherwise be mistaken for the file it started with.
	go watchConfig(*configPath, *listen, active, boundListen, fingerprint(*configPath), configWatchInterval)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           active,
		ReadHeaderTimeout: 10 * time.Second,
	}
	httpErr := make(chan error, 1)
	go func() {
		slog.Info("onyx-davd webdav listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			httpErr <- err
		}
	}()

	gs := grpc.NewServer()
	onyxv1.RegisterHealthServer(gs, &server{})
	sock := absSocketPath(*socketDir, "onyx-davd.sock")
	_ = os.Remove(sock) // stale socket from a previous run
	lis, err := net.Listen("unix", sock)
	if err != nil {
		fatal("listen", err)
	}
	slog.Info("onyx-davd listening", "socket", sock, "pid", os.Getpid())

	go func() {
		if err := gs.Serve(lis); err != nil {
			fatal("serve", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case s := <-sig:
			if s == syscall.SIGHUP {
				reload(active, *configPath, *listen, boundListen)
				continue
			}
			slog.Info("shutting down")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = httpSrv.Shutdown(ctx)
			cancel()
			gs.GracefulStop()
			return
		case err := <-httpErr:
			fatal("serve webdav", err)
		}
	}
}

// configWatchInterval is how often the rendered config is checked for a change.
// The file is tiny and changes only when a share or a grant does, so one stat
// every few seconds costs nothing and is far more portable than an inotify
// dependency — and it is what makes a container deployment apply a change it
// cannot be signalled about.
const configWatchInterval = 3 * time.Second

// watchConfig re-reads the rendered config whenever its contents change, and
// serves it as soon as it first appears (the baseline is empty in that case). A
// revision that fails to load leaves the previous share table serving: reload
// never swaps in a bad config, and the fingerprint still advances so a broken
// render is logged once, not once per tick.
func watchConfig(path, listenOverride string, active *reloadable, boundListen, baseline string, interval time.Duration) {
	last := baseline
	for range time.Tick(interval) {
		current := fingerprint(path)
		// Empty means the file is not there (not rendered yet, or the instant
		// between onyx-privd's tmp file and the rename that publishes it), which
		// is not a revision to load.
		if current == "" || current == last {
			continue
		}
		last = current
		reload(active, path, listenOverride, boundListen)
	}
}

// fingerprint identifies one revision of a file by size and mtime. onyx-privd
// writes atomically (tmp -> fsync -> rename), so a new revision always has a
// fresh mtime; reading the content every tick to hash it would be wasted work.
func fingerprint(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())
}

// reload re-reads the config on SIGHUP (the path onyx-privd uses for a WebDAV
// reload). A config that fails to load or validate leaves the previous share
// table serving: a bad render must never take the shares offline.
func reload(active *reloadable, path, listenOverride, boundListen string) {
	cfg, err := loadConfig(path)
	if err != nil {
		if errors.Is(err, errNoConfig) {
			slog.Warn("no share configuration has been rendered yet, keeping the current configuration", "config", path)
			return
		}
		slog.Error("reload failed, keeping the current configuration", "config", path, "error", err)
		return
	}
	if err := applyListenOverride(cfg, listenOverride); err != nil {
		slog.Error("reload failed, keeping the current configuration", "config", path, "error", err)
		return
	}
	active.swap(newHandler(cfg), cfg.Shares)
	// The share table is swapped in place, but the socket is not: a changed
	// listen address takes effect on the next start (privd restarts this
	// daemon when the rendered listener changes).
	if boundListen != "" && cfg.Listen != boundListen {
		slog.Warn("configuration reloaded, but the listen address changed and needs a restart",
			"listening", boundListen, "configured", cfg.Listen, "shares", len(cfg.Shares))
		return
	}
	slog.Info("configuration reloaded", "listen", cfg.Listen, "shares", len(cfg.Shares))
}

// applyListenOverride re-applies --listen to a freshly loaded config and
// re-validates it. The override has to survive every reload: validation is what
// keeps `auth = "none"` off a routable address, and it would otherwise be
// checked against the file's loopback address while the socket listens on
// 0.0.0.0.
func applyListenOverride(cfg *config, listenOverride string) error {
	if listenOverride == "" {
		return nil
	}
	cfg.Listen = listenOverride
	return cfg.validate()
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
