// Command onyx-vmm is the virtualization service (docs/design/11 §6.3): VM
// inventory, disk images and lifecycle on a libvirt/QEMU backend. Definitions
// persist in SQLite; the domains themselves are defined and driven through
// `virsh` (never CGo bindings), so the service runs as its own unprivileged
// user in the libvirt group.
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
		socketDir = flag.String("socket-dir", "/run/onyx", "directory for onyx unix sockets")
		tcpListen = flag.String("tcp-listen", "", "optional TCP listen address (e.g. 0.0.0.0:9095) for containerized deployments")
		stateDir  = flag.String("state-dir", "/var/lib/onyx/vmm", "service state directory")
		// The disk root is inside a pool subvolume (docs/design/05#2): VM images
		// are ordinary pool files, so they are snapshotted and backed up with
		// everything else.
		diskRoot = flag.String("disk-root", "/mnt/onyx/main-pool/@apps/vms", "directory holding VM qcow2 images")
		xmlRoot  = flag.String("domain-dir", "", "where domain XML is written (default: <state-dir>/domains)")
		virshBin = flag.String("virsh", "virsh", "libvirt CLI")
		qemuImg  = flag.String("qemu-img", "qemu-img", "qemu-img binary")
		network  = flag.String("network", "default", "libvirt network attached to VM interfaces")
	)
	flag.Parse()

	if err := os.MkdirAll(*socketDir, 0o750); err != nil {
		fatal("create socket dir", err)
	}
	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		fatal("create state dir", err)
	}
	if *xmlRoot == "" {
		*xmlRoot = filepath.Join(*stateDir, "domains")
	}

	st, err := openStore(*stateDir)
	if err != nil {
		fatal("open store", err)
	}
	defer st.Close()

	gs := grpc.NewServer()
	srv := newServer(st, newLibvirtHypervisor(*virshBin, *qemuImg, *diskRoot, *xmlRoot, *network), *diskRoot)
	onyxv1.RegisterHealthServer(gs, srv)
	onyxv1.RegisterVmmServer(gs, srv)
	slog.Info("onyx-vmm ready", "disk_root", *diskRoot, "domains", *xmlRoot, "virsh", *virshBin)

	lis, err := listen(*socketDir, "onyx-vmm.sock", *tcpListen)
	if err != nil {
		fatal("listen", err)
	}
	slog.Info("onyx-vmm listening", "addr", lis.Addr().String(), "pid", os.Getpid())

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
