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

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}