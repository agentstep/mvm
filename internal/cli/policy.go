package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

// Policy surfaces what constrains a sandbox, in one place.
//
// The three axes already existed but were scattered across unrelated flags
// (--net-policy, --seccomp, --cpus/--memory) with no way to ask "what is
// actually enforced on this VM". Splitting them out follows the same shape Fly
// use for Sprites (network / privileges / resources), and gives later policy
// somewhere obvious to live.
//
// Read-only deliberately. Every field here is set at start and changing one
// mid-flight means re-applying host firewall rules, re-entering the guest, or
// resizing a live VM — each with different failure modes. Reporting honestly is
// the useful half and carries no risk of half-applied state.
type Policy struct {
	Network    NetworkPolicy    `json:"network"`
	Privileges PrivilegesPolicy `json:"privileges"`
	Resources  ResourcesPolicy  `json:"resources"`
}

// NetworkPolicy is what the VM may reach.
type NetworkPolicy struct {
	Mode    string   `json:"mode"`              // open | deny | allow
	Domains []string `json:"domains,omitempty"` // allow mode only
	// EnforcedAt is "host" when the filter is outside the guest and therefore
	// a boundary, "guest" when it is inside and therefore only a guardrail —
	// a root process in the sandbox can remove it.
	EnforcedAt string `json:"enforced_at"`
}

// PrivilegesPolicy is what the workload inside may do.
type PrivilegesPolicy struct {
	Seccomp string `json:"seccomp,omitempty"` // profile name, empty = none
	// Isolation names the boundary the sandbox actually has.
	Isolation string `json:"isolation"`
	// Root records that the workload runs as root. It always does — that is the
	// product — and stating it stops anyone inferring otherwise.
	Root bool `json:"root"`
}

// ResourcesPolicy is what the VM is allowed to consume.
type ResourcesPolicy struct {
	Cpus     int `json:"cpus,omitempty"`
	MemoryMB int `json:"memory_mb,omitempty"`
}

// PolicyFor assembles the policy view for a VM.
func PolicyFor(v *state.VM) Policy {
	parsed, err := state.ParseNetPolicy(v.NetPolicy)
	mode := "open"
	var domains []string
	if err == nil {
		mode = parsed.Mode.String()
		domains = parsed.Domains
	}

	// Where the network filter lives is the single most important thing on this
	// screen, and it differs by backend: Firecracker filters on the host TAP
	// device, applevz still runs iptables inside the guest.
	enforcedAt := "guest"
	if v.Backend == "firecracker" {
		enforcedAt = "host"
	}

	p := Policy{
		Network: NetworkPolicy{Mode: mode, Domains: domains, EnforcedAt: enforcedAt},
		Privileges: PrivilegesPolicy{
			Isolation: "hardware virtualization (microVM)",
			Root:      true,
		},
	}
	if v.Spec != nil {
		p.Privileges.Seccomp = v.Spec.Seccomp
		p.Resources = ResourcesPolicy{Cpus: v.Spec.Cpus, MemoryMB: v.Spec.MemoryMB}
	}
	if p.Resources.Cpus == 0 {
		p.Resources.Cpus = v.Cpus
	}
	if p.Resources.MemoryMB == 0 {
		p.Resources.MemoryMB = v.MemoryMB
	}
	return p
}

// orDefault renders a resource value, distinguishing "unset" from a real zero.
func orDefault(v int, unit string) string {
	if v == 0 {
		return "(backend default)"
	}
	return fmt.Sprintf("%d%s", v, unit)
}

func newPolicyCmd(store *state.Store) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "policy <name>",
		Short: "Show what constrains a microVM",
		Long: `Report the three policies applied to a sandbox: network, privileges
and resources.

Read-only. Each of these is set at start; changing one mid-flight means
re-applying host firewall rules, re-entering the guest, or resizing a live VM,
each with its own failure modes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := store.GetVM(args[0])
			if err != nil {
				return err
			}
			p := PolicyFor(v)

			wantJSON, err := resolveFormat(format, false)
			if err != nil {
				return err
			}
			if wantJSON {
				data, err := json.MarshalIndent(p, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "NETWORK\t%s\n", p.Network.Mode)
			if len(p.Network.Domains) > 0 {
				for _, d := range p.Network.Domains {
					fmt.Fprintf(w, "  allow\t%s\n", d)
				}
			}
			fmt.Fprintf(w, "  enforced at\t%s\n", p.Network.EnforcedAt)
			if p.Network.EnforcedAt == "guest" {
				fmt.Fprintf(w, "\t(a root process in the sandbox can remove this)\n")
			}
			fmt.Fprintf(w, "PRIVILEGES\t\n")
			fmt.Fprintf(w, "  isolation\t%s\n", p.Privileges.Isolation)
			fmt.Fprintf(w, "  runs as root\t%v\n", p.Privileges.Root)
			if p.Privileges.Seccomp != "" {
				fmt.Fprintf(w, "  seccomp\t%s\n", p.Privileges.Seccomp)
			}
			fmt.Fprintf(w, "RESOURCES\t\n")
			// 0 means "not specified at start", not "no CPUs" — printing the
			// raw zero reads as a broken VM.
			fmt.Fprintf(w, "  cpus\t%s\n", orDefault(p.Resources.Cpus, ""))
			fmt.Fprintf(w, "  memory\t%s\n", orDefault(p.Resources.MemoryMB, " MiB"))
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: json|table")
	return cmd
}
