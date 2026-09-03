// Command onyx-objectstore is the S3-compatible object storage + hybrid cloud
// service (docs/design/11 §6.6): bucket tiering (local / cloud / tiered) and
// object I/O over an S3-style HTTP endpoint (storage.onyx.innotel.us) plus
// the gRPC control contract. v0.1 ships the working local engine + tier
// metadata skeleton; hybrid-cloud sync lands with v0.4.
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

	"onyx.dev/onyx/services/infisical"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

const version = "0.1.0-dev"

// resolveS3Credentials resolves the S3_ACCESS_KEY/S3_SECRET_KEY env values
// through the Infisical resolver, preserving plain values.
func resolveS3Credentials(ctx context.Context, iclient *infisical.Client) ([2]string, error) {
	var out [2]string
	values := []string{os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY")}
	for i, v := range values {
		r, err := iclient.ResolveEnv(ctx, v)
		if err != nil {
			return out, err
		}
		out[i] = r
	}
	return out, nil
}

func main() {
	var (
		socketDir = flag.String("socket-dir", "/run/onyx", "directory for onyx unix sockets")
		tcpListen = flag.String("tcp-listen", "", "optional gRPC TCP listen address (e.g. 0.0.0.0:9098) for containerized deployments")
		httpAddr  = flag.String("http-listen", "", "optional S3 endpoint listen address (e.g. 0.0.0.0:9000)")
		stateDir  = flag.String("state-dir", "/var/lib/onyx/objectstore", "service state directory (bucket metadata + objects)")
	)
	flag.Parse()

	if err := os.MkdirAll(*socketDir, 0o750); err != nil {
		fatal("create socket dir", err)
	}
	if err := os.MkdirAll(filepath.Join(*stateDir, "objects"), 0o750); err != nil {
		fatal("create state dir", err)
	}

	// Static S3 credentials may be plain values or infisical://<name>
	// references (SecretOps — the Innotel Platform Stack contract). Resolve
	// refs at startup so a misconfigured secret store fails loudly instead of
	// silently opening the endpoint.
	icfg := infisical.ConfigFromEnv()
	if icfg.Enabled() {
		iclient := infisical.New(icfg)
		resolved, err := resolveS3Credentials(context.Background(), iclient)
		if err != nil {
			fatal("resolve S3 credentials", err)
		}
		// Seed Infisical with plain .env values (best-effort) so the stack can
		// switch .env to references after the first boot.
		if written, errs := iclient.Mirror(context.Background(), map[string]string{
			"S3_ACCESS_KEY": os.Getenv("S3_ACCESS_KEY"),
			"S3_SECRET_KEY": os.Getenv("S3_SECRET_KEY"),
		}); len(written) > 0 {
			slog.Info("mirrored S3 credentials into Infisical", "secrets", written)
		} else {
			for _, e := range errs {
				slog.Warn("infisical mirror", "error", e)
			}
		}
		os.Setenv("S3_ACCESS_KEY", resolved[0])
		os.Setenv("S3_SECRET_KEY", resolved[1])
		slog.Info("infisical secret resolution enabled", "addr", icfg.Addr)
	}

	gs := grpc.NewServer()
	srv, err := newServer(*stateDir)
	if err != nil {
		fatal("load state", err)
	}
	onyxv1.RegisterHealthServer(gs, srv)
	onyxv1.RegisterObjectStoreServer(gs, srv)

	var httpSrv *http.Server
	if *httpAddr != "" {
		httpSrv = &http.Server{Addr: *httpAddr, Handler: newS3Handler(srv)}
		go func() {
			slog.Info("onyx-objectstore s3 endpoint", "addr", *httpAddr)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("s3 endpoint", "error", err)
			}
		}()
	}

	lis, err := listen(*socketDir, "onyx-objectstore.sock", *tcpListen)
	if err != nil {
		fatal("listen", err)
	}
	slog.Info("onyx-objectstore listening", "addr", lis.Addr().String(), "pid", os.Getpid())

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
