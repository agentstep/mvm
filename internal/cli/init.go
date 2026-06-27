package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/agentstep/mvm/internal/firecracker"
	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/agentstep/mvm/internal/vm"
	"github.com/spf13/cobra"
)

func newInitCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	var (
		cpus      int
		memory    string
		fcVersion string
		force     bool
		minimal   bool
		backend   string
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "One-time setup for running microVMs",
		Long: `One-time setup that installs everything needed to run microVMs.

  mvm init                         # auto-detect backend
  mvm init --backend applevz       # Apple Virtualization.framework (M1+, native, ~0.7s boot, save/restore)
  mvm init --backend firecracker   # Firecracker via Lima (M3+, adds pool + UFFD + named snapshots)
  mvm init --minimal               # slim image without agents`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(limaClient, store, cpus, memory, fcVersion, force, minimal, backend)
		},
	}

	cmd.Flags().IntVar(&cpus, "cpus", 4, "VM CPU count")
	cmd.Flags().StringVar(&memory, "memory", "4GiB", "VM memory")
	cmd.Flags().StringVar(&fcVersion, "fc-version", firecracker.DefaultVersion, "Firecracker version")
	cmd.Flags().BoolVar(&force, "force", false, "re-initialize even if already set up")
	cmd.Flags().BoolVar(&minimal, "minimal", false, "minimal rootfs without AI agents")
	cmd.Flags().StringVar(&backend, "backend", "", "backend: firecracker or applevz (auto-detect if empty)")

	return cmd
}

func runInit(limaClient *lima.Client, store *state.Store, cpus int, memory, fcVersion string, force, minimal bool, backend string) error {
	if !force {
		initialized, _ := store.IsInitialized()
		if initialized {
			fmt.Println("mvm is already initialized. Use --force to re-initialize.")
			return nil
		}
	}

	// Auto-detect backend if not specified
	if backend == "" {
		backend = autoDetectBackend(limaClient)
	}

	fmt.Printf("Initializing mvm (backend: %s)...\n\n", backend)

	switch backend {
	case "firecracker":
		return runInitFirecracker(limaClient, store, cpus, memory, fcVersion, force, minimal)
	case "applevz":
		return runInitAppleVZ(store, cpus, minimal)
	default:
		return fmt.Errorf("unknown backend %q (use 'firecracker' or 'applevz')", backend)
	}
}

func autoDetectBackend(limaClient *lima.Client) string {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return "firecracker" // fallback
	}

	// Check if M3+ (needed for Firecracker nested virt)
	if err := limaClient.CheckHardware(); err == nil {
		return "firecracker" // M3+ — use Firecracker for pause/resume
	}

	// M1/M2 — check if Apple VZ is available
	vzBackend := vm.NewAppleVZBackend(mvmDir)
	if vzBackend.IsAvailable() {
		fmt.Println("  Detected M1/M2 — using Apple Virtualization.framework backend")
		return "applevz"
	}

	return "firecracker" // fallback, will fail at hardware check
}

// runInitFirecracker is the existing Firecracker+Lima init path.
func runInitFirecracker(limaClient *lima.Client, store *state.Store, cpus int, memory, fcVersion string, force, minimal bool) error {
	fmt.Println("Checking prerequisites...")
	if err := limaClient.CheckHardware(); err != nil {
		return err
	}
	fmt.Println("  ✓ Hardware: Apple Silicon M3+ with macOS 15+")

	if !limaClient.IsInstalled() {
		if !limaClient.IsBrewInstalled() {
			return fmt.Errorf("Homebrew is not installed. Install it from https://brew.sh")
		}
		fmt.Println("Installing Lima...")
		if err := limaClient.InstallLima(); err != nil {
			return err
		}
	}
	fmt.Println("  ✓ Lima installed")

	exists, err := limaClient.VMExists()
	if err != nil {
		return err
	}
	if exists && force {
		fmt.Println("Removing existing Lima VM...")
		limaClient.StopVM()
		limaClient.DeleteVM()
		exists = false
	}
	if !exists {
		fmt.Printf("Creating Lima VM (cpus=%d, memory=%s)...\n", cpus, memory)
		if err := limaClient.CreateVM(cpus, memory); err != nil {
			return err
		}
		fmt.Println("Starting Lima VM...")
		if err := limaClient.StartVM(); err != nil {
			return err
		}
	} else {
		if err := limaClient.EnsureRunning(); err != nil {
			return err
		}
	}
	fmt.Println("  ✓ Lima VM running")

	fmt.Printf("Installing Firecracker %s...\n", fcVersion)
	if err := firecracker.Install(limaClient, fcVersion); err != nil {
		return err
	}
	fmt.Println("  ✓ Firecracker installed")

	// Copy guest agent binary to Lima (required)
	agentBin := filepath.Join(filepath.Dir(os.Args[0]), "mvm-agent")
	if _, err := os.Stat(agentBin); err != nil {
		return fmt.Errorf("mvm-agent binary not found at %s. Run: make agent", agentBin)
	}
	fmt.Println("Copying guest agent to Lima...")
	exec.Command("limactl", "copy", agentBin, "mvm:/opt/mvm/mvm-agent").Run()
	limaClient.Shell("sudo chmod +x /opt/mvm/mvm-agent")

	if minimal {
		fmt.Println("Downloading kernel and rootfs (minimal)...")
	} else {
		fmt.Println("Downloading kernel and rootfs (with AI agents)...")
	}
	if err := firecracker.DownloadImages(limaClient, firecracker.CacheDir(), minimal); err != nil {
		return err
	}
	fmt.Println("  ✓ Images cached")

	fmt.Println("Setting up SSH keys...")
	if err := firecracker.GenerateSSHKeys(limaClient, firecracker.KeyDir(), firecracker.CacheDir()); err != nil {
		return err
	}
	hostKeyDir := filepath.Join(mvmDir, "keys")
	os.MkdirAll(hostKeyDir, 0o755)
	if err := firecracker.CopySSHKeyToHost(limaClient, firecracker.KeyDir(), hostKeyDir); err != nil {
		return err
	}
	fmt.Println("  ✓ SSH keys ready")

	fmt.Println("Configuring networking...")
	if _, err := limaClient.ShellScript(state.SetupNATScript()); err != nil {
		return fmt.Errorf("setup NAT: %w", err)
	}
	if _, err := limaClient.ShellScript(state.NATSystemdServiceScript()); err != nil {
		return fmt.Errorf("install NAT service: %w", err)
	}
	fmt.Println("  ✓ NAT configured")

	if _, err := limaClient.Shell(fmt.Sprintf("sudo mkdir -p %s && sudo chmod 755 %s", firecracker.RunDir(), firecracker.RunDir())); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}

	// Deploy daemon inside Lima
	fmt.Println("Installing daemon inside Lima...")
	daemonBin, _ := os.Executable()
	// Cross-compile for Linux ARM64 if we're on macOS
	daemonLinux := filepath.Join(filepath.Dir(daemonBin), "mvm-linux-arm64")
	if runtime.GOOS == "darwin" {
		buildCmd := exec.Command("go", "build", "-ldflags", "-s -w", "-o", daemonLinux, "./cmd/mvm")
		buildCmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
		if out, err := buildCmd.CombinedOutput(); err != nil {
			fmt.Printf("  Warning: daemon build failed: %s\n", out)
		} else {
			exec.Command("limactl", "copy", daemonLinux, "mvm:/opt/mvm/mvm-daemon").Run()
			limaClient.Shell("sudo chmod +x /opt/mvm/mvm-daemon")
			// Install systemd service
			limaClient.Shell(`cat << UNIT | sudo tee /etc/systemd/system/mvm-daemon.service >/dev/null
[Unit]
Description=MVM Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/mvm/mvm-daemon serve start
Restart=always
RestartSec=2
Environment=HOME=$HOME

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable mvm-daemon.service >/dev/null 2>&1
sudo systemctl start mvm-daemon.service`)
			fmt.Println("  ✓ Daemon installed and started")
		}
		os.Remove(daemonLinux)
	}

	if err := store.MarkInitialized(fcVersion, "firecracker"); err != nil {
		return err
	}

	// Warm pool via daemon inside Lima (daemon can reach guests directly via TCP)
	fmt.Println("Warming VM pool...")
	// Set up SSH port forward so we can talk to daemon
	sshFwd := exec.Command("ssh", "-F", filepath.Join(os.Getenv("HOME"), ".lima", "mvm", "ssh.config"),
		"-N", "-L", "19876:/run/mvm/daemon.sock", "lima-mvm", "-f")
	sshFwd.Run()
	time.Sleep(1 * time.Second)

	// Tell daemon to warm pool
	warmCmd := exec.Command("curl", "-sf", "-X", "POST", "http://localhost:19876/pool/warm")
	if out, err := warmCmd.CombinedOutput(); err != nil {
		fmt.Printf("  Warning: pool warm request failed: %v (%s)\n", err, string(out))
	} else {
		fmt.Println("  ✓ Pool warming started (background)")
	}

	fmt.Println("\nReady! Create your first microVM with: mvm start my-vm")
	return nil
}

// runInitAppleVZ initializes the Apple Virtualization.framework backend.
// No Lima, no nested virt. Downloads kernel + rootfs to ~/.mvm/cache/.
func runInitAppleVZ(store *state.Store, cpus int, minimal bool) error {
	fmt.Println("Checking prerequisites...")
	if runtime.GOARCH != "arm64" {
		return fmt.Errorf("Apple VZ backend requires Apple Silicon")
	}
	fmt.Println("  ✓ Apple Silicon detected")

	vzBackend := vm.NewAppleVZBackend(mvmDir)
	if !vzBackend.IsAvailable() {
		return fmt.Errorf("mvm-vz binary not found. Run: make vz")
	}
	fmt.Println("  ✓ mvm-vz available")

	// Create cache directory
	cacheDir := filepath.Join(mvmDir, "cache")
	os.MkdirAll(cacheDir, 0o755)

	// Download kernel — need an aarch64 Linux kernel for VZ
	// Use the same Firecracker CI kernel (it's a standard vmlinux, works with VZ too)
	fmt.Println("Downloading kernel...")
	kernelPath := filepath.Join(cacheDir, "vmlinux")
	if _, err := os.Stat(kernelPath); os.IsNotExist(err) {
		// Download via curl (no Lima needed)
		if err := downloadFile(
			"https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.13/aarch64/vmlinux-6.1.141",
			kernelPath,
		); err != nil {
			return fmt.Errorf("download kernel: %w", err)
		}
	}
	fmt.Println("  ✓ Kernel ready")

	// The rootfs build injects the guest agent + a minimal init that launches
	// it on boot, so the agent must exist before we build the image.
	if err := ensureAgentBinary(cacheDir); err != nil {
		return err
	}
	fmt.Println("  ✓ Guest agent ready")

	// For Apple VZ, we need a rootfs. For now, point users to build one
	// or reuse the Firecracker rootfs if they have Lima available.
	rootfsPath := filepath.Join(cacheDir, "base.ext4")
	if _, err := os.Stat(rootfsPath); os.IsNotExist(err) {
		fmt.Println("Building Debian rootfs...")
		if err := buildLocalRootfs(rootfsPath, minimal); err != nil {
			return fmt.Errorf("build rootfs: %w\nNote: Apple VZ rootfs build requires Docker or a Linux host. Run 'mvm init --backend firecracker' on an M3+ Mac to build the rootfs, then copy ~/.mvm/cache/base.ext4 here.", err)
		}
	}
	fmt.Println("  ✓ Rootfs ready")

	// Generate SSH keys locally (no Lima needed)
	keyDir := filepath.Join(mvmDir, "keys")
	os.MkdirAll(keyDir, 0o755)
	keyPath := filepath.Join(keyDir, "mvm.id_ed25519")
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		fmt.Println("Generating SSH keys...")
		cmd := fmt.Sprintf("ssh-keygen -t ed25519 -f %s -N '' -q", keyPath)
		if err := execLocal(cmd); err != nil {
			return fmt.Errorf("generate SSH keys: %w", err)
		}
	}
	fmt.Println("  ✓ SSH keys ready")

	if err := store.MarkInitialized("", "applevz"); err != nil {
		return err
	}

	fmt.Println("\nReady! Create your first microVM with: mvm start my-vm")
	fmt.Println("Note: Apple VZ backend does not support pause/resume.")
	return nil
}

func downloadFile(url, dest string) error {
	return execLocal(fmt.Sprintf("curl -sL -o %s %s", dest, url))
}

// ensureAgentBinary makes sure a linux/arm64 mvm-agent binary exists in
// cacheDir; the rootfs build injects it as the guest's init-launched agent.
// If missing, it builds it from the agent module located relative to the repo,
// otherwise returns an actionable error.
func ensureAgentBinary(cacheDir string) error {
	agentBin := filepath.Join(cacheDir, "mvm-agent")
	if st, err := os.Stat(agentBin); err == nil && st.Size() > 0 {
		return nil
	}
	src := findAgentSrc()
	if src == "" {
		return fmt.Errorf("mvm-agent not found at %s; build it with: (cd agent && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o %s .)", agentBin, agentBin)
	}
	fmt.Println("Building guest agent (linux/arm64)...")
	cmd := exec.Command("go", "build", "-ldflags", "-s -w", "-o", agentBin, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build mvm-agent: %w\n%s", err, out)
	}
	return nil
}

// findAgentSrc walks up from the working directory looking for the agent
// module (agent/go.mod). Returns "" if not found.
func findAgentSrc() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		cand := filepath.Join(dir, "agent")
		if _, err := os.Stat(filepath.Join(cand, "go.mod")); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func buildLocalRootfs(dest string, minimal bool) error {
	// Try Docker if available
	if _, err := os.Stat("/usr/local/bin/docker"); err == nil {
		return buildRootfsViaDocker(dest, minimal)
	}
	if _, err := os.Stat("/opt/homebrew/bin/docker"); err == nil {
		return buildRootfsViaDocker(dest, minimal)
	}
	return fmt.Errorf("Docker not found — needed to build rootfs without Lima")
}

func buildRootfsViaDocker(dest string, minimal bool) error {
	// Debian package names (the base image is debian:bookworm). The old list
	// used Alpine names (py3-pip), so apt-get aborted under `set -e` and the
	// rootfs shipped without python/node — hence the bare base.
	packages := "openssh-server dropbear git curl wget ca-certificates python3 python3-pip nodejs npm iptables"
	// Image size must hold the populated rootfs. The full set (node/npm/python)
	// is ~640 MB, so size to 1536 MB with headroom; minimal stays small. Cold
	// boot clones this via APFS copy-on-write (`cp -c`), so the larger size
	// doesn't cost boot latency.
	sizeMB := 1536
	if minimal {
		packages = "openrc openssh-server dropbear"
		sizeMB = 256
	}

	script := fmt.Sprintf(`
docker run --rm --platform linux/arm64 -v "$(dirname %s):/output" debian:bookworm bash -c '
set -e
apt-get update -qq && apt-get install -y --no-install-recommends %s e2fsprogs >/dev/null
mkdir -p /rootfs
cp -a /bin /etc /home /lib /root /run /sbin /srv /tmp /usr /var /rootfs/
mkdir -p /rootfs/dev /rootfs/proc /rootfs/sys /rootfs/mnt
echo "mvm" > /rootfs/etc/hostname
echo "root:root" | chpasswd -R /rootfs
mkdir -p /rootfs/root/.ssh
chmod 755 /rootfs/root/.ssh
echo "nameserver 8.8.8.8" > /rootfs/etc/resolv.conf
mkdir -p /rootfs/opt /rootfs/sbin
cp /output/mvm-agent /rootfs/opt/mvm-agent
chmod +x /rootfs/opt/mvm-agent
cat > /rootfs/sbin/mvm-init << MVMINIT
#!/bin/sh
mount -t proc proc /proc 2>/dev/null
mount -t sysfs sysfs /sys 2>/dev/null
mount -t devtmpfs devtmpfs /dev 2>/dev/null
# devpts + /dev/ptmx are required for PTY allocation (interactive exec -it).
mkdir -p /dev/pts
mount -t devpts -o mode=620,ptmxmode=666 devpts /dev/pts 2>/dev/null
ln -sf /dev/pts/ptmx /dev/ptmx
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
exec /opt/mvm-agent
MVMINIT
chmod +x /rootfs/sbin/mvm-init
dd if=/dev/zero of=/output/base.ext4 bs=1M count=0 seek=%d
mkfs.ext4 -F -d /rootfs /output/base.ext4
'`, dest, packages, sizeMB)

	return execLocal(script)
}

func execLocal(cmd string) error {
	c := exec.Command("bash", "-c", cmd)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
