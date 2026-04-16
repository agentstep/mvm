# mvm autoresearch

This directory contains the setup for running [autoresearch](https://github.com/karpathy/autoresearch)-style AI-driven performance optimization on mvm.

## What is this

An autonomous loop where Claude Code proposes optimizations, runs a benchmark, keeps wins, and reverts regressions. Unattended overnight, ~100 iterations.

## Files

- `program.md` — the goal, rules, and leverage points (Claude reads this)
- `../../scripts/autoresearch-bench.sh` — the benchmark harness (prints `METRIC x=<n>`)
- `journal.jsonl` — every iteration's attempt (created by the loop)
- `best.txt` — current best score (created by the loop)

## Prerequisites

1. A GCP VM with nested virtualization + mvm installed per `scripts/install-cloud.sh`
2. Claude Code installed on that VM
3. The `autoresearch` Claude Code skill installed:
   ```bash
   claude skill install github.com/uditgoenka/autoresearch
   ```

## Running

```bash
# On the GCP benchmark VM
gcloud compute ssh mvm-bench --project=agentstep --zone=us-central1-a

# Clone the repo on the branch
sudo git clone https://github.com/paulmeller/mvm.git /root/firecracker
cd /root/firecracker
sudo git checkout autoresearch

# Verify the bench works from scratch
sudo bash scripts/autoresearch-bench.sh

# Should print METRIC lines and exit 0.

# Start the loop
tmux new -s autoresearch
claude
# /autoresearch:optimize docs/autoresearch/program.md --max-iters 100
```

## Cost

- GCP: ~$0.20/hr × 8h = ~$1.60
- Claude API: ~$0.12 × 100 iters = ~$12 (Sonnet) or ~$60 (Opus)

## Safety

- The bench script resets daemon + pool between iterations (prevents caching)
- Correctness gate after perf measurements (exec real command, exit code propagation, pool size check)
- Go unit tests run every iteration
- Runs on a dedicated `autoresearch` branch — never on main
- Human reviews the diff before cherry-picking to main

## When you come back

```bash
git log --oneline main..autoresearch
tail -20 docs/autoresearch/journal.jsonl
cat docs/autoresearch/best.txt

# Verify manually
sudo bash scripts/autoresearch-bench.sh

# Cherry-pick wins
git checkout main
git cherry-pick <sha>
```
