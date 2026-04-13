#!/bin/bash
#
# Integration test for mvm — requires Apple Silicon M3+ with macOS 15+
# Run: ./scripts/integration-test.sh
#
# Tests the full VM lifecycle: init, start, exec, pause, resume,
# port forwarding, network policy, logs, stop, delete, pool.
#

set -euo pipefail

MVM="./bin/mvm"
PASS=0
FAIL=0
TOTAL=0

pass() { PASS=$((PASS+1)); TOTAL=$((TOTAL+1)); echo "  ✓ $1"; }
fail() { FAIL=$((FAIL+1)); TOTAL=$((TOTAL+1)); echo "  ✗ $1"; }

run_test() {
    local name="$1"
    shift
    if "$@" >/dev/null 2>&1; then
        pass "$name"
    else
        fail "$name"
    fi
}

run_test_output() {
    local name="$1"
    local expected="$2"
    shift 2
    local output
    output=$("$@" 2>&1) || true
    if echo "$output" | grep -q "$expected"; then
        pass "$name"
    else
        fail "$name (expected '$expected', got: $output)"
    fi
}

echo "================================================"
echo "  mvm Integration Tests"
echo "================================================"
echo ""

# Build
echo "Building..."
make build >/dev/null 2>&1
echo ""

# Ensure init has been run
echo "[Prerequisites]"
run_test_output "mvm is initialized" "true\|already initialized\|Ready" $MVM init 2>&1 || true
echo ""

# Clean slate
echo "[Cleanup]"
$MVM delete --all --force >/dev/null 2>&1 || true
echo ""

# --- Test 1: Start (cold) ---
echo "[1. Cold Start]"
run_test_output "mvm start (cold boot)" "running\|booting" $MVM start test-cold -d
sleep 2
run_test_output "VM appears in list" "test-cold" $MVM list
$MVM delete test-cold --force >/dev/null 2>&1
echo ""

# --- Test 2: Pool ---
echo "[2. Warm Pool]"
run_test_output "mvm pool warm" "Pool ready\|Pool VM" $MVM pool warm
run_test_output "mvm pool status" "1/1\|ready" $MVM pool status
echo ""

# --- Test 3: Start (warm) ---
echo "[3. Warm Start]"
run_test_output "mvm start (from pool)" "pool\|running" $MVM start test-warm
echo ""

# --- Test 4: Exec ---
echo "[4. Exec]"
run_test_output "exec uname" "Linux" $MVM exec test-warm -- uname
run_test_output "exec hostname" "mvm" $MVM exec test-warm -- hostname
run_test_output "exec with workdir" "/tmp" $MVM exec test-warm -w /tmp -- pwd
echo ""

# --- Test 5: Dev tools ---
echo "[5. Dev Tools]"
run_test_output "node installed" "v22" $MVM exec test-warm -- node --version
run_test_output "python3 installed" "Python 3" $MVM exec test-warm -- python3 --version
run_test_output "git installed" "git version" $MVM exec test-warm -- git --version
run_test_output "claude installed" "installed" $MVM exec test-warm -- "test -x /usr/local/bin/claude && echo installed || echo missing"
run_test_output "opencode installed" "installed" $MVM exec test-warm -- "test -x /usr/local/bin/opencode && echo installed || echo missing"
echo ""

# --- Test 6: Skills files ---
echo "[6. Skills Files]"
run_test_output "SKILLS.md exists" "MVM Environment" $MVM exec test-warm -- cat /.mvm/SKILLS.md
run_test_output "CLAUDE.md exists" "MVM Environment" $MVM exec test-warm -- cat /root/CLAUDE.md
echo ""

# --- Test 7: Pause/Resume ---
echo "[7. Pause/Resume]"
run_test_output "mvm pause" "paused" $MVM pause test-warm
run_test_output "status shows paused" "paused" $MVM list
run_test_output "mvm resume" "resumed" $MVM resume test-warm
run_test_output "exec after resume" "ALIVE" $MVM exec test-warm -- echo ALIVE
echo ""

# --- Test 8: Logs ---
echo "[8. Logs]"
run_test_output "boot log" "firecracker\|Linux\|Booting\|resume\|API" $MVM logs test-warm --boot -n 5
run_test_output "guest journal" "init\|login\|crng\|mvm\|syslog\|dropbear" $MVM logs test-warm -n 5
echo ""

# --- Test 9: List ---
echo "[9. List]"
run_test_output "list shows VM" "test-warm" $MVM list
run_test_output "list --json" "test-warm" $MVM list --json
run_test_output "list -q" "test-warm" $MVM list -q
echo ""

# --- Test 10: Stop ---
echo "[10. Stop]"
run_test_output "mvm stop" "stopped" $MVM stop test-warm
run_test_output "status shows stopped" "stopped" $MVM list
echo ""

# --- Test 11: Delete ---
echo "[11. Delete]"
run_test_output "mvm delete" "removed" $MVM delete test-warm
run_test_output "list is empty" "No microVMs" $MVM list
echo ""

# --- Test 12: Port forwarding (syntax only — can't test actual forwarding in CI) ---
echo "[12. Port Forwarding]"
$MVM pool warm >/dev/null 2>&1 || true
run_test_output "start with -p" "running\|pool\|booting" $MVM start test-ports -p 8080:80 -d
run_test_output "list shows VM" "test-ports" $MVM list
$MVM delete test-ports --force >/dev/null 2>&1
echo ""

# --- Test 13: Network policy (syntax only) ---
echo "[13. Network Policy]"
run_test_output "start with --net-policy deny" "running\|pool\|booting" $MVM start test-deny --net-policy deny -d
$MVM delete test-deny --force >/dev/null 2>&1
echo ""

# --- Test 14: Version ---
echo "[14. Version]"
run_test_output "mvm version" "mvm" $MVM version
echo ""

# --- Summary ---
echo "================================================"
echo "  Results: $PASS passed, $FAIL failed, $TOTAL total"
echo "================================================"

if [ $FAIL -gt 0 ]; then
    exit 1
fi
