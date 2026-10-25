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

alive() {
  local pid
  [ -f "$PIDFILE" ] || return 1
  pid="$(cat "$PIDFILE" 2>/dev/null || true)"
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

case "${1:-start}" in
  start)
    if alive; then
      echo "already running (pid $(cat "$PIDFILE"), port ${PORT})"
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
      echo "started (pid $(cat "$PIDFILE"), port ${PORT})"
    fi
    echo "open:  http://127.0.0.1:58681/_cicd_   (prod)  |  http://127.0.0.1:59681/_cicd_  (test)"
    echo "auth:  same credentials as /usermanagement/"
    ;;
  stop)
    if alive; then
      kill "$(cat "$PIDFILE")" 2>/dev/null || true
      sleep 0.5
      echo "stopped"
    else
      echo "not running"
    fi
    rm -f "$PIDFILE"
    ;;
  status)
    if alive; then
      echo "running (pid $(cat "$PIDFILE"), port ${PORT})"
      curl -fsS -m 2 "http://127.0.0.1:${PORT}/state" || true
      echo
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
