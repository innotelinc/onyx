package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Hypervisor is the virtualization backend behind onyx-vmm (docs/design/11
// §6.3). The libvirt implementation below drives a defined domain through
// `virsh` and creates its qcow2 image with `qemu-img`; tests inject a fake so
// the service's policy can be checked without libvirt.
type Hypervisor interface {
	// Create defines a domain for the VM (disk image + domain XML) without
	// starting it. Returns the disk image path.
	Create(ctx context.Context, spec vmSpec) (string, error)
	// Start boots the domain.
	Start(ctx context.Context, name string) error
	// Stop shuts the domain down: a graceful ACPI request when graceful, an
	// immediate power-off otherwise.
	Stop(ctx context.Context, name string, graceful bool) error
	// Remove undefines the domain and, when deleteDisk is set, deletes the image.
	Remove(ctx context.Context, name, diskPath string, deleteDisk bool) error
	// State reports the domain's live state as running | stopped | paused | error.
	State(ctx context.Context, name string) (string, error)
}

// vmSpec is everything needed to define one domain.
type vmSpec struct {
	Name     string
	VCPUs    int32
	MemoryMB int64
	DiskMB   int64
	DiskPath string
	OS       string
	ISO      string
}

// runner runs one command and returns its stdout, with stderr folded into the
// error. A field on the backend so tests can assert the exact argv.
type runner func(ctx context.Context, name string, args ...string) (string, error)

func execRunner(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return string(out), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), detail)
	}
	return string(out), nil
}

type libvirtHypervisor struct {
	virsh    string
	qemuImg  string
	diskRoot string
	xmlRoot  string
	// network is the libvirt network attached to every VM's interface.
	network string
	run     runner
	timeout time.Duration
}

func newLibvirtHypervisor(virshBin, qemuImgBin, diskRoot, xmlRoot, network string) *libvirtHypervisor {
	return &libvirtHypervisor{
		virsh:    virshBin,
		qemuImg:  qemuImgBin,
		diskRoot: diskRoot,
		xmlRoot:  xmlRoot,
		network:  network,
		run:      execRunner,
		timeout:  2 * time.Minute,
	}
}

func (h *libvirtHypervisor) runTimeout(ctx context.Context, name string, args ...string) (string, error) {
	if h.timeout <= 0 {
		return h.run(ctx, name, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	return h.run(ctx, name, args...)
}

// diskPath is where a VM's qcow2 image lives: one directory per pool subvolume
// convention (docs/design/05#2: /mnt/onyx/<pool>/@apps/vms).
func (h *libvirtHypervisor) diskPath(name string) string {
	return filepath.Join(h.diskRoot, name+".qcow2")
}

func (h *libvirtHypervisor) Create(ctx context.Context, spec vmSpec) (string, error) {
	if spec.DiskPath == "" {
		spec.DiskPath = h.diskPath(spec.Name)
	}
	if spec.DiskMB <= 0 {
		return "", errors.New("disk size must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(spec.DiskPath), 0o750); err != nil {
		return "", fmt.Errorf("create disk directory: %w", err)
	}
	if _, err := os.Stat(spec.DiskPath); err == nil {
		return "", fmt.Errorf("disk image %s already exists", spec.DiskPath)
	}
	// qemu-img refuses to overwrite, so a failed create cannot clobber an
	// existing image — the Stat above exists to say so in plain language.
	size := fmt.Sprintf("%dM", spec.DiskMB)
	if _, err := h.runTimeout(ctx, h.qemuImg, "create", "-f", "qcow2", spec.DiskPath, size); err != nil {
		return "", err
	}

	xmlDoc, err := renderDomainXML(spec, h.network)
	if err != nil {
		_ = os.Remove(spec.DiskPath) // do not leave an orphan image behind
		return "", err
	}
	if err := os.MkdirAll(h.xmlRoot, 0o750); err != nil {
		return "", fmt.Errorf("create domain directory: %w", err)
	}
	xmlPath := filepath.Join(h.xmlRoot, spec.Name+".xml")
	if err := os.WriteFile(xmlPath, xmlDoc, 0o640); err != nil {
		_ = os.Remove(spec.DiskPath)
		return "", fmt.Errorf("write domain xml: %w", err)
	}
	if _, err := h.runTimeout(ctx, h.virsh, "define", xmlPath); err != nil {
		_ = os.Remove(spec.DiskPath)
		return "", err
	}
	return spec.DiskPath, nil
}

func (h *libvirtHypervisor) Start(ctx context.Context, name string) error {
	_, err := h.runTimeout(ctx, h.virsh, "start", name)
	return err
}

// Stop maps the caller's intent onto libvirt: a graceful ACPI shutdown for a
// cooperative guest, an immediate power-off when the operator asks for it (the
// contract's `graceful = false`).
func (h *libvirtHypervisor) Stop(ctx context.Context, name string, graceful bool) error {
	verb := "destroy"
	if graceful {
		verb = "shutdown"
	}
	_, err := h.runTimeout(ctx, h.virsh, verb, name)
	return err
}

func (h *libvirtHypervisor) Remove(ctx context.Context, name, diskPath string, deleteDisk bool) error {
	if _, err := h.runTimeout(ctx, h.virsh, "undefine", name); err != nil {
		return err
	}
	if xmlPath := filepath.Join(h.xmlRoot, name+".xml"); xmlPath != "" {
		_ = os.Remove(xmlPath)
	}
	if !deleteDisk {
		return nil
	}
	if diskPath == "" {
		return nil
	}
	if err := os.Remove(diskPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete disk image %s: %w", diskPath, err)
	}
	return nil
}

func (h *libvirtHypervisor) State(ctx context.Context, name string) (string, error) {
	out, err := h.runTimeout(ctx, h.virsh, "domstate", name)
	if err != nil {
		return "", err
	}
	return domainState(out), nil
}

// domainState maps libvirt's own vocabulary onto the contract's
// (stopped | running | paused | error).
func domainState(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "running":
		return "running"
	case "paused", "pmsuspended":
		return "paused"
	case "shut off", "in shutdown", "no state", "shutdown":
		return "stopped"
	case "crashed":
		return "error"
	default:
		return "error"
	}
}

// --- domain XML ---

// renderDomainXML builds the libvirt domain definition for one VM. It is built
// with encoding/xml rather than string concatenation so a VM name or ISO path
// containing XML-special characters cannot break the document.
func renderDomainXML(spec vmSpec, network string) ([]byte, error) {
	bootDev := "hd"
	if spec.ISO != "" {
		bootDev = "cdrom"
	}
	diskPath, err := filepath.Abs(spec.DiskPath)
	if err != nil {
		return nil, fmt.Errorf("resolve disk path: %w", err)
	}

	doc := domain{
		Type:   "kvm",
		Name:   spec.Name,
		Memory: domainMemory{Unit: "MiB", Value: spec.MemoryMB},
		VCPU:   spec.VCPUs,
		OS: domainOS{
			Type: domainOSType{Arch: "x86_64", Text: "hvm"},
			Boot: domainBoot{Dev: bootDev},
		},
		Features: domainFeatures{ACPI: &struct{}{}, APIC: &struct{}{}},
		Devices: domainDevices{
			Disks: []domainDisk{{
				Type: "file", Device: "disk",
				Driver: domainDriver{Name: "qemu", Type: "qcow2"},
				Source: domainSource{File: diskPath},
				Target: domainTarget{Dev: "vda", Bus: "virtio"},
			}},
			Interface: domainInterface{
				Type: "network", Source: domainNetworkSource{Network: network},
				Model: domainModel{Type: "virtio"},
			},
			Graphics: domainGraphics{Type: "vnc", Port: -1, AutoPort: "yes", Listen: "127.0.0.1"},
			Console:  domainConsole{Type: "pty"},
		},
	}
	if spec.ISO != "" {
		isoPath, err := filepath.Abs(spec.ISO)
		if err != nil {
			return nil, fmt.Errorf("resolve iso path: %w", err)
		}
		doc.Devices.Disks = append(doc.Devices.Disks, domainDisk{
			Type: "file", Device: "cdrom",
			Driver:   domainDriver{Name: "qemu", Type: "raw"},
			Source:   domainSource{File: isoPath},
			Target:   domainTarget{Dev: "sda", Bus: "sata"},
			Readonly: &struct{}{},
		})
	}
	return xml.MarshalIndent(doc, "", "  ")
}

type domain struct {
	XMLName  xml.Name       `xml:"domain"`
	Type     string         `xml:"type,attr"`
	Name     string         `xml:"name"`
	Memory   domainMemory   `xml:"memory"`
	VCPU     int32          `xml:"vcpu"`
	OS       domainOS       `xml:"os"`
	Features domainFeatures `xml:"features"`
	Devices  domainDevices  `xml:"devices"`
}

type domainMemory struct {
	Unit  string `xml:"unit,attr"`
	Value int64  `xml:",chardata"`
}

type domainOS struct {
	Type domainOSType `xml:"type"`
	Boot domainBoot   `xml:"boot"`
}

type domainOSType struct {
	Arch string `xml:"arch,attr"`
	Text string `xml:",chardata"`
}

type domainBoot struct {
	Dev string `xml:"dev,attr"`
}

type domainFeatures struct {
	ACPI *struct{} `xml:"acpi,omitempty"`
	APIC *struct{} `xml:"apic,omitempty"`
}

type domainDevices struct {
	Disks     []domainDisk    `xml:"disk"`
	Interface domainInterface `xml:"interface"`
	Graphics  domainGraphics  `xml:"graphics"`
	Console   domainConsole   `xml:"console"`
}

type domainDisk struct {
	Type     string       `xml:"type,attr"`
	Device   string       `xml:"device,attr"`
	Driver   domainDriver `xml:"driver"`
	Source   domainSource `xml:"source"`
	Target   domainTarget `xml:"target"`
	Readonly *struct{}    `xml:"readonly,omitempty"`
}

type domainDriver struct {
	Name string `xml:"name,attr"`
	Type string `xml:"type,attr"`
}

type domainSource struct {
	File string `xml:"file,attr"`
}

type domainTarget struct {
	Dev string `xml:"dev,attr"`
	Bus string `xml:"bus,attr"`
}

type domainInterface struct {
	Type   string              `xml:"type,attr"`
	Source domainNetworkSource `xml:"source"`
	Model  domainModel         `xml:"model"`
}

type domainNetworkSource struct {
	Network string `xml:"network,attr"`
}

type domainModel struct {
	Type string `xml:"type,attr"`
}

type domainGraphics struct {
	Type     string `xml:"type,attr"`
	Port     int    `xml:"port,attr"`
	AutoPort string `xml:"autoport,attr"`
	Listen   string `xml:"listen,attr"`
}

type domainConsole struct {
	Type string `xml:"type,attr"`
}
