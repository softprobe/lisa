#!/usr/bin/env bash
set -u

export PATH="/opt/lisa-venv/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

mount -t proc proc /proc 2>/dev/null || true
mount -t sysfs sysfs /sys 2>/dev/null || true
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mkdir -p /dev/pts /dev/shm /run /tmp /var/log /var/lib/lisa
mount -t devpts devpts /dev/pts 2>/dev/null || true
mount -t tmpfs -o mode=1777 tmpfs /dev/shm 2>/dev/null || true
mount -o remount,rw / 2>/dev/null || true
chmod 1777 /tmp /dev/shm 2>/dev/null || true

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
export LISA_MCP_SERVERS_PATH="${LISA_MCP_SERVERS_PATH:-/etc/lisavm/plugins/mcp-servers.json}"
export OPENHANDS_SUPPRESS_BANNER=1
export OH_PERSISTENCE_DIR="${OH_PERSISTENCE_DIR:-/var/lib/lisa/openhands}"

if [[ -z "${LISA_WORKSPACE_ID:-}" ]]; then
  ws_file="/var/lib/lisa/workspace-id"
  if [[ ! -s "${ws_file}" ]]; then
    /opt/lisa-venv/bin/python -c 'import uuid; print(uuid.uuid4())' > "${ws_file}"
    chmod 0600 "${ws_file}"
  fi
  LISA_WORKSPACE_ID="$(tr -d '\r\n' < "${ws_file}")"
  export LISA_WORKSPACE_ID
fi

LOCAL_PG_STARTED=0
PG_BIN_DIR=""
PG_DATA="/var/lib/lisa/postgres/data"

start_and_provision_local_postgres() {
  local pg_ctl_bin
  pg_ctl_bin="$(ls /usr/lib/postgresql/*/bin/pg_ctl 2>/dev/null | head -n1 || true)"
  if [[ -z "${pg_ctl_bin}" ]]; then
    echo "error: pg_ctl not found in guest image" >&2
    return 1
  fi
  PG_BIN_DIR="$(dirname "${pg_ctl_bin}")"
  local pg_root="/var/lib/lisa/postgres"
  local pg_creds="${pg_root}/credentials.env"
  local pg_log="/var/log/postgresql.log"

  mkdir -p -m 0700 "${pg_root}"
  mkdir -p -m 0755 /run/postgresql
  chown -R postgres:postgres "${pg_root}" /run/postgresql
  install -m 0600 -o postgres -g postgres /dev/null "${pg_log}"

  if [[ ! -s "${PG_DATA}/PG_VERSION" ]]; then
    su -s /bin/bash postgres -c "${PG_BIN_DIR}/initdb -D '${PG_DATA}' --auth-local=peer --auth-host=scram-sha-256 --encoding=UTF8 --locale=C.UTF-8" >/dev/null
    printf "\nlisten_addresses = '127.0.0.1'\nport = 5432\nunix_socket_directories = '/run/postgresql'\n" >> "${PG_DATA}/postgresql.conf"
  fi

  rm -f "${PG_DATA}/postmaster.pid"
  su -s /bin/bash postgres -c "${PG_BIN_DIR}/pg_ctl -D '${PG_DATA}' -l '${pg_log}' -w start" >/dev/null
  LOCAL_PG_STARTED=1

  if [[ ! -s "${pg_creds}" ]]; then
    (
      umask 077
      /opt/lisa-venv/bin/python - <<'PY' > "${pg_creds}"
import secrets
print(f"WRITER_PW={secrets.token_hex(24)}")
print(f"READER_PW={secrets.token_hex(24)}")
PY
    )
    chmod 0600 "${pg_creds}"
  fi

  local WRITER_PW="" READER_PW=""
  # shellcheck disable=SC1090
  source "${pg_creds}"

  su -s /bin/bash postgres -c \
    "LISA_DATABASE_PROVISION_URL='postgresql://postgres@/lisa?host=/run/postgresql' \
     LISA_WORKSPACE_ID='${LISA_WORKSPACE_ID}' \
     LISA_DATABASE_WRITER_PASSWORD='${WRITER_PW}' \
     LISA_DATABASE_READER_PASSWORD='${READER_PW}' \
     /opt/lisa-venv/bin/python /app/provision_lisa.py"

  export LISA_DATABASE_URL="postgresql://lisa_writer:${WRITER_PW}@127.0.0.1:5432/lisa"
  export LISA_DATABASE_READ_URL="postgresql://lisa_reader:${READER_PW}@127.0.0.1:5432/lisa"
}

if [[ -z "${LISA_DATABASE_URL:-}" ]]; then
  start_and_provision_local_postgres || echo "error: local PostgreSQL bootstrap failed" >&2
elif [[ -n "${LISA_DATABASE_PROVISION_URL:-}" ]]; then
  /opt/lisa-venv/bin/python /app/provision_lisa.py || echo "error: external PostgreSQL provision_lisa.py failed" >&2
  unset LISA_DATABASE_PROVISION_URL LISA_DATABASE_WRITER_PASSWORD LISA_DATABASE_READER_PASSWORD
fi

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
  if [[ "${LOCAL_PG_STARTED}" -eq 1 && -n "${PG_BIN_DIR}" ]]; then
    su -s /bin/bash postgres -c "${PG_BIN_DIR}/pg_ctl -D '${PG_DATA}' -m fast stop" >/dev/null 2>&1 || true
  fi
  vm_halt
}
trap shutdown_vm INT TERM

install -m 0600 /dev/null /var/log/lisa.log
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

echo "init: lisa process exited; last log lines:" > /dev/console 2>/dev/null || true
tail -n 40 /var/log/lisa.log > /dev/console 2>/dev/null || true

if [[ "${LOCAL_PG_STARTED}" -eq 1 && -n "${PG_BIN_DIR}" ]]; then
  su -s /bin/bash postgres -c "${PG_BIN_DIR}/pg_ctl -D '${PG_DATA}' -m fast stop" >/dev/null 2>&1 || true
fi
vm_halt
