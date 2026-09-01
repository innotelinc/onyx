// Command onyx-snapd is the snapshot service (docs/design/11 §6.1): Btrfs
// snapshot lifecycle on the pool's fixed subvolume layout (@snapshots).
// v0.1 ships the contract + in-memory skeleton; the btrfs data plane lands
// with the v0.3 milestone.
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

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

const version = "0.1.0-dev"

func main() {
	var (
		socketDir = flag.String("socket-dir", "/run/onyx", "directory for onyx unix sockets")
		tcpListen = flag.String("tcp-listen", "", "optional TCP listen address (e.g. 0.0.0.0:9093) for containerized deployments")
		stateDir  = flag.String("state-dir", "/var/lib/onyx/snapd", "service state directory")
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
	onyxv1.RegisterSnapdServer(gs, srv)

	lis, err := listen(*socketDir, "onyx-snapd.sock", *tcpListen)
	if err != nil {
		fatal("listen", err)
	}
	slog.Info("onyx-snapd listening", "addr", lis.Addr().String(), "pid", os.Getpid())

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
