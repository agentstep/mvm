#!/bin/bash
# Autoresearch loop driver — runs on the GCP VM.
#
# For each of N iterations:
#   1. Prompt Claude to propose one atomic change + implement it.
#   2. Run bench, capture score.
#   3. Keep if improved, revert if regressed or bench failed.
#   4. Log every attempt to journal.jsonl.
#
# Guardrails:
#   - Baseline must be a real measurement (not 999999) before iterating.
#   - Forbidden files are chattr'd immutable during each iter + post-iter check.
#   - Each iter capped at 10 min via `timeout`, Claude at 20 turns.
#   - Stream-json output so we see live progress.
#
# Usage: MAX_ITERS=10 bash autoresearch-loop.sh

set -euo pipefail

REPO=/root/mvm
JOURNAL=/root/autoresearch/journal.jsonl
BEST_FILE=/root/autoresearch/best.txt
BENCH=/root/mvm/scripts/autoresearch-bench.sh
MAX_ITERS="${MAX_ITERS:-10}"
ITER_TIMEOUT="${ITER_TIMEOUT:-600}"   # 10 min per iter hard cap
CLAUDE_MAX_TURNS="${CLAUDE_MAX_TURNS:-20}"

mkdir -p /root/autoresearch
chmod 755 /root/autoresearch

# Git identity for the loop
cd "$REPO"
git config user.email "autoresearch@mvm.local" 2>/dev/null || true
git config user.name "autoresearch" 2>/dev/null || true

# ------------------------------------------------------------------
# Protect forbidden files with chattr +i (immutable — even root can't edit)
# ------------------------------------------------------------------

FORBIDDEN=(
    "$REPO/scripts/autoresearch-bench.sh"
    "$REPO/scripts/autoresearch-loop.sh"
    "$REPO/docs/autoresearch/program.md"
    "$REPO/internal/server/auth.go"
)

lock_forbidden() {
    for f in "${FORBIDDEN[@]}"; do
        [ -f "$f" ] && chattr +i "$f" 2>/dev/null || true
    done
}

unlock_forbidden() {
    for f in "${FORBIDDEN[@]}"; do
        [ -f "$f" ] && chattr -i "$f" 2>/dev/null || true
    done
}

trap unlock_forbidden EXIT
lock_forbidden
echo "[loop] forbidden files locked (chattr +i)" >&2

# ------------------------------------------------------------------
# Establish a REAL baseline. Bail if bench can't produce a valid score.
# ------------------------------------------------------------------

if [ ! -f "$BEST_FILE" ]; then
    echo "[loop] measuring baseline (up to 3 attempts)..." >&2
    BASELINE_SCORE=""
    for attempt in 1 2 3; do
        echo "[loop]   baseline attempt $attempt..." >&2
        if BENCH_FAST=1 bash "$BENCH" > /tmp/baseline.log 2>&1; then
            BASELINE_SCORE=$(grep 'METRIC score=' /tmp/baseline.log | tail -1 | cut -d= -f2)
            if [ -n "$BASELINE_SCORE" ] && [ "$BASELINE_SCORE" -gt 0 ] 2>/dev/null; then
                break
            fi
        fi
        echo "[loop]   baseline attempt $attempt failed — see /tmp/baseline.log" >&2
        sleep 5
    done

    if [ -z "$BASELINE_SCORE" ] || ! [ "$BASELINE_SCORE" -gt 0 ] 2>/dev/null; then
        echo "[loop] FATAL: could not establish baseline score after 3 attempts." >&2
        echo "[loop]        Last bench log:" >&2
        tail -20 /tmp/baseline.log >&2
        exit 1
    fi

    echo "$BASELINE_SCORE" > "$BEST_FILE"
    BASELINE_METRICS=$(grep '^METRIC' /tmp/baseline.log | tr '\n' ' ')
    echo "{\"ts\":\"$(date -u +%FT%TZ)\",\"iter\":0,\"change\":\"baseline\",\"kept\":true,\"score\":$BASELINE_SCORE,\"metrics\":\"$BASELINE_METRICS\"}" >> "$JOURNAL"
    echo "[loop] baseline score: $BASELINE_SCORE" >&2
fi

# ------------------------------------------------------------------
# Main loop
# ------------------------------------------------------------------

for iter in $(seq 1 "$MAX_ITERS"); do
    echo "" >&2
    echo "============================================================" >&2
    echo "=== ITERATION $iter / $MAX_ITERS" >&2
    echo "============================================================" >&2

    BEST=$(cat "$BEST_FILE")
    JOURNAL_TAIL=$(tail -10 "$JOURNAL" 2>/dev/null || echo "(empty)")

    # Record pre-iter HEAD so we can check for forbidden-file changes later.
    PRE_ITER_HEAD=$(cd "$REPO" && git rev-parse HEAD)

    PROMPT="You are inside an autoresearch loop on the mvm project at /root/mvm.

GOAL: reduce the composite 'score' metric printed by:
  bash /root/mvm/scripts/autoresearch-bench.sh

Current best score: $BEST (lower is better).
Run this iteration using BENCH_FAST=1 (skips snapshot metrics, ~90s).

SCORE FORMULA (BENCH_FAST mode):
  score = tti_ms + exec_warm_ms * 20

Recent attempts (journal JSONL):
$JOURNAL_TAIL

YOU MUST:
1. READ /root/mvm/docs/autoresearch/program.md — editable/forbidden files + leverage points.
2. Propose ONE atomic optimization. Be surgical. Pick from the leverage points.
3. Implement it. Keep the change small (ideally 1-2 files, <30 lines).
4. Rebuild: cd /root/mvm && /usr/local/go/bin/go build ./...
5. If build fails: run 'cd /root/mvm && git reset --hard HEAD' and exit.
6. Deploy: sudo cp /root/mvm/bin/mvm-linux-amd64 /usr/local/bin/mvm 2>/dev/null ||
   /usr/local/go/bin/go build -o /tmp/mvm-new ./cmd/mvm/ && sudo cp /tmp/mvm-new /usr/local/bin/mvm
   sudo systemctl restart mvm-daemon && sleep 3
7. Run bench: sudo BENCH_FAST=1 bash /root/mvm/scripts/autoresearch-bench.sh > /tmp/iter-$iter.log 2>&1
8. Parse: NEW_SCORE=\$(grep 'METRIC score=' /tmp/iter-$iter.log | tail -1 | cut -d= -f2)
9. If NEW_SCORE exists AND NEW_SCORE < $BEST:
   - cd /root/mvm && git add -A && git commit -m 'iter $iter: <short description>'
   - echo \$NEW_SCORE > /root/autoresearch/best.txt
   - KEPT=true
10. Else:
   - cd /root/mvm && git reset --hard HEAD
   - sudo systemctl restart mvm-daemon && sleep 3
   - KEPT=false
11. Append to /root/autoresearch/journal.jsonl (one line):
   {\"ts\":\"...\",\"iter\":$iter,\"change\":\"<description>\",\"kept\":<KEPT>,\"score\":<NEW_SCORE>,\"metrics\":\"<METRIC lines joined>\"}

STRICT RULES:
- Do not touch /root/mvm/scripts/autoresearch-bench.sh (chattr +i)
- Do not touch /root/mvm/docs/autoresearch/program.md (chattr +i)
- One atomic change per iteration
- Report status at the end: DONE / BLOCKED

Be decisive. 20 turns max. Aim for actual code changes, not analysis.
"

    echo "$PROMPT" > /tmp/prompt-$iter.txt

    # Run Claude with stream-json + verbose so we see live tool usage.
    # Wrap in timeout so a stuck iter can't block the whole loop.
    echo "[iter $iter] launching Claude..." >&2
    timeout "$ITER_TIMEOUT" claude -p \
        --output-format stream-json \
        --verbose \
        --permission-mode acceptEdits \
        --max-turns "$CLAUDE_MAX_TURNS" \
        --allowedTools "Bash(*),Edit,Write,Read,Glob,Grep" \
        < /tmp/prompt-$iter.txt \
        > /tmp/claude-$iter.jsonl 2>&1 || {
            echo "[iter $iter] Claude exited non-zero (likely timeout or max-turns)" >&2
        }

    # Post-iter forbidden-file audit.
    cd "$REPO"
    CURRENT_HEAD=$(git rev-parse HEAD)
    if [ "$CURRENT_HEAD" != "$PRE_ITER_HEAD" ]; then
        FORBIDDEN_TOUCHED=$(git diff --name-only "$PRE_ITER_HEAD" HEAD | grep -E '(autoresearch-bench\.sh|autoresearch-loop\.sh|program\.md|internal/server/auth\.go)$' || true)
        if [ -n "$FORBIDDEN_TOUCHED" ]; then
            echo "[iter $iter] REVERT: iteration touched forbidden files: $FORBIDDEN_TOUCHED" >&2
            git reset --hard "$PRE_ITER_HEAD"
            # Don't update best.txt
            echo "{\"ts\":\"$(date -u +%FT%TZ)\",\"iter\":$iter,\"change\":\"reverted — touched forbidden files\",\"kept\":false}" >> "$JOURNAL"
            continue
        fi
    fi

    NEW_BEST=$(cat "$BEST_FILE")
    echo "[iter $iter] done. Best: $NEW_BEST" >&2
done

# ------------------------------------------------------------------
# Final report
# ------------------------------------------------------------------

echo "" >&2
echo "=== ALL ITERATIONS COMPLETE ===" >&2
echo "Final best score: $(cat $BEST_FILE)" >&2
echo "" >&2
echo "Journal:" >&2
cat "$JOURNAL" >&2
echo "" >&2
echo "Commits made:" >&2
cd "$REPO" && git log --oneline "$(git rev-list --max-parents=0 HEAD | head -1)..HEAD" >&2
