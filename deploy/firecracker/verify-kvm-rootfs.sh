#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LISAVM_ROOT="${LISAVM_ROOT:-/var/lib/lisavm}"
BRIDGE="${LISAVM_BRIDGE:-lisabr0}"
TAP_DEV="${LISAVM_VERIFY_TAP:-lisatap-verify}"
VM_IP="172.16.0.10"
GW_IP="172.16.0.1"
JAIL_ID="verify-kvm-rootfs"
JAIL_UID=200010
JAIL_GID=200010
CHROOT_BASE="${LISAVM_ROOT}/jailer"
JAIL_INST_DIR="${CHROOT_BASE}/firecracker/${JAIL_ID}"
JAIL_ROOT="${JAIL_INST_DIR}/root"
INSTANCE_DIR="${LISAVM_ROOT}/instances/verify-test"
DISK_PATH="${INSTANCE_DIR}/disk.ext4"
SOCK_PATH="${JAIL_ROOT}/firecracker.sock"
FC_LOG="/tmp/lisavm-verify.log"
FC_STDIN="/tmp/lisavm-verify.stdin"
FC_PID=""

if [[ "${EUID}" -ne 0 ]]; then
  echo "error: verify-kvm-rootfs.sh must run as root (try sudo)" >&2
  exit 1
fi

cleanup() {
  if [[ -n "${FC_PID}" ]] && kill -0 "${FC_PID}" 2>/dev/null; then
    kill -KILL "${FC_PID}" 2>/dev/null || true
    wait "${FC_PID}" 2>/dev/null || true
  fi
  ip link del "${TAP_DEV}" >/dev/null 2>&1 || true
  rm -f "${FC_STDIN}" "${FC_LOG}"
  rm -rf "${JAIL_INST_DIR}" "${INSTANCE_DIR}"
  rmdir "/sys/fs/cgroup/firecracker/${JAIL_ID}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> 1. Running setup-kvm-host.sh (pass 1 and idempotent pass 2)"
"${SCRIPT_DIR}/setup-kvm-host.sh"
"${SCRIPT_DIR}/setup-kvm-host.sh"

echo "==> 2. Ensuring golden rootfs.ext4"
if [[ ! -f "${LISAVM_ROOT}/images/rootfs.ext4" ]]; then
  "${SCRIPT_DIR}/build-rootfs.sh"
fi

echo "==> 3. Cloning persistent disk.ext4"
"${SCRIPT_DIR}/clone-disk.sh" "${DISK_PATH}"

echo "==> 4. Injecting generic bootstrap config into cloned disk.ext4"
MNT_DIR="$(mktemp -d)"
mount -o loop "${DISK_PATH}" "${MNT_DIR}"
mkdir -p -m 0700 "${MNT_DIR}/etc/lisavm"
chmod 0700 "${MNT_DIR}/etc/lisavm"
(
  umask 077
  cat > "${MNT_DIR}/etc/lisavm/bootstrap.json" <<'JSON'
{
  "env": {
    "LISA_WORKSPACE_ID": "11111111-1111-1111-1111-111111111111",
    "LISA_LOCAL_ANONYMOUS": "1"
  },
  "plugins": {
    "example": {
      "transport": "stdio",
      "command": "echo",
      "enabled": false
    }
  }
}
JSON
)
chmod 0600 "${MNT_DIR}/etc/lisavm/bootstrap.json"
sync
umount "${MNT_DIR}"
rmdir "${MNT_DIR}"

echo "==> 5. Creating UID-owned TAP device ${TAP_DEV} on ${BRIDGE}"
ip link del "${TAP_DEV}" >/dev/null 2>&1 || true
ip tuntap add dev "${TAP_DEV}" mode tap user "${JAIL_UID}" group "${JAIL_GID}"
ip link set "${TAP_DEV}" master "${BRIDGE}"
ip link set "${TAP_DEV}" up

boot_vm() {
  rm -rf "${JAIL_INST_DIR}"
  rmdir "/sys/fs/cgroup/firecracker/${JAIL_ID}" >/dev/null 2>&1 || true
  rm -f "${FC_LOG}" "${FC_STDIN}"
  install -m 0600 /dev/null "${FC_LOG}"

  mkdir -p -m 0700 "${JAIL_ROOT}"
  chown "${JAIL_UID}:${JAIL_GID}" "${JAIL_ROOT}"
  chmod 0600 "${DISK_PATH}"
  chown "${JAIL_UID}:${JAIL_GID}" "${DISK_PATH}"
  ln "${DISK_PATH}" "${JAIL_ROOT}/disk.ext4"
  cp "${LISAVM_ROOT}/vmlinux" "${JAIL_ROOT}/vmlinux"
  chmod 0444 "${JAIL_ROOT}/vmlinux"

  mkfifo "${FC_STDIN}"
  jailer \
    --id "${JAIL_ID}" \
    --exec-file "$(command -v firecracker)" \
    --uid "${JAIL_UID}" \
    --gid "${JAIL_GID}" \
    --chroot-base-dir "${CHROOT_BASE}" \
    --cgroup-version 2 \
    -- --api-sock /firecracker.sock < "${FC_STDIN}" > "${FC_LOG}" 2>&1 &
  FC_PID=$!
  exec 3>"${FC_STDIN}"

  local i
  for i in $(seq 1 50); do
    if [[ -S "${SOCK_PATH}" ]]; then
      break
    fi
    sleep 0.1
  done
  if [[ ! -S "${SOCK_PATH}" ]]; then
    echo "error: jailed firecracker API socket did not appear" >&2
    cat "${FC_LOG}" >&2 || true
    exit 1
  fi

  local fc_uid
  fc_uid="$(ps -o uid= -p "${FC_PID}" | tr -d ' ')"
  if [[ "${fc_uid}" != "${JAIL_UID}" ]]; then
    echo "error: expected jailed firecracker UID ${JAIL_UID}, got ${fc_uid}" >&2
    exit 1
  fi

  curl -fsS --unix-socket "${SOCK_PATH}" -X PUT "http://localhost/machine-config" \
    -H "Content-Type: application/json" \
    -d '{"vcpu_count": 1, "mem_size_mib": 1024}'

  curl -fsS --unix-socket "${SOCK_PATH}" -X PUT "http://localhost/boot-source" \
    -H "Content-Type: application/json" \
    -d "{\"kernel_image_path\": \"/vmlinux\", \"boot_args\": \"console=ttyS0 reboot=k panic=1 pci=off ip=${VM_IP}::${GW_IP}:255.255.0.0:lisavm-verify:eth0:off\"}"

  curl -fsS --unix-socket "${SOCK_PATH}" -X PUT "http://localhost/drives/rootfs" \
    -H "Content-Type: application/json" \
    -d '{"drive_id": "rootfs", "path_on_host": "/disk.ext4", "is_root_device": true, "is_read_only": false}'

  curl -fsS --unix-socket "${SOCK_PATH}" -X PUT "http://localhost/network-interfaces/eth0" \
    -H "Content-Type: application/json" \
    -d "{\"iface_id\": \"eth0\", \"guest_mac\": \"06:00:AC:10:00:0A\", \"host_dev_name\": \"${TAP_DEV}\"}"

  curl -fsS --unix-socket "${SOCK_PATH}" -X PUT "http://localhost/actions" \
    -H "Content-Type: application/json" \
    -d '{"action_type": "InstanceStart"}'

  local health=""
  for i in $(seq 1 240); do
    if health="$(curl -fsS --max-time 2 "http://${VM_IP}:8000/health?require_db=1" 2>/dev/null)"; then
      if echo "${health}" | jq -e '.ok == true and .database_configured == true and .database_ready == true' >/dev/null 2>&1; then
        echo "VM healthy with provisioned Postgres: ${health}"
        return 0
      fi
    fi
    sleep 0.25
  done
  echo "error: VM database-backed health endpoint did not become ready" >&2
  cat "${FC_LOG}" >&2 || true
  exit 1
}

shutdown_vm() {
  printf 'shutdown\n' >&3 || true
  exec 3>&- || true

  local i
  for i in $(seq 1 80); do
    if ! kill -0 "${FC_PID}" 2>/dev/null; then
      wait "${FC_PID}" 2>/dev/null || true
      FC_PID=""
      chown 0:0 "${DISK_PATH}"
      chmod 0600 "${DISK_PATH}"
      rm -rf "${JAIL_INST_DIR}"
      rmdir "/sys/fs/cgroup/firecracker/${JAIL_ID}" >/dev/null 2>&1 || true
      return 0
    fi
    sleep 0.25
  done
  echo "error: jailed firecracker VM did not exit cleanly after shutdown" >&2
  cat "${FC_LOG}" >&2 || true
  exit 1
}

echo "==> 6. First boot: verify /health?require_db=1, frontend assets, and create persisted conversation"
boot_vm
index_html="$(curl -fsS "http://${VM_IP}:8000/")"
if ! grep -q '<div id="root"></div>' <<<"${index_html}"; then
  echo "error: guest root / did not serve built Lisa frontend HTML" >&2
  exit 1
fi
asset_rel="$(grep -oE '/assets/[^"'"'"' >]+\.js' <<<"${index_html}" | head -n1 || true)"
if [[ -z "${asset_rel}" ]]; then
  echo "error: no built JS asset referenced in guest index.html" >&2
  exit 1
fi
asset_bytes="$(curl -fsS "http://${VM_IP}:8000${asset_rel}" | wc -c | tr -d ' ')"
if [[ "${asset_bytes}" -lt 100 ]]; then
  echo "error: static JS asset ${asset_rel} looked empty (${asset_bytes} bytes)" >&2
  exit 1
fi
echo "verified frontend HTML and static bundle ${asset_rel} (${asset_bytes} bytes)"

conv_1="$(curl -fsS -X POST "http://${VM_IP}:8000/v1/conversations" | jq -r '.conversation_id')"
if [[ -z "${conv_1}" || "${conv_1}" == "null" ]]; then
  echo "error: failed to create conversation on first boot" >&2
  exit 1
fi
echo "created conversation on boot 1: ${conv_1}"
shutdown_vm

echo "==> 7. Second boot from same disk.ext4: verify persisted state"
boot_vm
conv_2="$(curl -fsS "http://${VM_IP}:8000/v1/conversations" | jq -r '.[0].conversation_id')"
if [[ "${conv_1}" != "${conv_2}" ]]; then
  echo "error: conversation_id did not persist across reboot (${conv_1} != ${conv_2})" >&2
  exit 1
fi
echo "verified persisted conversation on boot 2: ${conv_2}"
shutdown_vm

boot_count="$(debugfs -R "cat /var/lib/lisa/boot-count" "${DISK_PATH}" 2>/dev/null | tr -d '\r\n')"
if [[ "${boot_count}" != "2" ]]; then
  echo "error: expected boot-count=2 on disk.ext4, got '${boot_count}'" >&2
  exit 1
fi
env_stat="$(debugfs -R "stat /etc/lisavm/lisa.env" "${DISK_PATH}" 2>/dev/null)"
if ! grep -q "Mode:  0600" <<<"${env_stat}"; then
  echo "error: expected /etc/lisavm/lisa.env mode 0600, got: ${env_stat}" >&2
  exit 1
fi
echo "verified boot-count=${boot_count} and restrictive guest file modes on disk.ext4"
echo "ALL CHECKS PASSED"
