#!/usr/bin/env bash
# Install / run the mf-pi watchdog for one or more environments.
#
#   ./install-watchdog.sh start  [qa prod test]
#   ./install-watchdog.sh stop   [qa prod test]
#   ./install-watchdog.sh status [qa prod test]
#
# With root + systemd it installs mfpi-watchdog-<env>.service plus a 60s timer
# that survives reboot. Without root it starts a nohup loop (pidfile) — that
# survives the shell but not a reboot.
#
# In the demo checkout the values come from env/<name>.env. When this script is
# shipped standalone (offline installer), export the MFPI_* variables first:
#   MFPI_ATESPACE MFPI_NAMESPACE MFPI_ROUTER_PORT MFPI_ADMIN_PORT MFPI_NGINX_CONTAINER
set -euo pipefail
cd "$(dirname "$0")"

ACTION="${1:-start}"
case "$ACTION" in
  start|stop|status) shift || true ;;
  *) echo "usage: $0 {start|stop|status} [env ...]" >&2; exit 2 ;;
esac
ENVS=("$@")
[[ ${#ENVS[@]} -eq 0 ]] && ENVS=(qa)

LOG_DIR="${MFPI_LOG_DIR:-/tmp/mfpi-logs}"
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
  : "${MFPI_ATESPACE:?need MFPI_ATESPACE (run from the demo checkout, or export MFPI_*)}" \
    "${MFPI_NAMESPACE:?need MFPI_NAMESPACE}" \
    "${MFPI_ROUTER_PORT:?need MFPI_ROUTER_PORT}" \
    "${MFPI_ADMIN_PORT:?need MFPI_ADMIN_PORT}" \
    "${MFPI_NGINX_CONTAINER:?need MFPI_NGINX_CONTAINER}"

  pidfile="${LOG_DIR}/watchdog-${env}.pid"

  if have_systemd; then
    unit="/etc/systemd/system/mfpi-watchdog-${env}.service"
    timer="/etc/systemd/system/mfpi-watchdog-${env}.timer"
    case "$ACTION" in
      start)
        {
          echo "[Unit]"
          echo "Description=mf-pi watchdog (${env})"
          echo "After=docker.service"
          echo
          echo "[Service]"
          echo "Type=oneshot"
          echo "WorkingDirectory=${PWD}"
          env | grep -E '^MFPI_' | sed 's/^/Environment=/' || true
          echo "ExecStart=${PWD}/watchdog.sh --once --env ${env}"
        } >"$unit"
        {
          echo "[Unit]"
          echo "Description=Run mf-pi watchdog (${env}) every minute"
          echo
          echo "[Timer]"
          echo "OnBootSec=30s"
          echo "OnUnitActiveSec=60s"
          echo "Unit=mfpi-watchdog-${env}.service"
          echo
          echo "[Install]"
          echo "WantedBy=timers.target"
        } >"$timer"
        systemctl daemon-reload
        systemctl enable --now "mfpi-watchdog-${env}.timer"
        echo "${env}: installed systemd timer mfpi-watchdog-${env}.timer"
        ;;
      stop)
        systemctl disable --now "mfpi-watchdog-${env}.timer" 2>/dev/null || true
        rm -f "$unit" "$timer"
        systemctl daemon-reload
        echo "${env}: stopped and removed"
        ;;
      status)
        systemctl status --no-pager "mfpi-watchdog-${env}.timer" || true
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
      # watchdog.sh already logs to ${LOG_DIR}/watchdog-<env>.log.
      nohup ./watchdog.sh --interval 60 --env "$env" >/dev/null 2>&1 &
      echo $! >"$pidfile"
      echo "${env}: started (pid $!); no root/systemd, so this does NOT survive reboot"
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
