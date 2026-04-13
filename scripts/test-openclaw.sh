#!/bin/bash
#
# Test: Post-install OpenClaw into a running mvm and verify it works
#
# This proves the real user story: start a blank VM, install OpenClaw,
# run it. No pre-installation in the rootfs.
#

set -euo pipefail

MVM="./bin/mvm"
PASS=0
FAIL=0

pass() { PASS=$((PASS+1)); echo "  ✓ $1"; }
fail() { FAIL=$((FAIL+1)); echo "  ✗ $1"; }

ssh_vm() {
    limactl shell mvm -- sudo ssh -i /opt/mvm/keys/mvm.id_ed25519 \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ServerAliveInterval=30 -o ServerAliveCountMax=10 \
        -o ConnectTimeout=10 "root@172.16.0.2" "$@" 2>/dev/null
}

echo "================================================"
echo "  OpenClaw Post-Install Test"
echo "================================================"
echo ""

# Clean
$MVM delete openclaw-test --force 2>/dev/null || true

# Start
echo "[1. Start blank VM]"
$MVM pool warm 2>/dev/null || true
$MVM start openclaw-test 2>&1 | tail -3
echo ""

# Check resources
echo "[2. Resources]"
ssh_vm "free -m; df -h /"
echo ""

# Install OpenClaw
echo "[3. npm install -g openclaw (this takes a few minutes)...]"
START_TIME=$(date +%s)
ssh_vm "npm install -g openclaw 2>&1 | tail -5"
END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))
echo "  Install took ${ELAPSED}s"
echo ""

# Verify binary
echo "[4. Verify binary]"
WHICH=$(ssh_vm "which openclaw 2>/dev/null || find /usr/local -name openclaw -type f 2>/dev/null | head -1")
if [ -n "$WHICH" ]; then
    pass "openclaw at: $WHICH"
else
    fail "openclaw not found after install"
fi

# Version
VER=$(ssh_vm "openclaw --version 2>&1 | head -1 || /usr/local/bin/openclaw --version 2>&1 | head -1" || echo "failed")
if echo "$VER" | grep -qiE "[0-9]"; then
    pass "version: $VER"
else
    fail "version check: $VER"
fi
echo ""

# Doctor
echo "[5. openclaw doctor]"
DOC=$(ssh_vm "timeout 15 openclaw doctor 2>&1 | head -10 || timeout 15 /usr/local/bin/openclaw doctor 2>&1 | head -10" || echo "failed")
if [ -n "$DOC" ] && [ "$DOC" != "failed" ]; then
    pass "doctor output:"
    echo "$DOC" | head -5 | sed 's/^/    /'
else
    fail "doctor: $DOC"
fi
echo ""

# Cleanup
echo "[6. Cleanup]"
$MVM delete openclaw-test --force
echo ""

echo "================================================"
echo "  Results: $PASS passed, $FAIL failed"
echo "================================================"
[ $FAIL -eq 0 ] || exit 1
