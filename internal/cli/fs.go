package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/agentstep/mvm/internal/agentclient"
	"github.com/agentstep/mvm/internal/state"
	"github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

// fsAgent dials the guest agent for file operations.
//
// applevz only for now, like the other agent-direct verbs. Firecracker file
// access still works through `mvm exec cat`, just without the benefits below.
func fsAgent(store *state.Store, name string) (*agentclient.Client, error) {
	v, err := store.GetVM(name)
	if err != nil {
		return nil, err
	}
	if v.Status != "running" {
		return nil, fmt.Errorf("VM %q is %s (file access needs a running VM)", name, v.Status)
	}
	if v.Backend != "applevz" {
		return nil, fmt.Errorf("mvm cp/dir currently support the applevz backend only")
	}
	return vm.NewAppleVZBackend(mvmDir).AgentClient(name), nil
}

// splitVMPath parses "vm:/path" into its parts. ok is false for a local path.
//
// The colon must be followed by an absolute path: a Windows-style "C:\..." or a
// stray colon in a filename should read as local, not as a VM reference.
func splitVMPath(s string) (vmName, path string, ok bool) {
	name, p, found := strings.Cut(s, ":")
	if !found || name == "" || !strings.HasPrefix(p, "/") {
		return "", "", false
	}
	return name, p, true
}

func newCpCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "cp <src> <dst>",
		Short: "Copy a file into or out of a microVM",
		Long: `Copy a single file between the host and a sandbox.

Either side may be a VM path, written as ` + "`vm:/absolute/path`" + `.

  mvm cp ./config.json web:/etc/app/config.json
  mvm cp web:/var/log/app.log ./app.log

This goes straight to the guest agent rather than through a shell, so it is
binary-safe and needs no quoting — unlike piping through ` + "`mvm exec cat`" + `, which
mangles anything that is not text.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCp(store, args[0], args[1])
		},
	}
}

func runCp(store *state.Store, src, dst string) error {
	srcVM, srcPath, srcRemote := splitVMPath(src)
	dstVM, dstPath, dstRemote := splitVMPath(dst)

	switch {
	case srcRemote && dstRemote:
		return fmt.Errorf("copying between two VMs is not supported; copy via the host")
	case !srcRemote && !dstRemote:
		return fmt.Errorf("neither path refers to a VM (use vm:/absolute/path for one side)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if srcRemote {
		agent, err := fsAgent(store, srcVM)
		if err != nil {
			return err
		}
		data, err := agent.ReadFile(ctx, srcPath)
		if err != nil {
			return fmt.Errorf("read %s:%s: %w", srcVM, srcPath, err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("  Copied %s:%s -> %s (%d bytes)\n", srcVM, srcPath, dst, len(data))
		return nil
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	agent, err := fsAgent(store, dstVM)
	if err != nil {
		return err
	}
	mode := uint32(0o644)
	if fi, err := os.Stat(src); err == nil {
		mode = uint32(fi.Mode().Perm())
	}
	if err := agent.WriteFile(ctx, dstPath, data, mode); err != nil {
		return fmt.Errorf("write %s:%s: %w", dstVM, dstPath, err)
	}
	fmt.Printf("  Copied %s -> %s:%s (%d bytes)\n", src, dstVM, dstPath, len(data))
	return nil
}

// newDirCmd is `mvm dir`, not `mvm ls`: `ls` is already an alias for `list`
// (the VM listing), and cobra resolves the alias first, so registering `ls`
// here silently shadowed nothing and this command was unreachable.
func newDirCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "dir <vm:/path>",
		Short: "List a directory inside a microVM",
		Example: "  mvm dir web:/etc\n" +
			"  mvm dir web:/workspace",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vmName, path, ok := splitVMPath(args[0])
			if !ok {
				return fmt.Errorf("expected vm:/absolute/path, got %q", args[0])
			}
			agent, err := fsAgent(store, vmName)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			entries, err := agent.ListDir(ctx, path)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				fmt.Printf("%s is empty.\n", path)
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "MODE\tSIZE\tNAME")
			for _, e := range entries {
				name := e.Name
				if e.IsDir {
					name += "/"
				}
				fmt.Fprintf(w, "%s\t%d\t%s\n", e.Mode, e.Size, name)
			}
			return w.Flush()
		},
	}
}
