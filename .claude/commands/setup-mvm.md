---
name: setup-mvm
description: Interactive mvm setup — checks hardware, installs deps, builds and runs first VM
allowed-tools: Bash Read
---

You are guiding the user through a complete mvm setup. No docs needed — you are the installer. Run each step, check the output, troubleshoot errors, and don't move on until each step succeeds.

## Step 1: Check hardware

Run these and check the output:
- `sw_vers -productVersion` — must be 15+ (macOS Sequoia). If older, stop and explain.
- `sysctl -n machdep.cpu.brand_string` — must be Apple M3 or newer. If M1/M2, explain that mvm requires M3+ for nested virtualization and suggest Vibe (github.com/lynaghk/vibe) or Shuru (github.com/superhq-ai/shuru) as M1/M2 alternatives.

## Step 2: Check dependencies

- `which brew` — if missing, tell them to install from https://brew.sh and wait.
- `which go` — if missing, run `brew install go`.

## Step 3: Build mvm

```
make build
```

If it fails, check `go version` (needs 1.26+, per go.mod). If older, run `brew upgrade go`.

## Step 4: Run mvm init

```
./bin/mvm init
```

This takes ~5 minutes. It installs Lima, creates a VM with nested virtualization, installs Firecracker, downloads the kernel and Alpine rootfs, installs AI agents (Claude Code, Gemini, Codex, OpenCode), generates SSH keys, configures networking, and warms the VM pool.

If it fails:
- **Lima start timeout**: The VM may take 10+ minutes on first boot. Re-run `./bin/mvm init`.
- **apt lock**: Wait 30 seconds and retry.
- **Permission denied**: Usually a file ownership issue inside Lima. The script handles sudo automatically.

## Step 5: Test it

Start a VM:
```
./bin/mvm start test-vm
```

Once it's running, verify:
```
./bin/mvm exec test-vm -- uname -a
./bin/mvm exec test-vm -- node --version
./bin/mvm exec test-vm -- python3 --version
```

Test pause/resume:
```
./bin/mvm pause test-vm
./bin/mvm resume test-vm
./bin/mvm exec test-vm -- echo "still alive"
```

Clean up:
```
./bin/mvm delete test-vm --force
```

## Step 6: Install globally (optional)

Ask the user if they want mvm on their PATH:
```
make install
```

## Step 7: Done

Tell them:
- `mvm pool warm` — pre-boot VMs for 0.35s instant starts
- `mvm start <name>` — create a VM
- `mvm ssh <name>` — shell with Claude Code, Gemini, Codex, OpenCode pre-installed
- `mvm start <name> --net-policy deny` — sandboxed agent environment
- `mvm pause` / `mvm resume` — instant checkpoints (0.15s)

## Troubleshooting

If something goes wrong at any point:
- Check Lima: `limactl list` — the `mvm` VM should show Running
- Check logs: `./bin/mvm logs <name> --boot`
- Re-initialize: `./bin/mvm init --force`
- Nuclear: `limactl delete mvm --force` then `./bin/mvm init`
