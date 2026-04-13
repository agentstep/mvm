# Testing

## Unit tests

Run all unit tests (no special hardware required):

```bash
make test
```

This runs `go test ./internal/... -v -race`. Currently 42+ tests covering state management, network allocation, config generation, hardware detection, port parsing, protocol encoding, and concurrent reservation.

The agent protocol tests are in a separate module:

```bash
cd agent && go test ./... -v -race
```

## Integration tests

Integration tests require an Apple Silicon Mac (M3+ for Firecracker backend) with macOS 15+.

### Prerequisites

1. Run `mvm init` to set up the environment
2. Verify with `mvm doctor` — all checks should pass

### Run

```bash
./scripts/integration-test.sh
```

This tests the full VM lifecycle: cold start, warm pool, exec, dev tools, skills files, pause/resume, logs, list, stop, delete, port forwarding, and network policy.

**Important:** The integration test creates VMs with `test-` and `bench-` prefixes. It will not touch your existing VMs.

### What the integration test covers

| Test group | What it tests |
|------------|---------------|
| Cold start | `mvm start -d` boots a VM |
| Warm pool | `mvm pool warm` + `mvm pool status` |
| Warm start | `mvm start` claims from pool |
| Exec | `uname`, hostname, workdir flag |
| Dev tools | node, python3, git, claude, opencode binaries |
| Skills files | SKILLS.md and CLAUDE.md exist |
| Pause/resume | Pause, verify status, resume, exec after resume |
| Logs | Boot log and guest system log |
| List | Default, JSON, and quiet output |
| Stop | Graceful stop |
| Delete | Remove VM and verify |
| Port forwarding | Start with `-p` flag (syntax check) |
| Network policy | Start with `--net-policy deny` (syntax check) |
| Version | `mvm version` output |

## CI

GitHub Actions runs unit tests on every push and PR (`.github/workflows/ci.yml`). Integration tests cannot run in CI because they require Lima, Firecracker, and nested virtualization (M3+ hardware).
