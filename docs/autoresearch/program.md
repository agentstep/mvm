# mvm autoresearch — Round 1: minimize composite score

## Goal

Minimize the `score` metric printed by `scripts/autoresearch-bench.sh`.

`score = tti_ms + exec_warm_ms*20 + snap_create_ms/10 + snap_restore_ms/10`

Weights are chosen to prevent single-axis wins at the expense of others:
- Heavy weight on exec_warm_ms because it's the most-called operation
- TTI and snapshot ops are rarer but still matter

Baseline (GCP n2-standard-4, as of 2026-04-16):
- tti_ms ≈ 1700
- exec_warm_ms ≈ 16
- snap_create_ms ≈ 19400
- snap_restore_ms ≈ 10000
- **baseline score ≈ 5080**

Target for this round: **score ≤ 3000** (roughly 40% improvement).

## How the loop works

Every iteration you:
1. Propose ONE atomic change
2. Commit it with a clear message
3. Run `sudo bash scripts/autoresearch-bench.sh`
4. Parse `METRIC score=<n>` from output
5. If score improved AND all correctness/tests pass → keep the commit
6. If it regressed or anything failed → `git revert HEAD` and try a different idea
7. Log the attempt to `docs/autoresearch/journal.jsonl`

The bench takes ~3-5 minutes per run. Budget: 100 iterations per overnight session.

## You MAY modify

- `internal/firecracker/*.go` — VM lifecycle, pool, snapshots, config
- `internal/server/*.go` — daemon routes, client
- `internal/uffd/*.go` — page-fault handler (if you touch UFFD, test extra carefully)
- `internal/agentclient/*.go` — host-to-guest agent protocol
- `internal/state/*.go` — state file handling
- `cmd/mvm-uffd/main.go`
- `agent/` — guest agent
- `scripts/install-cloud.sh` only for **rootfs** changes that measurably help

## You MAY NOT modify

- `scripts/autoresearch-bench.sh` — the benchmark itself
- `docs/**` — documentation
- `sdks/**` — Python/TS SDKs
- `vz/**` — Swift code (macOS-only, untested here)
- `Makefile` targets unrelated to perf
- `go.mod` / `go.sum` — don't swap dependencies
- Build tags that would skip Linux tests
- `internal/cli/exec_direct.go` — deleted intentionally
- TLS, auth middleware (`internal/server/auth.go`), or anything in `cmd/mvm-uffd/main_other.go`

## You MUST preserve

**Correctness is non-negotiable.** The bench script has a correctness gate that runs after perf measurements. If any of these break, `exit 5` fires and the iteration is discarded:

- `mvm exec` returns the actual command output (not "ok" unconditionally)
- Exit codes propagate (exec of `false` returns nonzero)
- File roundtrip works (write via exec, read via exec)
- Pool always has 3 slots total (can't set PoolSize to 0)
- All Go unit tests pass (`go test ./internal/...`)

The bench also exits nonzero on any mvm command failure, so breaking functionality will be caught.

## Known leverage points (humans think these matter)

Listed by estimated impact. Start with the easier wins:

### 1. Snapshot restore: File backend copy is synchronous 2GB (`snapshot.go`)
UFFD already exists and is wired, but the File-backend fallback still copies `mem.bin` to `vmDir`. `cp --sparse=always` on 2GB sparse files takes ~9s. Options:
- Use `cp --reflink=auto` if the underlying filesystem supports it (btrfs/xfs — returns immediately if supported, falls back to sparse if not)
- Skip the copy entirely when UFFD is active (already done, but verify) and also when the snapshot is known-read-only

### 2. Pool refill is serial (`pool.go`)
`RefillPool` processes one slot at a time. On a 4-vCPU box, parallel refill across 3 slots could cut pool-warm from 60s to ~25s (one cold boot + 2 parallel snapshot restores).

### 3. Sudo overhead in daemon commands (`snapshot.go`, `pool.go`, `process.go`)
Every `sudo` on a cold process takes 5-20ms for PAM + setuid. A VM start does ~10 sudo calls = 50-200ms. The daemon already runs as root (per systemd unit) — most sudo calls are redundant. Detect `uid == 0` and skip sudo when already privileged.

### 4. Exec round-trip: HTTP + NDJSON + vsock framing stack (`routes.go`, `agentclient/*.go`)
For `exec true` the dominant cost is protocol overhead, not the command. Measure where the 16ms goes:
- HTTP server routing
- JSON marshaling of request
- Vsock dial to guest agent
- Agent protocol handshake
- JSON unmarshal of response

A persistent vsock connection pool could eliminate the dial cost. Warning: agent protocol currently closes connection after each request — check the agent side.

### 5. First-exec-after-pool-claim is 300-450ms, stabilizes to 16ms (`snapshot.go`, `uffd/handler.go`)
Memory pages fault in from mem.bin on demand via UFFD. Options:
- During pool claim, prefetch "hot" pages (kernel text, init data, agent binary) with `madvise(MADV_WILLNEED)` on the backing mmap
- Serve more pages per UFFDIO_COPY (batch) — but kernel ABI limits this to 1 page at a time

### 6. Pool-warm golden snapshot includes a Claude pre-warm step (`pool.go`)
The first pool boot runs `claude --help` to warm the Node.js compile cache, then snapshots. For users who don't care about Claude startup time, this adds ~30s to pool warm. Gate it behind `MVM_PREWARM_CLAUDE=1`.

### 7. TAP device setup is sequential (`process.go`, `pool.go`)
Each `ip link/addr/set` is a separate sudo invocation. Batch into one script.

### 8. `mvm exec` JSON output has Base64-encoded stdout
For `echo ok`, we serialize "ok\n" as base64 "b2sK" inside JSON. Noticeable overhead for large outputs. Could use NDJSON streaming with raw text (avoid base64) if Content-Type indicates it.

## Things NOT to try (waste of iterations)

- **Switching Firecracker for a different VMM** — out of scope
- **Writing a custom kernel** — you can't do this in 100 iterations
- **Changing the rootfs distro** — rebuild is 70s, eats your budget
- **"Optimizing" the benchmark harness** — forbidden and gameable
- **Disabling features** — if it turns off the agent, network policy, or seccomp, you're gaming

## Gaming patterns to avoid

The correctness gate catches these, but don't waste iterations on them:

- Setting `PoolSize = 0` or 1 (gate checks ≥3)
- Having the agent return "ok" for `exec` without actually running the command (gate runs `echo correctness_ok`)
- Returning 0 for all exit codes (gate runs `false`)
- Caching benchmark output anywhere (daemon restart each iteration prevents this)
- Pinning rootfs to tmpfs (rootfs must persist; bench restarts daemon)
- Short-circuiting UFFD by skipping memory load (VM must actually work after restore)

## Workflow

Each iteration:

```bash
# 1. Create an idea from the leverage points above. One change only.
# 2. Implement it
git add <files>
git commit -m "<change>: <one-line reasoning>"

# 3. Build
go build ./... || { git reset --hard HEAD~1; continue; }

# 4. Run benchmark
sudo bash scripts/autoresearch-bench.sh > /tmp/bench.log 2>&1
if [ $? -ne 0 ]; then
    # Correctness or test failure — discard
    git reset --hard HEAD~1
    continue
fi

# 5. Parse new score
NEW_SCORE=$(grep 'METRIC score=' /tmp/bench.log | tail -1 | cut -d= -f2)
BEST_SCORE=$(cat docs/autoresearch/best.txt 2>/dev/null || echo 999999)

# 6. Keep or revert
if [ "$NEW_SCORE" -lt "$BEST_SCORE" ]; then
    echo "$NEW_SCORE" > docs/autoresearch/best.txt
    git add docs/autoresearch/best.txt
    git commit --amend --no-edit
else
    git reset --hard HEAD~1
fi

# 7. Log attempt
echo "{\"ts\":\"$(date -u +%FT%TZ)\",\"change\":\"...\",\"score\":$NEW_SCORE,\"kept\":<bool>}" >> docs/autoresearch/journal.jsonl
```

## Environment notes

- Running on GCP n2-standard-4, Debian 12, nested virt ON
- mvm daemon runs as root via systemd
- `/var/mvm` is on ext4 (no reflink support — btrfs/xfs would require a reformat, out of scope)
- Claude/Codex/etc are pre-installed in the rootfs (pool-warm step pre-launches Claude)
- 8 CPUs, 16GB RAM — you have room for parallelism
- The source repo is at `/root/firecracker`

## End of round criteria

Stop when ANY of:
- Score ≤ 3000 (target hit)
- 100 iterations done
- 3 consecutive regressions on the same sub-metric (you're stuck — surface to the human)

## For the human reviewer

When you come back in the morning:
1. `git log --oneline main..HEAD` — see what landed
2. `tail -20 docs/autoresearch/journal.jsonl` — see what was tried
3. Read each kept commit's diff
4. Run `sudo bash scripts/autoresearch-bench.sh` yourself — verify score and correctness
5. Cherry-pick winners into main, discard rest
