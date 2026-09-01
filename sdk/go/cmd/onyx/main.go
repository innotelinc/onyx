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

	"onyx.dev/onyx/sdk/go/client"
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
	fmt.Printf("onyx %s (api %s)\n", v.Version, v.APIVersion)
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
		return fmt.Errorf("usage: onyx pool list|show [--json]")
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
	default:
		return fmt.Errorf("unknown pool command %q (usage: onyx pool list|show)", args[0])
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
		return fmt.Errorf("usage: onyx share create <name> <path> [--comment TEXT] [--readonly] [--smb|--nfs] [--json]")
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

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}