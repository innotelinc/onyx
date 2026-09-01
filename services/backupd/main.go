// Command onyx-backupd is the backup service (docs/design/11 §6.2): jobs,
// schedules, retention, restore, and the Backup Intelligence report consumed
// by onyx-ai. v0.1 ships the contract + in-memory skeleton with a JSON API
// surface for backup.onyx.innotel.us; the run engine lands with v0.3.
package main

import (
	"context"
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

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

const version = "0.1.0-dev"

func main() {
	var (
		socketDir = flag.String("socket-dir", "/run/onyx", "directory for onyx unix sockets")
		tcpListen = flag.String("tcp-listen", "", "optional gRPC TCP listen address (e.g. 0.0.0.0:9094) for containerized deployments")
		httpAddr  = flag.String("http-listen", "", "optional HTTP listen address for the JSON API (e.g. 0.0.0.0:8084)")
		stateDir  = flag.String("state-dir", "/var/lib/onyx/backupd", "service state directory")
	)
	flag.Parse()

	if err := os.MkdirAll(*socketDir, 0o750); err != nil {
		fatal("create socket dir", err)
	}
	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		fatal("create state dir", err)
	}

	gs := grpc.NewServer()
	srv := newServer()
	onyxv1.RegisterHealthServer(gs, srv)
	onyxv1.RegisterBackupdServer(gs, srv)

	var httpSrv *http.Server
	if *httpAddr != "" {
		httpSrv = &http.Server{Addr: *httpAddr, Handler: newHTTPHandler(srv)}
		go func() {
			slog.Info("onyx-backupd http api", "addr", *httpAddr)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("http api", "error", err)
			}
		}()
	}

	lis, err := listen(*socketDir, "onyx-backupd.sock", *tcpListen)
	if err != nil {
		fatal("listen", err)
	}
	slog.Info("onyx-backupd listening", "addr", lis.Addr().String(), "pid", os.Getpid())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("shutting down")
		if httpSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(ctx)
		}
		gs.GracefulStop()
	}()

	if err := gs.Serve(lis); err != nil {
		fatal("serve", err)
	}
}

func listen(socketDir, sockName, tcp string) (net.Listener, error) {
	if tcp != "" {
		return net.Listen("tcp", tcp)
	}
	sock := filepath.Join(socketDir, sockName)
	_ = os.Remove(sock) // stale socket from a previous run
	return net.Listen("unix", sock)
}

func fatal(what string, err error) {
	slog.Error(what, "error", err)
	fmt.Fprintf(os.Stderr, "%s: %v\n", what, err)
	os.Exit(1)
}
