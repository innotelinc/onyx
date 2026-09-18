// Command onyx-appd is the application/container management service
// (docs/design/11 §6.4): the curated app catalog, install lifecycle, and
// container operations. Installations and containers are persisted in SQLite
// and every action runs against the compose engine, so an app survives a
// restart and its containers are the engine's real ones (docs/design/09).
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"google.golang.org/grpc"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
	releaseversion "github.com/innotelinc/onyx/services/version"
)

var version = releaseversion.Version

func main() {
	var (
		socketDir  = flag.String("socket-dir", "/run/onyx", "directory for onyx unix sockets")
		tcpListen  = flag.String("tcp-listen", "", "optional TCP listen address (e.g. 0.0.0.0:9096) for containerized deployments")
		stateDir   = flag.String("state-dir", "/var/lib/onyx/appd", "service state directory")
		appRoot    = flag.String("app-root", "", "where app compose projects are written (default: <state-dir>/apps)")
		composeBin = flag.String("compose-bin", "docker", "container engine CLI (docker compose)")
		hostName   = flag.String("host", "", "public hostname apps use for their own URLs (e.g. onyx.example.com)")
	)
	flag.Parse()

	if err := os.MkdirAll(*socketDir, 0o750); err != nil {
		fatal("create socket dir", err)
	}
	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		fatal("create state dir", err)
	}
	if *appRoot == "" {
		*appRoot = filepath.Join(*stateDir, "apps")
	}
	if err := os.MkdirAll(*appRoot, 0o750); err != nil {
		fatal("create app root", err)
	}

	apps := catalog()
	if err := validateCatalog(apps); err != nil {
		fatal("invalid app catalog", err)
	}
	st, err := openStore(*stateDir)
	if err != nil {
		fatal("open store", err)
	}
	defer st.Close()

	gs := grpc.NewServer()
	srv := newServer(apps, st, newComposeRuntime(*composeBin, *appRoot), *hostName)
	onyxv1.RegisterHealthServer(gs, srv)
	onyxv1.RegisterAppdServer(gs, srv)
	slog.Info("onyx-appd ready", "catalog", len(apps), "engine", *composeBin, "app_root", *appRoot)

	lis, err := listen(*socketDir, "onyx-appd.sock", *tcpListen)
	if err != nil {
		fatal("listen", err)
	}
	slog.Info("onyx-appd listening", "addr", lis.Addr().String(), "pid", os.Getpid())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("shutting down")
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
