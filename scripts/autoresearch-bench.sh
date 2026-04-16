#!/usr/bin/env bash
# autoresearch-bench.sh — mvm performance + correctness benchmark for autoresearch.
#
# Prints METRIC <name>=<value> lines to stdout. Returns nonzero on ANY
# correctness failure so the autoresearch loop treats it as a regression,
# not a winning diff.
#
# Must be run as root on a Linux host with /dev/kvm and mvm installed.
# Restarts daemon + clears pool between runs to prevent benchmark gaming
# (caching, stale state, pre-created VMs).

set -euo pipefail

# -------------------------------------------------------------------
# Prerequisites
# -------------------------------------------------------------------

if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: must be run as root" >&2
    exit 2
fi
if [ ! -c /dev/kvm ]; then
    echo "ERROR: /dev/kvm not accessible" >&2
    exit 2
fi
if ! command -v mvm >/dev/null 2>&1; then
    echo "ERROR: mvm not in PATH" >&2
    exit 2
fi

# Set a short timeout so a stuck VM doesn't hang the loop forever.
export MVM_BENCH_TIMEOUT=60

# -------------------------------------------------------------------
# Reset state — critical for benchmark integrity
# -------------------------------------------------------------------

echo "[bench] resetting state..." >&2
systemctl stop mvm-daemon 2>/dev/null || true
pkill -9 firecracker mvm-uffd 2>/dev/null || true
sleep 1
# Reset runtime state. Preserve /var/mvm/pool/snapshot/ (the golden snapshot
# infrastructure — recreating it costs ~60s and isn't something a single
# benchmark iteration should amortize).
rm -rf /var/mvm/vms/* 2>/dev/null || true
rm -rf /var/mvm/pool/slot* 2>/dev/null || true
rm -f /run/mvm/*.socket /run/mvm/*.vsock* /run/mvm/*-uffd.sock 2>/dev/null || true
# Reset any leftover TAP devices from previous runs
for t in $(ip link show 2>/dev/null | grep -oE 'tap[0-9]+' | sort -u); do
    ip link del "$t" 2>/dev/null || true
done
# Reset state.json — keep initialized flag but clear VM list
mkdir -p /root/.mvm
echo '{"initialized":true,"backend":"firecracker","vms":{}}' > /root/.mvm/state.json
systemctl start mvm-daemon
sleep 3

# Verify daemon up
if ! systemctl is-active --quiet mvm-daemon; then
    echo "ERROR: mvm-daemon did not start" >&2
    exit 2
fi

# -------------------------------------------------------------------
# Warm the pool (part of setup, NOT measured)
# -------------------------------------------------------------------

echo "[bench] warming pool..." >&2
mvm pool warm >/dev/null 2>&1 || true
POOL_WARM_START=$(date +%s)
# 180s timeout: first iteration builds the golden snapshot (~60s). Subsequent
# iterations restore from it (~10-15s per slot, 3 slots in parallel).
for _ in $(seq 1 180); do
    if mvm pool status 2>/dev/null | grep -q "3/3"; then break; fi
    sleep 1
    # Nudge pool refill periodically (some code paths need re-triggering)
    if [ $(( $(date +%s) - POOL_WARM_START )) -gt 5 ] && \
       [ $(( ($(date +%s) - POOL_WARM_START) % 15 )) -eq 0 ]; then
        mvm pool warm >/dev/null 2>&1 || true
    fi
done
POOL_WARM_SEC=$(( $(date +%s) - POOL_WARM_START ))
echo "METRIC pool_warm_s=$POOL_WARM_SEC"

if ! mvm pool status 2>/dev/null | grep -q "3/3"; then
    echo "ERROR: pool did not reach 3/3 within 180s" >&2
    exit 3
fi

# -------------------------------------------------------------------
# Metric 1: TTI (create + first exec), 5 samples, report median
# -------------------------------------------------------------------

echo "[bench] measuring TTI..." >&2
declare -a TTI_SAMPLES=()
for i in 1 2 3 4 5; do
    # Refill pool between samples so we're always measuring warm-pool claim.
    # Extra settle time after 3/3 because the "ready" flag sometimes fires
    # before the VM is fully accepting exec; retry exec with brief backoff.
    while ! mvm pool status 2>/dev/null | grep -q "3/3"; do
        mvm pool warm >/dev/null 2>&1 || true
        sleep 2
    done
    sleep 1  # small extra settle

    T0=$(date +%s%N)
    mvm start "b$i" >/dev/null 2>&1 || { echo "ERROR: start b$i failed" >&2; exit 4; }

    # Exec with small retry — pool claim sets up sockets async on some paths
    EXEC_OK=0
    for retry in 1 2 3 4 5; do
        if mvm exec "b$i" -- echo ok >/dev/null 2>&1; then
            EXEC_OK=1
            break
        fi
        sleep 0.2
    done
    if [ $EXEC_OK -eq 0 ]; then
        echo "ERROR: exec b$i failed after 5 retries" >&2
        mvm delete "b$i" --force >/dev/null 2>&1 || true
        exit 4
    fi
    T1=$(date +%s%N)
    MS=$(( (T1 - T0) / 1000000 ))
    TTI_SAMPLES+=( "$MS" )

    mvm delete "b$i" --force >/dev/null 2>&1 || true
done

# Median of 5
TTI_MEDIAN=$(printf '%s\n' "${TTI_SAMPLES[@]}" | sort -n | awk 'NR==3')
echo "METRIC tti_ms=$TTI_MEDIAN"

# -------------------------------------------------------------------
# Metric 2: exec_warm_ms — steady-state exec latency, 20 samples, median
# -------------------------------------------------------------------

echo "[bench] measuring exec latency..." >&2
while ! mvm pool status 2>/dev/null | grep -q "3/3"; do
    mvm pool warm >/dev/null 2>&1 || true
    sleep 2
done
mvm start bw >/dev/null 2>&1 || { echo "ERROR: start bw failed" >&2; exit 4; }

# First exec is slow (page-in); warmup and discard
for _ in 1 2 3; do
    mvm exec bw -- true >/dev/null 2>&1 || { echo "ERROR: warmup exec failed" >&2; exit 4; }
done

declare -a EX_SAMPLES=()
for _ in $(seq 1 20); do
    T0=$(date +%s%N)
    mvm exec bw -- true >/dev/null 2>&1 || { echo "ERROR: exec failed" >&2; exit 4; }
    T1=$(date +%s%N)
    EX_SAMPLES+=( "$(( (T1 - T0) / 1000000 ))" )
done
EXEC_WARM_MEDIAN=$(printf '%s\n' "${EX_SAMPLES[@]}" | sort -n | awk 'NR==10')
echo "METRIC exec_warm_ms=$EXEC_WARM_MEDIAN"

# -------------------------------------------------------------------
# Metric 3: snapshot_create_ms (2GB VM)
# -------------------------------------------------------------------

echo "[bench] measuring snapshot create..." >&2
T0=$(date +%s%N)
mvm snapshot create bw bench_snap >/dev/null 2>&1 || { echo "ERROR: snapshot create failed" >&2; exit 4; }
T1=$(date +%s%N)
SNAP_CREATE_MS=$(( (T1 - T0) / 1000000 ))
echo "METRIC snap_create_ms=$SNAP_CREATE_MS"

# -------------------------------------------------------------------
# Metric 4: snapshot_restore_ms
# -------------------------------------------------------------------

echo "[bench] measuring snapshot restore..." >&2
mvm stop bw --force >/dev/null 2>&1 || true
mvm delete bw --force >/dev/null 2>&1 || true
sleep 2

T0=$(date +%s%N)
mvm snapshot restore bw bench_snap >/dev/null 2>&1 || { echo "ERROR: snapshot restore failed" >&2; exit 4; }
T1=$(date +%s%N)
SNAP_RESTORE_MS=$(( (T1 - T0) / 1000000 ))
echo "METRIC snap_restore_ms=$SNAP_RESTORE_MS"

# -------------------------------------------------------------------
# Correctness gate — VM must still work end-to-end.
# Fail the whole benchmark if any of these regress.
# -------------------------------------------------------------------

echo "[bench] running correctness gate..." >&2

# Restored VM must still be functional.
if ! mvm exec bw -- true >/dev/null 2>&1; then
    echo "ERROR: exec failed on restored VM (correctness regression)" >&2
    exit 5
fi

# Guest agent protocol works end-to-end.
OUT=$(mvm exec bw -- echo "correctness_ok" 2>&1 | head -1)
if [ "$OUT" != "correctness_ok" ]; then
    echo "ERROR: exec returned wrong output '$OUT' (expected 'correctness_ok')" >&2
    exit 5
fi

# Agent stdout is not being short-circuited.
# Writing + reading a file round-trips data.
mvm exec bw -- sh -c 'echo roundtrip_$$ > /tmp/rt' >/dev/null 2>&1 \
    || { echo "ERROR: file write via exec failed" >&2; exit 5; }
RTOUT=$(mvm exec bw -- cat /tmp/rt 2>&1 | head -1)
if ! echo "$RTOUT" | grep -q "roundtrip_"; then
    echo "ERROR: file roundtrip failed: got '$RTOUT'" >&2
    exit 5
fi

# Exit code propagation (prevents agent-stub hacks that always return 0).
if mvm exec bw -- false >/dev/null 2>&1; then
    echo "ERROR: exec returned 0 for false (exit code not propagated)" >&2
    exit 5
fi

# Pool is real (not set to 0 to game TTI).
POOL_TOTAL=$(mvm pool status 2>&1 | grep -oE '[0-9]+/[0-9]+' | cut -d/ -f2)
if [ "${POOL_TOTAL:-0}" -lt 3 ]; then
    echo "ERROR: pool total is $POOL_TOTAL, expected >= 3 (pool gaming suspected)" >&2
    exit 5
fi

echo "[bench] correctness gate passed" >&2

# -------------------------------------------------------------------
# Go tests must still pass.
# -------------------------------------------------------------------

if [ -f /root/firecracker/go.mod ]; then
    echo "[bench] running go tests..." >&2
    cd /root/firecracker
    TMPDIR=/tmp go test -race -count=1 -timeout=120s ./internal/uffd/... ./internal/server/... ./internal/firecracker/... ./internal/state/... >/tmp/autoresearch-gotest.log 2>&1 \
        || { echo "ERROR: go tests failed — see /tmp/autoresearch-gotest.log" >&2; exit 6; }
    echo "[bench] go tests passed" >&2
fi

# -------------------------------------------------------------------
# Composite score (lower is better)
# Weighted to discourage single-axis wins at others' expense.
# -------------------------------------------------------------------

SCORE=$(( TTI_MEDIAN + EXEC_WARM_MEDIAN * 20 + SNAP_CREATE_MS / 10 + SNAP_RESTORE_MS / 10 ))
echo "METRIC score=$SCORE"

# -------------------------------------------------------------------
# Cleanup
# -------------------------------------------------------------------

mvm delete bw --force >/dev/null 2>&1 || true
mvm snapshot delete bench_snap >/dev/null 2>&1 || true
