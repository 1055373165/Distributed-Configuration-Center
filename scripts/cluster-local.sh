#!/usr/bin/env bash
# cluster-local.sh — Start a 3-node PaladinCore cluster on localhost.
# Docker-free replacement for `docker compose up -d`.
#
# Default port layout (override with env vars, see below):
#   node1  http://127.0.0.1:$HTTP_BASE       raft 127.0.0.1:$RAFT_BASE       (bootstrap)
#   node2  http://127.0.0.1:$((HTTP_BASE+1)) raft 127.0.0.1:$((RAFT_BASE+1))
#   node3  http://127.0.0.1:$((HTTP_BASE+2)) raft 127.0.0.1:$((RAFT_BASE+2))
#
# Defaults: HTTP_BASE=8080  RAFT_BASE=9001  (matches docker-compose.yml)
#
# Usage:
#   ./scripts/cluster-local.sh                        # start (keep data)
#   ./scripts/cluster-local.sh --fresh                # wipe data-node{1,2,3}/ first
#   HTTP_BASE=18080 RAFT_BASE=19001 ./scripts/cluster-local.sh
#
# Stop with ./scripts/cluster-stop.sh

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/paladin-core"
LOG_DIR="$ROOT/.cluster-logs"
PID_FILE="$ROOT/.cluster-pids"

HTTP_BASE="${HTTP_BASE:-8080}"
RAFT_BASE="${RAFT_BASE:-9001}"

FRESH=0
if [ "${1:-}" = "--fresh" ]; then
  FRESH=1
fi

# --- Guard against double-start ---
if [ -f "$PID_FILE" ]; then
  if xargs -I{} kill -0 {} 2>/dev/null < "$PID_FILE"; then
    echo "cluster already running (see $PID_FILE). Run ./scripts/cluster-stop.sh first." >&2
    exit 1
  fi
  rm -f "$PID_FILE"
fi

mkdir -p "$LOG_DIR"

if [ "$FRESH" -eq 1 ]; then
  echo "==> wiping data dirs"
  rm -rf "$ROOT/data-node1" "$ROOT/data-node2" "$ROOT/data-node3"
fi

# --- Pre-flight: check all 6 ports are free ---
port_in_use() {
  # Returns 0 (true) if something is LISTENing on the given TCP port.
  lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
}

check_ports() {
  local busy=0 p
  for p in "$HTTP_BASE" $((HTTP_BASE+1)) $((HTTP_BASE+2)) \
           "$RAFT_BASE" $((RAFT_BASE+1)) $((RAFT_BASE+2)); do
    if port_in_use "$p"; then
      echo "port $p already in use:" >&2
      lsof -nP -iTCP:"$p" -sTCP:LISTEN >&2 || true
      busy=1
    fi
  done
  if [ "$busy" -ne 0 ]; then
    cat >&2 <<EOF

Some ports are taken. Options:
  1) stop the conflicting process
  2) pick different ports, e.g.:
       HTTP_BASE=18080 RAFT_BASE=19001 ./scripts/cluster-local.sh
EOF
    exit 1
  fi
}
check_ports

# --- Build binary if missing / stale ---
# Previously we only checked cmd/paladin-core/main.go which missed edits in
# raft/, server/, store/, internal/* etc — leading to confusing "why is my
# fix not live?" moments. Compare against the newest .go file in the tree.
need_build=0
if [ ! -x "$BIN" ]; then
  need_build=1
else
  newest_src=$(find "$ROOT" -name '*.go' -not -path '*/data-node*/*' -newer "$BIN" -print -quit 2>/dev/null || true)
  if [ -n "$newest_src" ]; then
    need_build=1
  fi
fi
if [ "$need_build" -eq 1 ]; then
  echo "==> building paladin-core (sources newer than binary)"
  (cd "$ROOT" && go build -o paladin-core ./cmd/paladin-core)
fi

# --- Helpers ---
start_node() {
  local id="$1" raft="$2" http="$3" data="$4"
  shift 4
  echo "==> starting $id  raft=$raft  http=$http  $*"
  "$BIN" cluster \
    --id "$id" \
    --raft "$raft" \
    --http "$http" \
    --data "$data" \
    "$@" \
    > "$LOG_DIR/$id.log" 2>&1 &
  echo $! >> "$PID_FILE"
}

cleanup_on_fail() {
  echo "==> startup failed, cleaning up" >&2
  if [ -f "$PID_FILE" ]; then
    while IFS= read -r pid; do
      [ -n "$pid" ] || continue
      kill "$pid" 2>/dev/null || true
    done < "$PID_FILE"
    rm -f "$PID_FILE"
  fi
}

wait_leader() {
  local url="$1"
  for _ in $(seq 1 40); do
    sleep 0.25
    if curl -sf "$url/admin/stats" 2>/dev/null | grep -q '"state":"Leader"'; then
      return 0
    fi
  done
  echo "timed out waiting for Leader at $url (see $LOG_DIR/node1.log)" >&2
  return 1
}

# --- Start cluster ---
: > "$PID_FILE"
trap 'cleanup_on_fail' ERR

H1=$HTTP_BASE;          H2=$((HTTP_BASE+1)); H3=$((HTTP_BASE+2))
R1=$RAFT_BASE;          R2=$((RAFT_BASE+1)); R3=$((RAFT_BASE+2))

start_node node1 "127.0.0.1:$R1" ":$H1" data-node1 --bootstrap
wait_leader "http://127.0.0.1:$H1"
echo "==> node1 is Leader"

start_node node2 "127.0.0.1:$R2" ":$H2" data-node2 --join "127.0.0.1:$H1"
start_node node3 "127.0.0.1:$R3" ":$H3" data-node3 --join "127.0.0.1:$H1"

sleep 0.5
trap - ERR

cat <<EOF

Cluster started:
  node1  http://127.0.0.1:$H1  (bootstrap)   raft 127.0.0.1:$R1
  node2  http://127.0.0.1:$H2                raft 127.0.0.1:$R2
  node3  http://127.0.0.1:$H3                raft 127.0.0.1:$R3

Logs : tail -f $LOG_DIR/node{1,2,3}.log
PIDs : $(tr '\n' ' ' < "$PID_FILE")
Stop : ./scripts/cluster-stop.sh           (keep data)
       ./scripts/cluster-stop.sh --clean   (also wipe data-node{1,2,3}/)

Try:
  curl -X PUT http://127.0.0.1:$H1/api/v1/config/public/prod/db_host -d '10.0.0.1'
  curl     http://127.0.0.1:$H2/api/v1/config/public/prod/db_host
  curl     http://127.0.0.1:$H3/admin/stats | jq .
EOF
