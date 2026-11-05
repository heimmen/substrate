#!/usr/bin/env bash
# Install / run the idle reaper for one or more mf-pi environments.
#
#   ./install-idle-reaper.sh start  [qa]
#   ./install-idle-reaper.sh stop   [qa]
#   ./install-idle-reaper.sh status [qa]
#
# Knobs (environment variables):
#   MFPI_IDLE_REAPER_IDLE   idle threshold (default 12h)
#   MFPI_IDLE_REAPER_MAX    max releases per run (default 5)
#   MFPI_IDLE_REAPER_TIERS  tiers to reap (default small,mid)
#
# With root + systemd: installs mfpi-idle-reaper-<env>.service + hourly timer.
# Otherwise: starts a nohup loop (pidfile) that runs once now and every hour.
# Releasing is snapshot-free: the actor becomes SUSPENDED and wakes cold on the
# next request; data stays on the sticky volume.
set -euo pipefail
cd "$(dirname "$0")"

ACTION="${1:-start}"
case "$ACTION" in
  start|stop|status) shift || true ;;
  *) echo "usage: $0 {start|stop|status} [env ...]" >&2; exit 2 ;;
esac
ENVS=("$@")
[[ ${#ENVS[@]} -eq 0 ]] && ENVS=(qa)

IDLE="${MFPI_IDLE_REAPER_IDLE:-12h}"
MAX="${MFPI_IDLE_REAPER_MAX:-5}"
TIERS="${MFPI_IDLE_REAPER_TIERS:-small,mid}"
LOG_DIR="${MFPI_LOG_DIR:-/tmp/mfpi-logs}"
INTERVAL="${MFPI_IDLE_REAPER_INTERVAL:-3600}"
mkdir -p "$LOG_DIR"

have_systemd() {
  [[ "$(id -u)" = "0" ]] && [[ -d /run/systemd/system ]] && command -v systemctl >/dev/null 2>&1
}

for env in "${ENVS[@]}"; do
  if [[ -f ./lib/env.sh ]]; then
    # shellcheck source=lib/env.sh
    source ./lib/env.sh
    mfpi_load_env "$env"
  fi
  : "${MFPI_ATESPACE:?need MFPI_ATESPACE (run from the demo checkout)}" \
    "${MFPI_NAMESPACE:?need MFPI_NAMESPACE}" \
    "${MFPI_NGINX_CONTAINER:?need MFPI_NGINX_CONTAINER}"

  pidfile="${LOG_DIR}/idle-reaper-${env}.pid"

  if have_systemd; then
    unit="/etc/systemd/system/mfpi-idle-reaper-${env}.service"
    timer="/etc/systemd/system/mfpi-idle-reaper-${env}.timer"
    case "$ACTION" in
      start)
        {
          echo "[Unit]"
          echo "Description=mf-pi idle reaper (${env})"
          echo "After=docker.service"
          echo
          echo "[Service]"
          echo "Type=oneshot"
          echo "WorkingDirectory=${PWD}"
          env | grep -E '^MFPI_' | sed 's/^/Environment=/' || true
          echo "ExecStart=${PWD}/idle-reaper.sh --apply --env ${env} --idle ${IDLE} --max ${MAX} --tiers ${TIERS}"
        } >"$unit"
        {
          echo "[Unit]"
          echo "Description=Run mf-pi idle reaper (${env}) hourly"
          echo
          echo "[Timer]"
          echo "OnBootSec=10m"
          echo "OnUnitActiveSec=1h"
          echo "Unit=mfpi-idle-reaper-${env}.service"
          echo
          echo "[Install]"
          echo "WantedBy=timers.target"
        } >"$timer"
        systemctl daemon-reload
        systemctl enable --now "mfpi-idle-reaper-${env}.timer"
        echo "${env}: installed hourly systemd timer (idle=${IDLE}, max=${MAX}, tiers=${TIERS})"
        ;;
      stop)
        systemctl disable --now "mfpi-idle-reaper-${env}.timer" 2>/dev/null || true
        rm -f "$unit" "$timer"
        systemctl daemon-reload
        echo "${env}: stopped and removed"
        ;;
      status)
        systemctl status --no-pager "mfpi-idle-reaper-${env}.timer" || true
        ;;
    esac
    continue
  fi

  case "$ACTION" in
    start)
      if [[ -f "$pidfile" ]] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
        echo "${env}: already running (pid $(cat "$pidfile"))"
        continue
      fi
      nohup bash -c '
        while true; do
          "./idle-reaper.sh" --apply --env '"$env"' --idle '"$IDLE"' --max '"$MAX"' --tiers '"$TIERS"'
          sleep '"$INTERVAL"'
        done
      ' >/dev/null 2>&1 &
      echo $! >"$pidfile"
      echo "${env}: started (pid $!); no root/systemd, so this does NOT survive reboot (idle=${IDLE}, max=${MAX})"
      ;;
    stop)
      if [[ -f "$pidfile" ]]; then
        kill "$(cat "$pidfile")" 2>/dev/null || true
        rm -f "$pidfile"
        echo "${env}: stopped"
      else
        echo "${env}: not running"
      fi
      ;;
    status)
      if [[ -f "$pidfile" ]] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
        echo "${env}: running (pid $(cat "$pidfile"))"
      else
        echo "${env}: not running"
      fi
      ;;
  esac
done
