// Command onyx-objectstore is the S3-compatible object storage + hybrid cloud
// service (docs/design/11 §6.6): bucket tiering (local / cloud / tiered) and
// object I/O over an S3-style HTTP endpoint (storage.onyx.innotel.us) plus the
// gRPC control contract. Cloud tiers write through the shared rclone remote
// catalog (the same one backupd and onyx-api use), and SyncBucket mirrors a
// bucket out and evicts only cloud-verified copies.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/innotelinc/onyx/services/vault"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
	releaseversion "github.com/innotelinc/onyx/services/version"
)

var version = releaseversion.Version

// resolveS3Credentials resolves the S3_ACCESS_KEY/S3_SECRET_KEY env values,
// preserving plain values. A value may be a `vault://<mount>/<path>#<key>`
// reference; plain values pass through, which is what a local run uses.
func resolveS3Credentials(ctx context.Context, vclient *vault.Client) ([2]string, error) {
	var out [2]string
	values := []string{os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY")}
	for i, v := range values {
		r, err := vclient.ResolveEnv(ctx, v)
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
		// Cloud tiers go through rclone (docs/design/11 §6.6). An empty
		// --rclone-bin disables them outright, which is the right setting for a
		// deployment that only wants local buckets.
		rcloneBin    = flag.String("rclone-bin", "rclone", "rclone binary backing CLOUD/TIERED buckets (empty disables cloud tiers)")
		rcloneConfig = flag.String("rclone-config", "/etc/rclone/rclone.conf", "shared rclone remote catalog")
	)
	flag.Parse()

	if err := os.MkdirAll(*socketDir, 0o750); err != nil {
		fatal("create socket dir", err)
	}
	if err := os.MkdirAll(filepath.Join(*stateDir, "objects"), 0o750); err != nil {
		fatal("create state dir", err)
	}

	// Static S3 credentials may be plain values or `vault://` references
	// (Cerulean Vault — SecretOps). Resolve refs at startup so a misconfigured
	// secret store fails loudly instead of silently opening the endpoint.
	vcfg := vault.ConfigFromEnv()
	if vcfg.Enabled() {
		resolved, err := resolveS3Credentials(context.Background(), vault.New(vcfg))
		if err != nil {
			fatal("resolve S3 credentials", err)
		}
		os.Setenv("S3_ACCESS_KEY", resolved[0])
		os.Setenv("S3_SECRET_KEY", resolved[1])
		slog.Info("secret resolution enabled", "vault", true)
	}

	// Cloud tiers are advertised only when the transport can actually run: a
	// bucket created against a missing rclone would fail on its first write, so
	// the condition is reported at startup instead.
	var cloud cloudTransport
	if *rcloneBin != "" {
		if _, err := exec.LookPath(*rcloneBin); err != nil {
			slog.Warn("rclone not found: CLOUD and TIERED buckets are unavailable", "binary", *rcloneBin, "error", err)
		} else {
			cloud = newRcloneTransport(*rcloneBin, *rcloneConfig)
			slog.Info("hybrid cloud enabled", "rclone", *rcloneBin, "config", *rcloneConfig)
		}
	}

	gs := grpc.NewServer()
	srv, err := newServer(*stateDir, cloud)
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
