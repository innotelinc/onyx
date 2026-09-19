// Command onyx is the CLI (docs/design/04#10-cli): a thin mirror of the REST
// API designed for scripting — --json everywhere, non-zero exit codes and
// structured errors. It authenticates with a machine token, never stores
// passwords (onyx login --token — coming with user management).
//
// Skeleton commands (v0.1): version, status, pool list.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/innotelinc/onyx/sdk/go/client"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

const usage = `onyx — Onyx CLI (docs/design/04#10)

Usage:
  onyx <command> [flags]

Commands:
  version     show core + API versions
  status      show aggregate service health
  pool list   list storage pools
  pool show   show one storage pool (<name>)
  pool create format a disk into a pool and mount it (<device> <name>)
  pool remove forget a pool record, releasing its mount (<name>)
  device list   list detected drives (hotplug, USB, SATA)
  device show   show one device (<name>)
  device attach mount a device and expose it as a share (<name>)
  device detach unmount a device (<name>)
  events        list the device audit trail (attach/detach/health/error)
  events --stream  tail live hotplug events as they happen
  share create  create a share
  share list    list shares
  share show    show one share (<name>)
  share delete  delete a share (<name>)
  storage remotes    list configured cloud/remote targets
  storage providers  list the cloud/remote backends setup supports
  storage add        configure a target (<name> <type> key=value ...)
  storage rm         remove a target (<name>)
  storage check      probe a target (<name>)
  storage clone      copy a storage folder to a target (<folder> <remote> [dest])
  app store     list the app catalog (--installed for the installed apps)
  app install   install an app (<app-id>) [--version V] [--set key=value ...]
  app rm        uninstall an app (<app-id>) [--purge-data] [--force]
  app containers  list containers [--app <app-id>]
  app start|stop|restart  act on a container (<container-id>)
  vm list       list virtual machines
  vm create     define a machine (<name>) [--vcpus N] [--memory-mb N] [--disk-mb N] [--iso PATH]
  vm start      boot a machine (<id>)
  vm stop       shut a machine down (<id>) [--no-graceful]
  vm delete     remove a machine (<id>) [--delete-disk]
  bucket list   list object-storage buckets
  bucket create create one (<name>) [--tier local|cloud|tiered] [--cloud-target REMOTE[:PATH]] [--evict-after-days N]
  bucket sync   mirror a cloud/tiered bucket out (<name>) [--evict]
  bucket delete remove one (<name>) [--force]
  help        show this help

Flags:
  --json      machine-readable JSON output
  --api URL   onyx-api endpoint (env ONYX_API, default http://127.0.0.1:8080)
`

func run(args []string) int {
	fs := flag.NewFlagSet("onyx", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	api := fs.String("api", os.Getenv("ONYX_API"), "onyx-api endpoint (env ONYX_API)")

	// Support --json at any position (before or after the subcommand), like a
	// cobra CLI: pull it out before flag.Parse so it never lands in args.
	jsonFlag := false
	positional := args[:0]
	for _, a := range args {
		if a == "--json" || a == "-json" {
			jsonFlag = true
			continue
		}
		positional = append(positional, a)
	}
	args = positional

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	jsonMode := *jsonOut || jsonFlag

	endpoint := *api
	if endpoint == "" {
		endpoint = client.DefaultEndpoint
	}
	c := client.New(endpoint)
	ctx := context.Background()

	var err error
	switch fs.Arg(0) {
	case "version":
		err = cmdVersion(ctx, c, jsonMode)
	case "status":
		err = cmdStatus(ctx, c, jsonMode)
	case "pool":
		err = cmdPool(ctx, c, jsonMode, fs.Args()[1:])
	case "device":
		err = cmdDevice(ctx, c, jsonMode, fs.Args()[1:])
	case "events":
		err = cmdEvents(ctx, c, jsonMode, fs.Args()[1:])
	case "share":
		err = cmdShare(ctx, c, jsonMode, fs.Args()[1:])
	case "storage":
		err = cmdStorage(ctx, c, jsonMode, fs.Args()[1:])
	case "app", "apps":
		err = cmdApp(ctx, c, jsonMode, fs.Args()[1:])
	case "vm", "vms":
		err = cmdVM(ctx, c, jsonMode, fs.Args()[1:])
	case "bucket", "buckets":
		err = cmdBucket(ctx, c, jsonMode, fs.Args()[1:])
	case "help", "-h", "--help":
		fs.Usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "onyx: unknown command %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "onyx: %v\n", err)
		return 1
	}
	return 0
}

func cmdVersion(ctx context.Context, c *client.Client, jsonOut bool) error {
	v, err := c.SystemVersion(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(v)
	}
	fmt.Printf("onyx %s (%s, api %s, commit %s)\n", v.Version, v.Codename, v.APIVersion, v.Commit)
	return nil
}

func cmdStatus(ctx context.Context, c *client.Client, jsonOut bool) error {
	s, err := c.SystemStatus(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(s)
	}
	fmt.Printf("core %s\n", s.CoreVersion)
	for _, svc := range s.Services {
		fmt.Printf("  %-14s %-12s %s\n", svc.Name, svc.Status, svc.Version)
	}
	return nil
}

func cmdPool(ctx context.Context, c *client.Client, jsonOut bool, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: onyx pool list|show|create|remove [--json]")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: onyx pool list [--json]")
		}
		return cmdPoolList(ctx, c, jsonOut)
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("usage: onyx pool show <name> [--json]")
		}
		return cmdPoolShow(ctx, c, jsonOut, args[1])
	case "create":
		return cmdPoolCreate(ctx, c, jsonOut, args[1:])
	case "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: onyx pool remove <name> [--json]")
		}
		return cmdPoolRemove(ctx, c, args[1])
	default:
		return fmt.Errorf("unknown pool command %q (usage: onyx pool list|show|create|remove)", args[0])
	}
}

func cmdPoolList(ctx context.Context, c *client.Client, jsonOut bool) error {
	p, err := c.ListPools(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(p)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tFS\tTOTAL\tUSED")
	for _, pool := range p.Pools {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\n", pool.Name, pool.State, pool.FSType, pool.TotalBytes, pool.UsedBytes)
	}
	return w.Flush()
}

func cmdPoolShow(ctx context.Context, c *client.Client, jsonOut bool, name string) error {
	pool, err := c.GetPool(ctx, name)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(pool)
	}
	fmt.Printf("Name:   %s\n", pool.Name)
	fmt.Printf("UUID:   %s\n", pool.UUID)
	fmt.Printf("FS:     %s\n", pool.FSType)
	fmt.Printf("State:  %s\n", pool.State)
	fmt.Printf("Total:  %d bytes\n", pool.TotalBytes)
	fmt.Printf("Used:   %d bytes\n", pool.UsedBytes)
	return nil
}

// cmdPoolCreate formats a removable whole disk into a pool. The data plane
// force-unmounts whatever is currently on the disk, force-erases it, and
// mounts the fresh filesystem under /mnt/onyx unless --no-mount.
func cmdPoolCreate(ctx context.Context, c *client.Client, jsonOut bool, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: onyx pool create <device> <name> [--fs btrfs|ext4] [--mount-name NAME] [--no-mount] [--no-force] [--json]")
	}
	req := &client.CreatePoolRequest{Device: args[0], Name: args[1]}
	force := true
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--fs":
			if i+1 >= len(args) {
				return fmt.Errorf("--fs requires btrfs or ext4")
			}
			i++
			req.FSType = strings.ToLower(args[i])
			if req.FSType != "btrfs" && req.FSType != "ext4" {
				return fmt.Errorf("--fs must be btrfs or ext4")
			}
		case "--mount-name":
			if i+1 >= len(args) {
				return fmt.Errorf("--mount-name requires a value")
			}
			i++
			req.MountName = args[i]
		case "--no-mount":
			no := false
			req.AutoMount = &no
		case "--force":
			force = true
		case "--no-force":
			force = false
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}
	req.Force = &force
	pool, err := c.CreatePool(ctx, req)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(pool)
	}
	fmt.Printf("created pool %q (%s, %s)\n", pool.Name, pool.FSType, pool.State)
	return nil
}

// cmdPoolRemove forgets a pool record. The mount is released and the pool is
// dropped from the registry; the filesystem on the disk is not erased, so it is
// the safe way to clear a pool whose record no longer matches a device.
func cmdPoolRemove(ctx context.Context, c *client.Client, name string) error {
	if err := c.DeletePool(ctx, name); err != nil {
		return err
	}
	fmt.Printf("removed pool %q (its filesystem is untouched)\n", name)
	return nil
}

func cmdEvents(ctx context.Context, c *client.Client, jsonOut bool, args []string) error {
	limit := 0
	var kname string
	stream := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--limit":
			if i+1 >= len(args) {
				return fmt.Errorf("--limit requires a value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("--limit must be a number")
			}
			limit = n
		case "--kname":
			if i+1 >= len(args) {
				return fmt.Errorf("--kname requires a value")
			}
			i++
			kname = args[i]
		case "--stream":
			stream = true
		default:
			return fmt.Errorf("unknown events flag %q (usage: onyx events [--limit N] [--kname NAME] [--stream] [--json])", args[i])
		}
	}
	if stream {
		return cmdEventsStream(ctx, c, jsonOut)
	}
	evs, err := c.ListEvents(ctx, limit, 0, kname)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(evs)
	}
	if len(evs.Events) == 0 {
		fmt.Println("no events")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTS\tEVENT\tDEVICE\tDETAIL")
	for _, e := range evs.Events {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", e.ID, e.TS, e.Event, displayDevice(e), e.Detail)
	}
	return w.Flush()
}

func cmdEventsStream(ctx context.Context, c *client.Client, jsonOut bool) error {
	ch, err := c.WatchEvents(ctx)
	if err != nil {
		return err
	}
	fmt.Println("watching device events (Ctrl-C to stop)")
	for e := range ch {
		if jsonOut {
			_ = printJSON(e)
			continue
		}
		fmt.Printf("%s  %-7s %-20s %s\n", e.TS, e.Event, displayDevice(e), e.Detail)
	}
	return nil
}

// displayDevice renders a device kname with its friendly name when they
// differ (e.g. "sdz1 (usb-data)").
func displayDevice(e client.DeviceEvent) string {
	if e.Name != "" && e.Name != e.KName {
		return fmt.Sprintf("%s (%s)", e.KName, e.Name)
	}
	return e.KName
}

func cmdDevice(ctx context.Context, c *client.Client, jsonOut bool, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: onyx device list|show|attach|detach [args] [--json]")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: onyx device list [--json]")
		}
		return cmdDeviceList(ctx, c, jsonOut)
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("usage: onyx device show <name> [--json]")
		}
		return cmdDeviceShow(ctx, c, jsonOut, args[1])
	case "attach":
		if len(args) != 2 {
			return fmt.Errorf("usage: onyx device attach <name>")
		}
		return cmdDeviceAttach(ctx, c, args[1])
	case "detach":
		if len(args) != 2 {
			return fmt.Errorf("usage: onyx device detach <name>")
		}
		return cmdDeviceDetach(ctx, c, args[1])
	default:
		return fmt.Errorf("unknown device command %q (usage: onyx device list|show|attach|detach)", args[0])
	}
}

func cmdDeviceList(ctx context.Context, c *client.Client, jsonOut bool) error {
	d, err := c.ListDevices(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(d)
	}
	if len(d.Devices) == 0 {
		fmt.Println("no devices")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKNAME\tSTATE\tFS\tLABEL\tSIZE\tMOUNTPOINT")
	for _, dev := range d.Devices {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			dev.Name, dev.KName, dev.State, dev.FSType, dev.Label, dev.SizeBytes, dev.Mountpoint)
	}
	return w.Flush()
}

func cmdDeviceShow(ctx context.Context, c *client.Client, jsonOut bool, name string) error {
	dev, err := c.GetDevice(ctx, name)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(dev)
	}
	fmt.Printf("Name:       %s\n", dev.Name)
	fmt.Printf("Kernel:     %s (%s)\n", dev.KName, dev.Path)
	fmt.Printf("State:      %s\n", dev.State)
	fmt.Printf("FS:         %s\n", orDash(dev.FSType))
	fmt.Printf("Label:      %s\n", orDash(dev.Label))
	fmt.Printf("UUID:       %s\n", orDash(dev.UUID))
	fmt.Printf("Size:       %d bytes\n", dev.SizeBytes)
	fmt.Printf("Removable:  %v\n", dev.Removable)
	fmt.Printf("Mountpoint: %s\n", orDash(dev.Mountpoint))
	fmt.Printf("Auto:       %s\n", dev.Auto)
	fmt.Printf("Health:     %s\n", orDash(dev.HealthStatus))
	if dev.TemperatureC > 0 {
		fmt.Printf("Temp:       %d C\n", dev.TemperatureC)
	}
	return nil
}

func cmdDeviceAttach(ctx context.Context, c *client.Client, name string) error {
	dev, err := c.MountDevice(ctx, name)
	if err != nil {
		return err
	}
	fmt.Printf("attached %s at %s (share \"%s\" is live)\n", dev.Name, dev.Mountpoint, dev.Name)
	return nil
}

func cmdDeviceDetach(ctx context.Context, c *client.Client, name string) error {
	dev, err := c.UnmountDevice(ctx, name)
	if err != nil {
		return err
	}
	fmt.Printf("detached %s\n", dev.Name)
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdShare(ctx context.Context, c *client.Client, jsonOut bool, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: onyx share create|list|show|delete [args] [--json]")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: onyx share list [--json]")
		}
		return cmdShareList(ctx, c, jsonOut)
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("usage: onyx share show <name> [--json]")
		}
		return cmdShareShow(ctx, c, jsonOut, args[1])
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: onyx share delete <name>")
		}
		return cmdShareDelete(ctx, c, args[1])
	case "create":
		return cmdShareCreate(ctx, c, jsonOut, args[1:])
	default:
		return fmt.Errorf("unknown share command %q (usage: onyx share create|list|show|delete)", args[0])
	}
}

func cmdShareList(ctx context.Context, c *client.Client, jsonOut bool) error {
	s, err := c.ListShares(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(s)
	}
	if len(s.Shares) == 0 {
		fmt.Println("no shares")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPATH\tREADONLY\tPROTOCOLS")
	for _, share := range s.Shares {
		fmt.Fprintf(w, "%s\t%s\t%v\t%s\n", share.Name, share.Path, share.Readonly, friendlyProtocols(share.Protocols))
	}
	return w.Flush()
}

func cmdShareShow(ctx context.Context, c *client.Client, jsonOut bool, name string) error {
	share, err := c.GetShare(ctx, name)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(share)
	}
	fmt.Printf("Name:     %s\n", share.Name)
	fmt.Printf("Path:     %s\n", share.Path)
	fmt.Printf("Comment:  %s\n", share.Comment)
	fmt.Printf("Readonly: %v\n", share.Readonly)
	fmt.Printf("Protocols:%s\n", friendlyProtocols(share.Protocols))
	return nil
}

func cmdShareCreate(ctx context.Context, c *client.Client, jsonOut bool, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: onyx share create <name> <path> [--comment TEXT] [--readonly] [--smb|--nfs|--ftp|--sftp|--webdav|--rsync] [--json]")
	}
	name, path := args[0], args[1]
	req := &client.CreateShareRequest{Name: name, Path: path}
	var protocols []client.ShareProtocol
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--comment":
			if i+1 >= len(args) {
				return fmt.Errorf("--comment requires a value")
			}
			i++
			req.Comment = args[i]
		case "--readonly":
			req.Readonly = true
		case "--smb":
			protocols = append(protocols, client.ProtocolSMB)
		case "--nfs":
			protocols = append(protocols, client.ProtocolNFS)
		case "--ftp":
			protocols = append(protocols, client.ProtocolFTP)
		case "--sftp":
			protocols = append(protocols, client.ProtocolSFTP)
		case "--webdav":
			protocols = append(protocols, client.ProtocolWebDAV)
		case "--rsync":
			protocols = append(protocols, client.ProtocolRsync)
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}
	if len(protocols) == 0 {
		protocols = []client.ShareProtocol{client.ProtocolSMB}
	}
	req.Protocols = protocols

	created, err := c.CreateShare(ctx, req)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(created)
	}
	fmt.Printf("created share %q (%s)\n", created.Name, created.Path)
	return nil
}

func cmdShareDelete(ctx context.Context, c *client.Client, name string) error {
	if err := c.DeleteShare(ctx, name); err != nil {
		return err
	}
	fmt.Printf("deleted share %q\n", name)
	return nil
}

// friendlyProtocols renders proto enum names ("SHARE_PROTOCOL_SMB") as
// lowercase protocol names ("smb, nfs") for human output.
func friendlyProtocols(ps []client.ShareProtocol) string {
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		s := strings.ToLower(strings.TrimPrefix(string(p), "SHARE_PROTOCOL_"))
		names = append(names, s)
	}
	return strings.Join(names, ", ")
}

// cmdStorage covers the cloud/remote storage surface: the remote catalog, the
// backends setup supports, remote configuration, reachability probes, and
// cloning a storage folder out to a remote.
func cmdStorage(ctx context.Context, c *client.Client, jsonOut bool, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: onyx storage remotes|providers|add|rm|check|clone [args] [--json]")
	}
	switch args[0] {
	case "remotes", "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: onyx storage remotes [--json]")
		}
		remotes, err := c.ListRemotes(ctx)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(remotes)
		}
		if len(remotes.Remotes) == 0 {
			fmt.Println("no cloud/remote targets configured")
			return nil
		}
		for _, name := range remotes.Remotes {
			if kind := remotes.Details[name]; kind != "" {
				fmt.Printf("%s\t%s\n", name, kind)
				continue
			}
			fmt.Println(name)
		}
		return nil
	case "providers":
		if len(args) != 1 {
			return fmt.Errorf("usage: onyx storage providers [--json]")
		}
		remotes, err := c.ListRemotes(ctx)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(remotes.Providers)
		}
		if len(remotes.Providers) == 0 {
			fmt.Println("no providers reported")
			return nil
		}
		for _, p := range remotes.Providers {
			kind := "credentials"
			if p.OAuth {
				kind = "oauth (one-time browser approval)"
			}
			fmt.Printf("%-24s %-34s %s\n", p.Type, kind, strings.Join(p.Required, ", "))
		}
		return nil
	case "add", "create":
		if len(args) < 3 {
			return fmt.Errorf("usage: onyx storage add <name> <type> [key=value ...] [--json]")
		}
		req := &client.CreateRemoteRequest{Name: args[1], Type: args[2], Params: map[string]string{}}
		for _, pair := range args[3:] {
			key, value, found := strings.Cut(pair, "=")
			if !found || key == "" {
				return fmt.Errorf("options must be key=value (got %q)", pair)
			}
			req.Params[key] = value
		}
		remote, err := c.CreateRemote(ctx, req)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(remote)
		}
		fmt.Printf("configured %s (%s)\n", remote.Name, remote.Type)
		if remote.NextStep != "" {
			fmt.Println(remote.NextStep)
		}
		return nil
	case "rm", "delete", "remove":
		if len(args) != 2 {
			return fmt.Errorf("usage: onyx storage rm <name>")
		}
		if err := c.DeleteRemote(ctx, args[1]); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", args[1])
		return nil
	case "check":
		if len(args) != 2 {
			return fmt.Errorf("usage: onyx storage check <name> [--json]")
		}
		check, err := c.CheckRemote(ctx, args[1])
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(check)
		}
		if check.OK {
			fmt.Printf("%s is reachable\n", check.Name)
			return nil
		}
		return fmt.Errorf("%s is not reachable: %s", check.Name, check.Detail)
	case "clone":
		if len(args) < 3 {
			return fmt.Errorf("usage: onyx storage clone <folder> <remote> [dest] [--json]")
		}
		req := &client.CloneToRemoteRequest{Source: args[1], Remote: args[2]}
		if len(args) > 3 {
			req.Dest = args[3]
		}
		result, err := c.CloneToRemote(ctx, req)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(result)
		}
		fmt.Printf("cloned %s to %s\n", result.Source, result.Target)
		if result.Detail != "" {
			fmt.Println(result.Detail)
		}
		return nil
	default:
		return fmt.Errorf("unknown storage command %q (usage: onyx storage remotes|providers|add|rm|check|clone)", args[0])
	}
}

// cmdApp covers the app store and container lifecycle (onyx-appd,
// docs/design/09). `app store` is the catalog; `app list` is the installed
// subset of it, which is how an operator asks "what is running here".
func cmdApp(ctx context.Context, c *client.Client, jsonOut bool, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: onyx app store|list|install|rm|containers|start|stop|restart [args] [--json]")
	}
	switch args[0] {
	case "store", "catalog":
		apps, err := c.ListApps(ctx, "")
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(apps)
		}
		return printApps(apps.Apps)
	case "list", "installed":
		apps, err := c.ListApps(ctx, "installed")
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(apps)
		}
		if len(apps.Apps) == 0 {
			fmt.Println("no apps installed")
			return nil
		}
		return printApps(apps.Apps)
	case "install", "add":
		appID, version, config, err := parseInstallArgs(args[1:])
		if err != nil {
			return err
		}
		app, err := c.InstallApp(ctx, &client.InstallAppRequest{AppID: appID, Version: version, Config: config})
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(app)
		}
		fmt.Printf("installed %s %s (%s)\n", app.Name, app.Version, app.Status)
		return nil
	case "rm", "remove", "uninstall":
		if len(args) < 2 {
			return fmt.Errorf("usage: onyx app rm <app-id> [--purge-data] [--force]")
		}
		purge, force := false, false
		for _, a := range args[2:] {
			switch a {
			case "--purge-data", "--purge_data":
				purge = true
			case "--force":
				force = true
			default:
				return fmt.Errorf("unknown app rm flag %q", a)
			}
		}
		if err := c.UninstallApp(ctx, args[1], purge, force); err != nil {
			return err
		}
		fmt.Printf("uninstalled %s\n", args[1])
		return nil
	case "containers", "ps":
		appID := ""
		if len(args) > 1 {
			if args[1] != "--app" || len(args) < 3 {
				return fmt.Errorf("usage: onyx app containers [--app <app-id>] [--json]")
			}
			appID = args[2]
		}
		containers, err := c.ListContainers(ctx, appID)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(containers)
		}
		if len(containers.Containers) == 0 {
			fmt.Println("no containers")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSERVICE\tSTATUS\tIMAGE")
		for _, ct := range containers.Containers {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ct.ID, ct.Service, ct.Status, ct.Image)
		}
		return w.Flush()
	case "start", "stop", "restart":
		if len(args) != 2 {
			return fmt.Errorf("usage: onyx app %s <container-id> [--json]", args[0])
		}
		container, err := c.ContainerAction(ctx, args[1], args[0])
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(container)
		}
		fmt.Printf("%s %s is %s\n", container.Service, container.ID, container.Status)
		return nil
	default:
		return fmt.Errorf("unknown app command %q (usage: onyx app store|list|install|rm|containers|start|stop|restart)", args[0])
	}
}

func printApps(apps []client.App) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tVERSION\tSTATUS\tDESCRIPTION")
	for _, a := range apps {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.Name, a.Version, a.Status, a.Description)
	}
	return w.Flush()
}

// parseInstallArgs reads the app id plus --version and --set key=value options
// for `onyx app install`.
func parseInstallArgs(args []string) (string, string, map[string]string, error) {
	if len(args) == 0 {
		return "", "", nil, fmt.Errorf("usage: onyx app install <app-id> [--version V] [--set key=value ...] [--json]")
	}
	appID := args[0]
	version := ""
	config := map[string]string{}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--version":
			if i+1 >= len(args) {
				return "", "", nil, fmt.Errorf("--version needs a value")
			}
			i++
			version = args[i]
		case "--set":
			if i+1 >= len(args) {
				return "", "", nil, fmt.Errorf("--set needs key=value")
			}
			i++
			key, value, found := strings.Cut(args[i], "=")
			if !found || key == "" {
				return "", "", nil, fmt.Errorf("--set takes key=value (got %q)", args[i])
			}
			config[key] = value
		default:
			return "", "", nil, fmt.Errorf("unknown app install flag %q", args[i])
		}
	}
	return appID, version, config, nil
}

// cmdVM covers the virtualization surface (onyx-vmm, docs/design/11 §6.3).
func cmdVM(ctx context.Context, c *client.Client, jsonOut bool, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: onyx vm list|create|start|stop|delete [args] [--json]")
	}
	switch args[0] {
	case "list", "ls":
		vms, err := c.ListVMs(ctx)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(vms)
		}
		if len(vms.VMs) == 0 {
			fmt.Println("no virtual machines")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSTATUS\tVCPUS\tMEMORY\tDISK")
		for _, v := range vms.VMs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d MiB\t%s\n", v.ID, v.Name, v.Status, v.VCPUs, v.MemoryMB, v.Disk)
		}
		return w.Flush()
	case "create":
		req, err := parseVMCreate(args[1:])
		if err != nil {
			return err
		}
		vm, err := c.CreateVM(ctx, req)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(vm)
		}
		fmt.Printf("created %s (%s), %s — start it with `onyx vm start %s`\n", vm.Name, vm.ID, vm.Status, vm.ID)
		return nil
	case "start":
		if len(args) != 2 {
			return fmt.Errorf("usage: onyx vm start <id> [--json]")
		}
		vm, err := c.StartVM(ctx, args[1])
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(vm)
		}
		fmt.Printf("%s is %s\n", vm.Name, vm.Status)
		return nil
	case "stop":
		if len(args) < 2 {
			return fmt.Errorf("usage: onyx vm stop <id> [--no-graceful] [--json]")
		}
		graceful := true
		for _, a := range args[2:] {
			switch a {
			case "--no-graceful", "--hard":
				graceful = false
			default:
				return fmt.Errorf("unknown vm stop flag %q", a)
			}
		}
		vm, err := c.StopVM(ctx, args[1], graceful)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(vm)
		}
		fmt.Printf("%s is %s\n", vm.Name, vm.Status)
		return nil
	case "delete", "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: onyx vm delete <id> [--delete-disk]")
		}
		deleteDisk := false
		for _, a := range args[2:] {
			switch a {
			case "--delete-disk", "--delete_disk":
				deleteDisk = true
			default:
				return fmt.Errorf("unknown vm delete flag %q", a)
			}
		}
		if err := c.DeleteVM(ctx, args[1], deleteDisk); err != nil {
			return err
		}
		if deleteDisk {
			fmt.Printf("deleted %s and its disk image\n", args[1])
			return nil
		}
		fmt.Printf("deleted %s (disk image kept)\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown vm command %q (usage: onyx vm list|create|start|stop|delete)", args[0])
	}
}

func parseVMCreate(args []string) (*client.CreateVMRequest, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("usage: onyx vm create <name> [--vcpus N] [--memory-mb N] [--disk-mb N] [--os NAME] [--iso PATH]")
	}
	req := &client.CreateVMRequest{Name: args[0], VCPUs: 1, MemoryMB: 2048}
	for i := 1; i < len(args); i++ {
		if i+1 >= len(args) {
			return nil, fmt.Errorf("%s needs a value", args[i])
		}
		value := args[i+1]
		switch args[i] {
		case "--vcpus":
			n, err := strconv.ParseInt(value, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("--vcpus must be a number")
			}
			req.VCPUs = int32(n)
		case "--memory-mb", "--memory":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("--memory-mb must be a number")
			}
			req.MemoryMB = n
		case "--disk-mb", "--disk":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("--disk-mb must be a number")
			}
			req.DiskMB = n
		case "--os":
			req.OS = value
		case "--iso":
			req.ISO = value
		default:
			return nil, fmt.Errorf("unknown vm create flag %q", args[i])
		}
		i++
	}
	return req, nil
}

// cmdBucket covers object storage and hybrid-cloud tiering (onyx-objectstore,
// docs/design/11 §6.6).
func cmdBucket(ctx context.Context, c *client.Client, jsonOut bool, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: onyx bucket list|create|sync|delete [args] [--json]")
	}
	switch args[0] {
	case "list", "ls":
		buckets, err := c.ListBuckets(ctx)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(buckets)
		}
		if len(buckets.Buckets) == 0 {
			fmt.Println("no buckets")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tTIER\tTARGET\tLOCAL\tCLOUD\tLAST SYNC")
		for _, b := range buckets.Buckets {
			target := b.CloudTarget
			if target == "" {
				target = "-"
			}
			last := b.LastSyncAt
			if last == "" {
				last = "never"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\n", b.Name, strings.ToLower(b.Tier), target, b.LocalObjects, b.CloudObjects, last)
			if b.SyncError != "" {
				fmt.Fprintf(w, "\t\tsync error: %s\n", b.SyncError)
			}
		}
		return w.Flush()
	case "create":
		req, err := parseBucketCreate(args[1:])
		if err != nil {
			return err
		}
		bucket, err := c.CreateBucket(ctx, req)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(bucket)
		}
		if bucket.Tier == "LOCAL" || bucket.Tier == "local" {
			fmt.Printf("created bucket %s (local)\n", bucket.Name)
			return nil
		}
		fmt.Printf("created bucket %s (%s → %s); run `onyx bucket sync %s` to mirror it out\n",
			bucket.Name, strings.ToLower(bucket.Tier), bucket.CloudTarget, bucket.Name)
		return nil
	case "sync":
		if len(args) < 2 {
			return fmt.Errorf("usage: onyx bucket sync <name> [--evict] [--json]")
		}
		evict := false
		for _, a := range args[2:] {
			switch a {
			case "--evict":
				evict = true
			default:
				return fmt.Errorf("unknown bucket sync flag %q", a)
			}
		}
		result, err := c.SyncBucket(ctx, args[1], evict)
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(result)
		}
		fmt.Printf("synced %s: %d uploaded, %d evicted\n", result.Bucket.Name, result.Uploaded, result.Evicted)
		for _, warning := range result.Warnings {
			fmt.Printf("warning: %s\n", warning)
		}
		return nil
	case "delete", "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: onyx bucket delete <name> [--force]")
		}
		force := false
		for _, a := range args[2:] {
			switch a {
			case "--force":
				force = true
			default:
				return fmt.Errorf("unknown bucket delete flag %q", a)
			}
		}
		if err := c.DeleteBucket(ctx, args[1], force); err != nil {
			return err
		}
		fmt.Printf("deleted bucket %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown bucket command %q (usage: onyx bucket list|create|sync|delete)", args[0])
	}
}

func parseBucketCreate(args []string) (*client.CreateBucketRequest, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("usage: onyx bucket create <name> [--tier local|cloud|tiered] [--cloud-target REMOTE[:PATH]] [--evict-after-days N]")
	}
	req := &client.CreateBucketRequest{Name: args[0], Tier: "local"}
	for i := 1; i < len(args); i++ {
		if i+1 >= len(args) {
			return nil, fmt.Errorf("%s needs a value", args[i])
		}
		value := args[i+1]
		switch args[i] {
		case "--tier":
			req.Tier = value
		case "--cloud-target", "--target":
			req.CloudTarget = value
		case "--evict-after-days":
			n, err := strconv.ParseInt(value, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("--evict-after-days must be a number")
			}
			req.EvictAfterDays = int32(n)
		default:
			return nil, fmt.Errorf("unknown bucket create flag %q", args[i])
		}
		i++
	}
	if req.Tier != "local" && req.CloudTarget == "" {
		return nil, fmt.Errorf("a %s bucket needs --cloud-target (a configured remote, e.g. b2-archive:)", req.Tier)
	}
	return req, nil
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
