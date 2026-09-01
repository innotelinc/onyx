// Command onyx-core is the control-plane orchestrator (docs/design/04): policy,
// service registry, audit trail. It listens on a unix socket and reaches the
// filesystem only through data-plane services such as onyx-storaged.
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
	"google.golang.org/grpc/credentials/insecure"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

const version = "0.1.0-dev"

func main() {
	var (
		socketDir    = flag.String("socket-dir", "/run/onyx", "directory for onyx unix sockets")
		stateDir     = flag.String("state-dir", "/var/lib/onyx/core", "service state directory (SQLite)")
		storagedSock = flag.String("storaged-socket", "", "onyx-storaged socket (default: <socket-dir>/onyx-storaged.sock)")
		privdSock    = flag.String("privd-socket", "", "onyx-privd socket (default: <socket-dir>/onyx-privd.sock)")
		sharedSock   = flag.String("shared-socket", "", "onyx-shared socket (default: <socket-dir>/onyx-shared.sock)")
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

	gs := grpc.NewServer()
	srv := &server{
		db:             db,
		shared:         onyxv1.NewSharedClient(sharedConn),
		storaged:       onyxv1.NewStoragedClient(storagedConn),
		storagedHealth: onyxv1.NewHealthClient(storagedConn),
		sharedHealth:   onyxv1.NewHealthClient(sharedConn),
		privdHealth:    onyxv1.NewHealthClient(privdConn),
	}
	onyxv1.RegisterHealthServer(gs, srv)
	onyxv1.RegisterCoreServer(gs, srv)
	onyxv1.RegisterCoreSharesServer(gs, srv)

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