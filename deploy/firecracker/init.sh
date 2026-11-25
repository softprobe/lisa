#!/usr/bin/env bash
set -u

export PATH="/opt/lisa-venv/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

mount -t proc proc /proc 2>/dev/null || true
mount -t sysfs sysfs /sys 2>/dev/null || true
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mkdir -p /dev/pts /run /tmp /var/log /var/lib/lisa
mount -t devpts devpts /dev/pts 2>/dev/null || true
mount -o remount,rw / 2>/dev/null || true

ip link set lo up 2>/dev/null || true
if [[ -f /etc/lisavm/net.env ]]; then
  set -a
  # shellcheck disable=SC1091
  source /etc/lisavm/net.env
  set +a
  if [[ -n "${GUEST_IP_CIDR:-}" ]]; then
    ip addr replace "${GUEST_IP_CIDR}" dev eth0 2>/dev/null || true
  fi
  ip link set eth0 up 2>/dev/null || true
  if [[ -n "${GUEST_GATEWAY:-}" ]]; then
    ip route replace default via "${GUEST_GATEWAY}" dev eth0 2>/dev/null || true
  fi
else
  ip link set eth0 up 2>/dev/null || true
fi

if [[ ! -s /etc/resolv.conf ]]; then
  printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /etc/resolv.conf
fi

if [[ -x /usr/local/bin/lisavm-guest-bootstrap ]]; then
  /usr/local/bin/lisavm-guest-bootstrap || echo "warning: guest bootstrap failed" >&2
fi

if [[ -f /etc/lisavm/lisa.env ]]; then
  set -a
  # shellcheck disable=SC1091
  source /etc/lisavm/lisa.env
  set +a
fi

export LISA_DATA_DIR="${LISA_DATA_DIR:-/var/lib/lisa}"
export LISA_STATIC_DIR="${LISA_STATIC_DIR:-/app/static}"
export OPENHANDS_SUPPRESS_BANNER=1
export OH_PERSISTENCE_DIR="${OH_PERSISTENCE_DIR:-/var/lib/lisa/openhands}"

count_file="/var/lib/lisa/boot-count"
prev=0
if [[ -f "${count_file}" ]]; then
  prev="$(cat "${count_file}" 2>/dev/null || echo 0)"
fi
echo "$((prev + 1))" > "${count_file}"
sync

vm_halt() {
  sync
  /opt/lisa-venv/bin/python -c 'import ctypes; ctypes.CDLL("libc.so.6").reboot(0x01234567)'
}

LISA_PID=""
shutdown_vm() {
  echo "init: shutting down" > /dev/console 2>/dev/null || true
  if [[ -n "${LISA_PID}" ]]; then
    kill -TERM "${LISA_PID}" 2>/dev/null || true
    wait "${LISA_PID}" 2>/dev/null || true
  fi
  vm_halt
}
trap shutdown_vm INT TERM

cd /app
/opt/lisa-venv/bin/uvicorn lisa.app:app --host 0.0.0.0 --port 8000 >/var/log/lisa.log 2>&1 &
LISA_PID=$!

(
  while kill -0 "${LISA_PID}" 2>/dev/null; do
    if read -r cmd; then
      if [[ "${cmd}" == "shutdown" ]]; then
        kill -INT "$$" 2>/dev/null || true
        break
      fi
    else
      sleep 1
    fi
  done
) </dev/ttyS0 &

while kill -0 "${LISA_PID}" 2>/dev/null; do
  wait "${LISA_PID}" || true
done

vm_halt
