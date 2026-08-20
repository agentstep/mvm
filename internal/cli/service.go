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
	)
	return cmd
}

// serviceAgent dials the guest agent for a running VM.
//
// applevz only for now: there is no daemon on that backend, so the CLI talks to
// the agent directly. The Firecracker path needs these verbs plumbed through
// the daemon like every other command.
func serviceAgent(store *state.Store, name string) (*agentclient.Client, error) {
	v, err := store.GetVM(name)
	if err != nil {
		return nil, err
	}
	if v.Status != "running" {
		return nil, fmt.Errorf("VM %q is %s (services need a running VM)", name, v.Status)
	}
	if v.Backend != "applevz" {
		return nil, fmt.Errorf("mvm service currently supports the applevz backend only")
	}
	return vm.NewAppleVZBackend(mvmDir).AgentClient(name), nil
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

			agent, err := serviceAgent(store, vmName)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := agent.ServiceAdd(ctx, svcName, run, workdir, restart, envMap); err != nil {
				return err
			}
			if err := persistService(store, vmName, state.Service{
				Name: svcName, Run: run, WorkDir: workdir, Env: envMap, Restart: restart,
			}, false); err != nil {
				return fmt.Errorf("service started but could not be persisted: %w", err)
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
			agent, err := serviceAgent(store, args[0])
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := agent.ServiceRemove(ctx, args[1]); err != nil {
				return err
			}
			if err := persistService(store, args[0], state.Service{Name: args[1]}, true); err != nil {
				return fmt.Errorf("service stopped but declaration remains: %w", err)
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
			agent, err := serviceAgent(store, args[0])
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := agent.ServiceRestart(ctx, args[1]); err != nil {
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
			agent, err := serviceAgent(store, args[0])
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			svcs, err := agent.ServiceList(ctx)
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
