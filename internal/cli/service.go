package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/agentstep/mvm/internal/agentclient"
	"github.com/agentstep/mvm/internal/server"
	"github.com/agentstep/mvm/internal/state"
	"github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

func newServiceCmd(store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage long-running processes inside a microVM",
		Long: `Declare processes mvm keeps alive inside a sandbox.

Services are supervised from outside the namespace they run in, so they survive
a ` + "`mvm bounce`" + ` and are restarted if they crash. The declaration is persisted with
the VM, so it is also replayed after stop/start.`,
	}
	cmd.AddCommand(
		newServiceAddCmd(store),
		newServiceRmCmd(store),
		newServiceLsCmd(store),
		newServiceRestartCmd(store),
		newServiceLogsCmd(store),
	)
	return cmd
}

// serviceTarget resolves how to reach a VM's services.
//
// The two backends differ in where the supervisor is reachable from, not in
// what it does. applevz has no daemon, so the CLI dials the guest agent
// directly; Firecracker goes through the daemon like every other verb, which is
// also what makes it work against a remote daemon over MVM_REMOTE.
type serviceTarget struct {
	agent  *agentclient.Client // applevz
	daemon *server.Client      // firecracker
	vmName string
}

func resolveServiceTarget(store *state.Store, name string) (*serviceTarget, error) {
	v, err := store.GetVM(name)
	if err == nil && v.Backend == "applevz" {
		if v.Status != "running" {
			return nil, fmt.Errorf("VM %q is %s (services need a running VM)", name, v.Status)
		}
		return &serviceTarget{agent: vm.NewAppleVZBackend(mvmDir).AgentClient(name), vmName: name}, nil
	}
	// Not in the local store, or a Firecracker VM: the daemon owns it.
	sc, derr := requireDaemon()
	if derr != nil {
		if err != nil {
			return nil, err // no local VM and no daemon — report the lookup failure
		}
		return nil, derr
	}
	return &serviceTarget{daemon: sc, vmName: name}, nil
}

// persistService records the declaration on the VM so it survives the guest.
// The agent's registry is live state that dies with the VM; this is the
// statement of what should be running, and it is what reconcile replays.
func persistService(store *state.Store, vmName string, svc state.Service, remove bool) error {
	return store.UpdateVM(vmName, func(v *state.VM) {
		if v.Spec == nil {
			v.Spec = &state.VMSpec{}
		}
		out := v.Spec.Services[:0:0]
		for _, existing := range v.Spec.Services {
			if existing.Name != svc.Name {
				out = append(out, existing)
			}
		}
		if !remove {
			out = append(out, svc)
		}
		v.Spec.Services = out
	})
}

func newServiceAddCmd(store *state.Store) *cobra.Command {
	var workdir, restart string
	var env []string
	cmd := &cobra.Command{
		Use:   "add <vm> <name> <command>",
		Short: "Declare a service and start supervising it",
		Example: "  mvm service add web api 'uvicorn app:app --port 8000'\n" +
			"  mvm service add web dev 'npm run dev' --restart on-failure",
		Args: cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			vmName, svcName := args[0], args[1]
			run := strings.Join(args[2:], " ")

			if !state.ValidServiceName(svcName) {
				return fmt.Errorf("invalid service name %q (use letters, digits, - _ .)", svcName)
			}
			switch restart {
			case "", state.RestartAlways, state.RestartOnFailure, state.RestartNever:
			default:
				return fmt.Errorf("invalid --restart %q (want always, on-failure or never)", restart)
			}

			envMap := map[string]string{}
			for _, kv := range env {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("invalid --env %q (want KEY=VALUE)", kv)
				}
				envMap[k] = v
			}

			t, err := resolveServiceTarget(store, vmName)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if t.agent != nil {
				if err := t.agent.ServiceAdd(ctx, svcName, run, workdir, restart, envMap); err != nil {
					return err
				}
				// The daemon persists the declaration itself; on applevz the
				// CLI owns the store, so it does it here.
				if err := persistService(store, vmName, state.Service{
					Name: svcName, Run: run, WorkDir: workdir, Env: envMap, Restart: restart,
				}, false); err != nil {
					return fmt.Errorf("service started but could not be persisted: %w", err)
				}
			} else if err := t.daemon.ServiceAdd(ctx, vmName, server.ServiceRequest{
				Name: svcName, Run: run, WorkDir: workdir, Env: envMap, Restart: restart,
			}); err != nil {
				return err
			}
			fmt.Printf("  Service '%s' started\n", svcName)
			return nil
		},
	}
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "", "working directory inside the VM")
	cmd.Flags().StringVar(&restart, "restart", "always", "restart policy: always, on-failure, never")
	cmd.Flags().StringArrayVarP(&env, "env", "e", nil, "set environment variables (KEY=VALUE)")
	return cmd
}

func newServiceRmCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <vm> <name>",
		Short:   "Stop a service and remove its declaration",
		Aliases: []string{"remove"},
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := resolveServiceTarget(store, args[0])
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if t.agent != nil {
				if err := t.agent.ServiceRemove(ctx, args[1]); err != nil {
					return err
				}
				if err := persistService(store, args[0], state.Service{Name: args[1]}, true); err != nil {
					return fmt.Errorf("service stopped but declaration remains: %w", err)
				}
			} else if err := t.daemon.ServiceRemove(ctx, args[0], args[1]); err != nil {
				return err
			}
			fmt.Printf("  Service '%s' removed\n", args[1])
			return nil
		},
	}
}

func newServiceRestartCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "restart <vm> <name>",
		Short: "Restart a service now",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := resolveServiceTarget(store, args[0])
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if t.agent != nil {
				err = t.agent.ServiceRestart(ctx, args[1])
			} else {
				err = t.daemon.ServiceRestart(ctx, args[0], args[1])
			}
			if err != nil {
				return err
			}
			fmt.Printf("  Service '%s' restarted\n", args[1])
			return nil
		},
	}
}

func newServiceLsCmd(store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:     "ls <vm>",
		Short:   "List services in a microVM",
		Aliases: []string{"list"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := resolveServiceTarget(store, args[0])
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var svcs []agentclient.ServiceState
			if t.agent != nil {
				svcs, err = t.agent.ServiceList(ctx)
			} else {
				svcs, err = t.daemon.ServiceList(ctx, args[0])
			}
			if err != nil {
				return err
			}
			if len(svcs) == 0 {
				fmt.Println("No services. Declare one with: mvm service add <vm> <name> <command>")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATE\tRESTARTS\tLAST EXIT\tCOMMAND")
			for _, s := range svcs {
				stateStr := "stopped"
				if s.Running {
					stateStr = "running"
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\n", s.Name, stateStr, s.Restarts, s.LastExit, s.Run)
			}
			return w.Flush()
		},
	}
}

func newServiceLogsCmd(store *state.Store) *cobra.Command {
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <vm> <name>",
		Short: "Show recent output from a service",
		Long: `Print what a service has written to stdout and stderr.

Output is retained outside the container, so it survives a restart of the
service and a ` + "`mvm bounce`" + ` — the output explaining why something died is still
there afterwards. Retention is capped, so only recent output is kept.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := resolveServiceTarget(store, args[0])
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var lines []agentclient.LogLine
			if t.agent != nil {
				lines, err = t.agent.ServiceLogs(ctx, args[1], tail)
			} else {
				lines, err = t.daemon.ServiceLogs(ctx, args[0], args[1], tail)
			}
			if err != nil {
				return err
			}
			if len(lines) == 0 {
				fmt.Printf("No output retained for '%s'.\n", args[1])
				return nil
			}
			for _, l := range lines {
				marker := " "
				if l.Stream == "stderr" {
					marker = "!"
				}
				fmt.Printf("%s %s %s\n", l.At.Format("15:04:05"), marker, l.Text)
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&tail, "tail", "n", 100, "number of lines to show (0 for all retained)")
	return cmd
}
