package cli

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newDoctorCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check system health and diagnose issues",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(limaClient, store)
		},
	}
}

func runDoctor(limaClient *lima.Client, store *state.Store) error {
	fmt.Println("mvm doctor — system diagnostics")
	fmt.Println()
	issues := 0

	// Backend
	backend := store.GetBackend()
	fmt.Printf("  Backend: %s\n", backend)

	// 1. macOS version
	out, _ := exec.Command("sw_vers", "-productVersion").Output()
	ver := strings.TrimSpace(string(out))
	if ver >= "15" {
		fmt.Printf("  ✓ macOS %s (15+ required)\n", ver)
	} else {
		fmt.Printf("  ✗ macOS %s — requires 15 (Sequoia) or newer\n", ver)
		issues++
	}

	// 2. Chip
	out, _ = exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
	chip := strings.TrimSpace(string(out))
	if err := limaClient.CheckHardware(); err == nil {
		fmt.Printf("  ✓ %s (M3+ required)\n", chip)
	} else {
		fmt.Printf("  ✗ %s — %v\n", chip, err)
		issues++
	}

	// 3. Architecture
	fmt.Printf("  ✓ Architecture: %s/%s\n", runtime.GOOS, runtime.GOARCH)

	// 4. Homebrew
	if _, err := exec.LookPath("brew"); err == nil {
		fmt.Println("  ✓ Homebrew installed")
	} else {
		fmt.Println("  ✗ Homebrew not found — install from https://brew.sh")
		issues++
	}

	// 5. Go
	if goPath, err := exec.LookPath("go"); err == nil {
		out, _ = exec.Command(goPath, "version").Output()
		fmt.Printf("  ✓ %s\n", strings.TrimSpace(string(out)))
	} else {
		fmt.Println("  ✗ Go not found — run: brew install go")
		issues++
	}

	// 6. Lima
	if limaClient.IsInstalled() {
		out, _ = exec.Command("limactl", "--version").Output()
		fmt.Printf("  ✓ Lima: %s\n", strings.TrimSpace(string(out)))
	} else {
		fmt.Println("  ✗ Lima not installed — run: brew install lima")
		issues++
	}

	// 7. Lima VM
	exists, _ := limaClient.VMExists()
	if exists {
		status, _ := limaClient.VMStatus()
		if status == "Running" {
			fmt.Println("  ✓ Lima VM 'mvm': Running")
		} else {
			fmt.Printf("  ! Lima VM 'mvm': %s (run: mvm init)\n", status)
		}
	} else {
		fmt.Println("  ✗ Lima VM 'mvm' not found — run: mvm init")
		issues++
	}

	// 8. Firecracker (inside Lima)
	if exists {
		status, _ := limaClient.VMStatus()
		if status == "Running" {
			fcOut, err := limaClient.Shell("firecracker --version 2>&1 | head -1")
			if err == nil && strings.Contains(fcOut, "Firecracker") {
				fmt.Printf("  ✓ Firecracker: %s\n", strings.TrimSpace(fcOut))
			} else {
				fmt.Println("  ✗ Firecracker not installed inside Lima — run: mvm init")
				issues++
			}
		}
	}

	// 9. mvm state
	initialized, _ := store.IsInitialized()
	if initialized {
		fmt.Println("  ✓ mvm initialized")
	} else {
		fmt.Println("  ✗ mvm not initialized — run: mvm init")
		issues++
	}

	// 10. Warm pool
	if exists {
		status, _ := limaClient.VMStatus()
		if status == "Running" {
			if firecracker.IsPoolReady(limaClient) {
				fmt.Println("  ✓ Warm pool: ready")
			} else {
				fmt.Println("  ! Warm pool: not ready (run: mvm pool warm)")
			}
		}
	}

	// 11. VMs
	vms, _ := store.ListVMs()
	running := 0
	paused := 0
	stopped := 0
	for _, vm := range vms {
		switch vm.Status {
		case "running":
			running++
		case "paused":
			paused++
		default:
			stopped++
		}
	}
	fmt.Printf("  ✓ VMs: %d running, %d paused, %d stopped\n", running, paused, stopped)

	// 12. Disk usage (inside Lima)
	if exists {
		status, _ := limaClient.VMStatus()
		if status == "Running" {
			dfOut, err := limaClient.Shell("df -h /opt/mvm 2>/dev/null | tail -1 | awk '{print $4 \" available of \" $2}'")
			if err == nil && dfOut != "" {
				fmt.Printf("  ✓ Lima disk: %s\n", strings.TrimSpace(dfOut))
			}

			duOut, err := limaClient.Shell("du -sh /opt/mvm 2>/dev/null | awk '{print $1}'")
			if err == nil && duOut != "" {
				fmt.Printf("  ✓ mvm data: %s\n", strings.TrimSpace(duOut))
			}
		}
	}

	fmt.Println()
	if issues == 0 {
		fmt.Println("All checks passed.")
	} else {
		fmt.Printf("%d issue(s) found. Fix them and run 'mvm doctor' again.\n", issues)
	}
	return nil
}
