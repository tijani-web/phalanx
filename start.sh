#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────
# Phalanx — Container Entrypoint for Fly.io Global Mesh
#
# This script auto-configures a Phalanx node using Fly.io environment
# variables. It derives the node ID, detects the deployment region,
# discovers peers via DNS, and starts the server.
#
# Fly.io provides:
#   FLY_APP_NAME    — application name (e.g., "phalanx")
#   FLY_MACHINE_ID  — unique machine identifier
#   FLY_REGION      — deployment region (e.g., "ord", "lhr", "los")
#   FLY_ALLOC_ID    — allocation ID
#
# 5-Node Global Mesh:
#   JNB (Johannesburg) — Africa
#   LHR (London)    — Europe
#   ORD (Chicago)   — North America
#   SIN (Singapore) — Asia-Pacific
#   FRA (Frankfurt) — Central Europe
#
# Machines are reachable internally via:
#   <app>.internal           — all machines (AAAA records)
#   <machine_id>.vm.<app>.internal — specific machine
# ─────────────────────────────────────────────────────────────────────
set -euo pipefail

# ─── Node Identity ───
export NODE_ID="${FLY_MACHINE_ID:-$(hostname)}"
REGION="${FLY_REGION:-local}"
echo "» node_id: $NODE_ID"
echo "» region:  $REGION"

# ─── Region-Aware Logging ───
# Tag the node with its region for operational visibility.
export NODE_REGION="$REGION"

# ─── Peer Discovery ───
# Query DNS for all AAAA records of the app's internal hostname.
# Each record is a Fly 6PN IPv6 address of a sibling machine.
APP="${FLY_APP_NAME:-phalanx}"
INTERNAL_HOST="${APP}.internal"

echo "» discovering peers via DNS: $INTERNAL_HOST"

# Wait for DNS to be available (new deploys may take a moment).
SEEDS=""
for attempt in $(seq 1 15); do
    # dig for AAAA records; extract IPv6 addresses.
    ADDRS=$(dig +short AAAA "$INTERNAL_HOST" 2>/dev/null || true)
    if [ -n "$ADDRS" ]; then
        # Build comma-separated seed list with gossip port.
        SEEDS=$(echo "$ADDRS" | while read -r addr; do
            echo "[$addr]:${BIND_PORT:-7946}"
        done | paste -sd',' -)
        break
    fi
    echo "» waiting for DNS... (attempt $attempt/15)"
    sleep 2
done

if [ -n "$SEEDS" ]; then
    export SEEDS
    echo "» seeds: $SEEDS"
else
    echo "» no peers found — starting as genesis node"
fi

# ─── Peer List ───
# For the 5-node global mesh, peers are discovered dynamically via
# gossip. PEERS can be left empty — the discovery layer handles it.
export PEERS="${PEERS:-}"

# ─── Cross-Continental Latency Tuning ───
# These defaults are set in fly.toml [env], but we log them here
# for operational visibility.
echo "» tick_ms:   ${TICK_MS:-200}"
echo "» election:  ${ELECTION:-20} ticks ($(( ${TICK_MS:-200} * ${ELECTION:-20} ))ms)"
echo "» heartbeat: ${HEARTBEAT:-5} ticks ($(( ${TICK_MS:-200} * ${HEARTBEAT:-5} ))ms)"
echo "» quorum:    3 of 5 (tolerates 2 region failures)"

# ─── Launch ───
echo "» starting phalanx server in region $REGION..."
exec /usr/local/bin/phalanx-server
