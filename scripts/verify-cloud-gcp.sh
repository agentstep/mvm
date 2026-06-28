#!/bin/bash
set -euo pipefail

# Provision a throwaway GCP nested-virt box, run the documented cloud install,
# verify the remote CLI end-to-end, then ALWAYS tear the box down.
#
# Why this script exists: the cloud install (scripts/install-cloud.sh) runs *on*
# a Linux box, but provisioning that box was historically a manual `gcloud`
# step with no guaranteed cleanup. A verification box (`mvm-ar2`, n2-standard-4)
# was once left around for weeks. This script makes teardown automatic: the box
# is deleted on a trap that fires on success, failure, or Ctrl-C — so a
# verification run can never leak a paid instance again.
#
# Usage:
#   scripts/verify-cloud-gcp.sh                 # provision, verify, tear down
#   scripts/verify-cloud-gcp.sh --keep          # leave the box up (prints how to delete)
#   scripts/verify-cloud-gcp.sh --cleanup       # delete any leftover mvm-verify-* boxes and exit
#
# Env overrides:
#   PROJECT       (default: current gcloud project)
#   ZONE          (default: us-central1-a)
#   MACHINE_TYPE  (default: n2-standard-4 — must support nested virt)
#   IMAGE_FAMILY  (default: debian-12)  IMAGE_PROJECT (default: debian-cloud)

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
ZONE="${ZONE:-us-central1-a}"
MACHINE_TYPE="${MACHINE_TYPE:-n2-standard-4}"
IMAGE_FAMILY="${IMAGE_FAMILY:-debian-12}"
IMAGE_PROJECT="${IMAGE_PROJECT:-debian-cloud}"

# Boxes are named with a shared prefix so orphans are easy to find and sweep.
NAME_PREFIX="mvm-verify"

die() { echo "ERROR: $*" >&2; exit 1; }

[ -n "$PROJECT" ] || die "no GCP project set (pass PROJECT=... or run: gcloud config set project <id>)"
command -v gcloud >/dev/null || die "gcloud not found on PATH"

gc() { gcloud --project="$PROJECT" "$@"; }

# --- --cleanup: sweep any leftover verification boxes and exit ---------------

if [ "${1:-}" = "--cleanup" ]; then
    echo "=== Sweeping leftover ${NAME_PREFIX}-* instances in $PROJECT ==="
    # `while read` (not mapfile) so this runs on macOS's bundled bash 3.2.
    found=0
    while IFS=$'\t' read -r n z; do
        [ -n "$n" ] || continue
        found=1
        echo "Deleting $n ($z)..."
        gc compute instances delete "$n" --zone="$z" --quiet
    done < <(gc compute instances list \
        --filter="name~^${NAME_PREFIX}-" --format="value(name,zone.basename())" 2>/dev/null)
    [ "$found" -eq 1 ] && echo "Done." || echo "None found. Nothing to clean up."
    exit 0
fi

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

NAME="${NAME_PREFIX}-$(date +%Y%m%d-%H%M%S)"

# --- Teardown trap: fires on success, failure, or interrupt ------------------

teardown() {
    local rc=$?
    if [ "$KEEP" -eq 1 ]; then
        echo
        echo "⚠️  --keep set: leaving $NAME RUNNING. It is billing until you delete it:"
        echo "    gcloud compute instances delete $NAME --project=$PROJECT --zone=$ZONE --quiet"
        return
    fi
    echo
    echo "=== Tearing down $NAME ==="
    # --quiet so the trap never blocks on a prompt; ignore errors if it's already gone.
    gc compute instances delete "$NAME" --zone="$ZONE" --quiet 2>/dev/null || true
    [ "$rc" -eq 0 ] && echo "✅ Verified and torn down." || echo "❌ Failed (rc=$rc) — box torn down anyway."
}
trap teardown EXIT INT TERM

# --- Provision a nested-virt box ---------------------------------------------

echo "=== Provisioning $NAME ($MACHINE_TYPE, $ZONE, nested virt) ==="
gc compute instances create "$NAME" \
    --zone="$ZONE" \
    --machine-type="$MACHINE_TYPE" \
    --image-family="$IMAGE_FAMILY" --image-project="$IMAGE_PROJECT" \
    --enable-nested-virtualization \
    --boot-disk-size=40GB

# --- Wait for SSH to come up -------------------------------------------------

echo "=== Waiting for SSH ==="
for _ in $(seq 1 30); do
    if gc compute ssh "$NAME" --zone="$ZONE" --command="true" >/dev/null 2>&1; then
        break
    fi
    sleep 5
done

# --- Run the documented install + verify the remote CLI ----------------------

echo "=== Running install-cloud.sh on $NAME ==="
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
gc compute scp "$SCRIPT_DIR/install-cloud.sh" "$NAME:/tmp/install-cloud.sh" --zone="$ZONE"
gc compute ssh "$NAME" --zone="$ZONE" --command="sudo bash /tmp/install-cloud.sh"

echo "=== Verifying daemon is active ==="
gc compute ssh "$NAME" --zone="$ZONE" \
    --command="systemctl is-active mvm-daemon && sudo mvm list"

echo "=== install + daemon verified on $NAME ==="
# Trap tears the box down from here.
