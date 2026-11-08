#!/usr/bin/env bash
# watchtower dev supervisor — runs the manager (out-of-cluster) and the UI in
# the background. Designed for "open the project → app is running".
#
# Usage:
#   scripts/dev.sh up       start manager + ui (builds when stale)
#   scripts/dev.sh down     stop both
#   scripts/dev.sh restart  down + up
#   scripts/dev.sh status   show pid/health
#   scripts/dev.sh logs     tail both logs
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"
STATE_DIR="$ROOT/.dev"
LOG_DIR="$STATE_DIR"
MANAGER_BIN="$ROOT/bin/manager"
UI_BIN="$ROOT/bin/ui"
MANAGER_PID="$STATE_DIR/manager.pid"
UI_PID="$STATE_DIR/ui.pid"
API_ADDR=":8482"
UI_ADDR=":8080"

mkdir -p "$STATE_DIR"

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

pid_of() {
  local f="$1"
  [ -f "$f" ] || return 1
  local pid
  pid="$(cat "$f" 2>/dev/null || true)"
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  printf '%s' "$pid"
}

port_busy() {
  # returns 0 when something already listens on the port
  (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null && { exec 3>&-; return 0; }
  return 1
}

build_if_needed() {
  if [ "${WATCHTOWER_SKIP_BUILD:-0}" = "1" ]; then
    return
  fi
  local need=0
  if [ ! -x "$MANAGER_BIN" ] || [ -n "$(find cmd api internal go.mod go.sum -newer "$MANAGER_BIN" -print -quit 2>/dev/null)" ]; then
    need=1
  fi
  if [ ! -x "$UI_BIN" ] || [ -n "$(find ui cmd/ui go.mod go.sum -newer "$UI_BIN" -print -quit 2>/dev/null)" ]; then
    need=1
  fi
  if [ "$need" = 1 ]; then
    if ! command -v go >/dev/null 2>&1; then
      if [ -x "$MANAGER_BIN" ] && [ -x "$UI_BIN" ]; then
        say "warning: go toolchain not found — using existing binaries (may be stale)"
        return
      fi
      die "go toolchain not found and no binaries exist — install Go or use docker compose"
    fi
    say "building binaries…"
    go build -o "$MANAGER_BIN" ./cmd/manager
    go build -o "$UI_BIN" ./cmd/ui
  fi
}

check_cluster() {
  local kc="${KUBECONFIG:-$HOME/.kube/config}"
  if [ -r "$kc" ]; then
    say "kubeconfig: $kc"
  else
    say "warning: no readable kubeconfig at $kc — set KUBECONFIG or the manager cannot reach the cluster"
  fi
  if command -v kubectl >/dev/null 2>&1; then
    local ctx
    ctx="$(kubectl config current-context 2>/dev/null || true)"
    [ -n "$ctx" ] && say "cluster context: $ctx"
  fi
}

start_manager() {
  if pid_of "$MANAGER_PID" >/dev/null; then say "manager already running (pid $(pid_of "$MANAGER_PID"))"; return; fi
  if port_busy 8482; then say "warning: port 8482 already in use — is another manager running?"; return; fi
  nohup "$MANAGER_BIN" --api-bind-address="$API_ADDR" > "$LOG_DIR/manager.log" 2>&1 &
  echo $! > "$MANAGER_PID"
  say "manager started (pid $!), log: .dev/manager.log"
}

start_ui() {
  if pid_of "$UI_PID" >/dev/null; then say "ui already running (pid $(pid_of "$UI_PID"))"; return; fi
  if port_busy 8080; then say "warning: port 8080 already in use — is another ui running?"; return; fi
  nohup "$UI_BIN" -listen="$UI_ADDR" -api="http://localhost:8482" > "$LOG_DIR/ui.log" 2>&1 &
  echo $! > "$UI_PID"
  say "ui started (pid $!), log: .dev/ui.log"
}

wait_healthy() {
  local url="$1" name="$2" i
  for i in $(seq 1 40); do
    if curl -fsS --max-time 1 "$url" >/dev/null 2>&1; then
      say "$name healthy"
      return 0
    fi
    sleep 0.25
  done
  say "warning: $name did not become healthy in time — check .dev logs"
  return 1
}

up() {
  build_if_needed
  check_cluster
  start_manager
  start_ui
  wait_healthy "http://localhost:8481/healthz" "manager" || true
  wait_healthy "http://localhost:8080/healthz" "ui" || true
  say ""
  say "watchtower is up → http://localhost:8080"
}

down() {
  local pid
  for f in "$UI_PID" "$MANAGER_PID"; do
    if pid="$(pid_of "$f")"; then
      kill "$pid" 2>/dev/null || true
      say "stopped pid $pid ($(basename "$f" .pid))"
    fi
    rm -f "$f"
  done
}

status() {
  local pid
  if pid="$(pid_of "$MANAGER_PID")"; then say "manager: running (pid $pid)"; else say "manager: stopped"; fi
  if pid="$(pid_of "$UI_PID")"; then say "ui:      running (pid $pid)"; else say "ui:      stopped"; fi
  curl -fsS --max-time 1 "http://localhost:8482/api/v1/topology" >/dev/null 2>&1 \
    && say "api:     ok" || say "api:     unreachable"
  curl -fsS --max-time 1 "http://localhost:8080/healthz" >/dev/null 2>&1 \
    && say "ui http: ok" || say "ui http: unreachable"
}

logs() {
  touch "$LOG_DIR/manager.log" "$LOG_DIR/ui.log"
  tail -n 40 -f "$LOG_DIR/manager.log" "$LOG_DIR/ui.log"
}

case "${1:-up}" in
  up)      up ;;
  down)    down ;;
  restart) down; sleep 0.5; up ;;
  status)  status ;;
  logs)    logs ;;
  *) die "unknown command: $1 (up|down|restart|status|logs)" ;;
esac
