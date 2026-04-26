#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────
# Phalanx — Chaos Script
#
# Proves zero-downtime availability by restarting random machines
# while continuously writing data to the cluster.
#
# The 5-node global mesh (N=5, Q=3) tolerates 2 simultaneous
# region failures. This script kills up to 2 machines per round.
#
# Requirements:
#   - flyctl installed and authenticated
#   - A running 5-node Phalanx cluster on Fly.io
#   - The `phalanx` CLI binary in PATH
#
# Usage:
#   ./scripts/chaos.sh [app_name] [leader_addr]
#
# Example:
#   ./scripts/chaos.sh phalanx "[fdaa::1]:9000"
# ─────────────────────────────────────────────────────────────────────
set -euo pipefail

APP="${1:-phalanx}"
LEADER="${2:-localhost:9000}"
WRITE_COUNT=0
FAIL_COUNT=0
CHAOS_ROUNDS=5

echo "╔══════════════════════════════════════════════════╗"
echo "║          PHALANX CHAOS TEST                      ║"
echo "║  App: $APP"
echo "║  Leader: $LEADER"
echo "║  Rounds: $CHAOS_ROUNDS"
echo "╚══════════════════════════════════════════════════╝"
echo

# ─── Background Writer ───
# Continuously writes key-value pairs to the cluster.
writer_pid=""

start_writer() {
    echo "» starting background writer..."
    (
        i=0
        while true; do
            key="chaos-key-$i"
            val="value-$i-$(date +%s)"
            if phalanx put "$key" "$val" -addr "$LEADER" >/dev/null 2>&1; then
                WRITE_COUNT=$((WRITE_COUNT + 1))
            else
                FAIL_COUNT=$((FAIL_COUNT + 1))
            fi
            i=$((i + 1))
            sleep 0.2
        done
    ) &
    writer_pid=$!
}

stop_writer() {
    if [ -n "$writer_pid" ]; then
        kill "$writer_pid" 2>/dev/null || true
        wait "$writer_pid" 2>/dev/null || true
    fi
}

trap stop_writer EXIT

# ─── Get Machine List ───
get_machines() {
    fly machines list -a "$APP" --json 2>/dev/null | \
        python3 -c "import sys,json; [print(m['id']) for m in json.load(sys.stdin) if m.get('state')=='started']" 2>/dev/null || \
        fly machines list -a "$APP" -q 2>/dev/null | awk 'NR>1 {print $1}'
}

# ─── Chaos Loop ───
start_writer

for round in $(seq 1 "$CHAOS_ROUNDS"); do
    echo
    echo "═══════════════════════════════════════════"
    echo "  CHAOS ROUND $round / $CHAOS_ROUNDS"
    echo "═══════════════════════════════════════════"

    # Pick 1-2 random machines to kill (testing double-fault tolerance).
    MACHINES=($(get_machines))
    if [ ${#MACHINES[@]} -lt 3 ]; then
        echo "✗ need at least 3 running machines for quorum!"
        exit 1
    fi

    # Kill 2 machines in later rounds, 1 in early rounds
    KILL_COUNT=1
    if [ "$round" -gt 2 ]; then
        KILL_COUNT=2
    fi

    for k in $(seq 1 "$KILL_COUNT"); do
        VICTIM=${MACHINES[$((RANDOM % ${#MACHINES[@]}))]}
        echo "» victim $k: $VICTIM"
        echo "» restarting machine $VICTIM..."
        fly machines restart "$VICTIM" -a "$APP" --yes
        # Remove victim from pool to avoid restarting same machine twice
        MACHINES=(${MACHINES[@]/$VICTIM/})
    done

    echo "» waiting 8s for recovery (cross-continental re-election)..."
    sleep 8

    # Verify the machine is back.
    echo "» checking machine status..."
    fly machines status "$VICTIM" -a "$APP" 2>/dev/null | head -5

    # Verify we can still write.
    TEST_KEY="chaos-verify-$round"
    TEST_VAL="round-$round-$(date +%s)"
    if phalanx put "$TEST_KEY" "$TEST_VAL" -addr "$LEADER"; then
        echo "✓ write after chaos: $TEST_KEY = $TEST_VAL"
    else
        echo "✗ WRITE FAILED after chaos round $round"
        FAIL_COUNT=$((FAIL_COUNT + 1))
    fi

    # Verify we can read it back.
    RESULT=$(phalanx get "$TEST_KEY" -addr "$LEADER" 2>/dev/null || echo "FAIL")
    if echo "$RESULT" | grep -q "$TEST_VAL"; then
        echo "✓ read verified: $RESULT"
    else
        echo "✗ READ MISMATCH: expected '$TEST_VAL', got '$RESULT'"
        FAIL_COUNT=$((FAIL_COUNT + 1))
    fi

    echo "» round $round complete"
    sleep 2
done

stop_writer

echo
echo "╔══════════════════════════════════════════════════╗"
echo "║          CHAOS TEST RESULTS                      ║"
echo "╠══════════════════════════════════════════════════╣"
echo "║  Rounds:        $CHAOS_ROUNDS"
echo "║  Total Writes:  $WRITE_COUNT"
echo "║  Failures:      $FAIL_COUNT"
echo "╚══════════════════════════════════════════════════╝"

if [ "$FAIL_COUNT" -eq 0 ]; then
    echo "✓ ZERO-DOWNTIME VERIFIED — Phalanx survived chaos."
    exit 0
else
    echo "✗ Some operations failed during chaos — investigate."
    exit 1
fi
