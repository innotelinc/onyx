// Command onyx-ai is the AI Storage Advisor + Backup Intelligence service
// (docs/design/11 §6.5): deterministic storage/backup heuristics in-process,
// with a model plane that turns the same findings into natural-language advice.
//
// The plane is the platform's shared OmniRoute
// (OMNIROUTE_BASE_URL + OMNIROUTE_API_KEY, resolved from Cerulean Vault when
// the value is a `vault://` reference); the BYO-key hook
// (AI_PROVIDER/AI_API_KEY/AI_MODEL) is kept as the documented local escape
// hatch. With neither set, the heuristics and the local summary are the whole
// answer and **no telemetry leaves the box**. ONYX never holds an upstream
// provider key: the gateway does.
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

	"google.golang.org/grpc"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

const version = "0.1.0-dev"

func main() {
	var (
		socketDir = flag.String("socket-dir", "/run/onyx", "directory for onyx unix sockets")
		tcpListen = flag.String("tcp-listen", "", "optional TCP listen address (e.g. 0.0.0.0:9097) for containerized deployments")
		stateDir  = flag.String("state-dir", "/var/lib/onyx/ai", "service state directory")
	)
	flag.Parse()

	if err := os.MkdirAll(*socketDir, 0o750); err != nil {
		fatal("create socket dir", err)
	}
	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		fatal("create state dir", err)
	}

	// Resolve `vault://` references in the credential-shaped env values before
	// anything reads them — a reference that cannot be resolved stops the boot
	// rather than becoming an empty credential.
	if err := resolveSecretEnv(context.Background(), "OMNIROUTE_API_KEY", "AI_API_KEY"); err != nil {
		fatal("resolve model-plane credentials", err)
	}

	gs := grpc.NewServer()
	srv := newServer()
	onyxv1.RegisterHealthServer(gs, srv)
	onyxv1.RegisterAiServer(gs, srv)

	lis, err := listen(*socketDir, "onyx-ai.sock", *tcpListen)
	if err != nil {
		fatal("listen", err)
	}
	slog.Info("onyx-ai listening", "addr", lis.Addr().String(), "pid", os.Getpid())

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
