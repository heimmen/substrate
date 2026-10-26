#!/usr/bin/env bash
# Start/stop the temporary mf-pi CI/CD page backend (a host process).
#
#   ./cicd-server.sh start     # start it, then open http://<gateway>/_cicd_
#   ./cicd-server.sh stop
#   ./cicd-server.sh status
#   ./cicd-server.sh logs      # tail the server log (deploy output lives in the page)
#
# The page itself is served by cicd_server.py; mfpi-nginx exposes it at /_cicd_
# behind the same Basic Auth as /usermanagement/ (see nginx.conf). Env forwarded
# to deploy.sh, so e.g. MFPI_SKIP_WORKLOAD_BUILD=1 ./cicd-server.sh start makes
# the button skip the workload image build.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PIDFILE="${TMPDIR:-/tmp}/mfpi-cicd.pid"
LOGFILE="${TMPDIR:-/tmp}/mfpi-cicd.log"
PORT="${CICD_PORT:-58690}"

# pid_of reports the runner's pid. The pidfile is authoritative when it still
# resolves, but a runner started from another shell (or one whose pidfile was
# removed) would otherwise look "not running" while happily serving the page — the
# wrapper then refuses to start (port busy) and cannot stop it. Fall back to
# whoever owns the port.
pid_of() {
  local pid
  if [ -f "$PIDFILE" ]; then
    pid="$(cat "$PIDFILE" 2>/dev/null || true)"
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      printf '%s\n' "$pid"
      return 0
    fi
  fi
  pid="$(ss -ltnp 2>/dev/null | grep -F "127.0.0.1:${PORT} " | grep -oE 'pid=[0-9]+' | head -1 | cut -d= -f2)"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    printf '%s\n' "$pid"
    return 0
  fi
  return 1
}

alive() { pid_of >/dev/null; }

# port_answers is the last resort when the pid cannot be discovered (restricted
# process visibility, a runner started by another account, ...). It still tells the
# truth about "is the page being served", which is what status/start care about.
port_answers() {
  curl -fsS -m 2 "http://127.0.0.1:${PORT}/state" >/dev/null 2>&1
}

case "${1:-start}" in
  start)
    if alive; then
      echo "already running (pid $(pid_of), port ${PORT})"
    elif port_answers; then
      echo "port ${PORT} is already serving this page, but its process is not visible"
      echo "  from this shell (pid unknown). Use it as-is, or find the owner with:"
      echo "  ss -ltnp | grep ${PORT}"
    else
      command -v python3 >/dev/null || { echo "python3 not found" >&2; exit 1; }
      nohup python3 "$DIR/cicd_server.py" >>"$LOGFILE" 2>&1 &
      echo $! >"$PIDFILE"
      # Wait until it actually answers before claiming success.
      for _ in $(seq 1 40); do
        if curl -fsS -m 1 "http://127.0.0.1:${PORT}/state" >/dev/null 2>&1; then
          break
        fi
        sleep 0.25
      done
      if ! curl -fsS -m 1 "http://127.0.0.1:${PORT}/state" >/dev/null 2>&1; then
        echo "failed to start; see $LOGFILE" >&2
        tail -5 "$LOGFILE" >&2 || true
        exit 1
      fi
      echo "started (pid $(pid_of), port ${PORT})"
    fi
    echo "open:  http://127.0.0.1:58681/_cicd_   (prod)  |  http://127.0.0.1:59681/_cicd_  (test)"
    echo "auth:  same credentials as /usermanagement/"
    ;;
  stop)
    if ! alive && port_answers; then
      echo "port ${PORT} is serving the page but its process cannot be identified here."
      echo "  find and stop it manually: ss -ltnp | grep ${PORT}"
    elif pid="$(pid_of)"; then
      kill "$pid" 2>/dev/null || true
      for _ in $(seq 1 20); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.25
      done
      echo "stopped (pid ${pid})"
    else
      echo "not running"
    fi
    rm -f "$PIDFILE"
    ;;
  status)
    if pid="$(pid_of)"; then
      echo "running (pid ${pid}, port ${PORT})"
      curl -fsS -m 2 "http://127.0.0.1:${PORT}/state" || true
      echo
    elif port_answers; then
      echo "running (pid unknown, port ${PORT} answers; see: ss -ltnp | grep ${PORT})"
    else
      echo "not running"
    fi
    ;;
  logs)
    tail -n "${2:-40}" "$LOGFILE"
    ;;
  *)
    echo "usage: $0 {start|stop|status|logs}" >&2
    exit 2
    ;;
esac
