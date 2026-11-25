#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LISAVM_ROOT="${LISAVM_ROOT:-/var/lib/lisavm}"
BRIDGE="${LISAVM_BRIDGE:-lisabr0}"
TAP_DEV="${LISAVM_VERIFY_TAP:-lisatap-verify}"
VM_IP="172.16.0.10"
GW_IP="172.16.0.1"
INSTANCE_DIR="${LISAVM_ROOT}/instances/verify-test"
DISK_PATH="${INSTANCE_DIR}/disk.ext4"
SOCK_PATH="/tmp/lisavm-verify.sock"
FC_LOG="/tmp/lisavm-verify.log"
FC_STDIN="/tmp/lisavm-verify.stdin"
FC_PID=""
FC_STDIN_FD=""

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
  rm -f "${SOCK_PATH}" "${FC_STDIN}"
  rm -rf "${INSTANCE_DIR}"
}
trap cleanup EXIT

echo "==> 1. Running setup-kvm-host.sh (pass 1 and idempotent pass 2)"
"${SCRIPT_DIR}/setup-kvm-host.sh"
"${SCRIPT_DIR}/setup-kvm-host.sh"

echo "==> 2. Building golden rootfs.ext4"
"${SCRIPT_DIR}/build-rootfs.sh"

echo "==> 3. Cloning persistent disk.ext4"
"${SCRIPT_DIR}/clone-disk.sh" "${DISK_PATH}"

echo "==> 4. Injecting generic bootstrap config into cloned disk.ext4"
MNT_DIR="$(mktemp -d)"
mount -o loop "${DISK_PATH}" "${MNT_DIR}"
mkdir -p "${MNT_DIR}/etc/lisavm"
cat > "${MNT_DIR}/etc/lisavm/bootstrap.json" <<'JSON'
{
  "env": {
    "LISA_WORKSPACE_ID": "11111111-1111-1111-1111-111111111111",
    "LISA_LOCAL_ANONYMOUS": "1"
  },
  "plugins": {
    "example": {
      "transport": "stdio",
      "command": "echo"
    }
  }
}
JSON
sync
umount "${MNT_DIR}"
rmdir "${MNT_DIR}"

echo "==> 5. Creating TAP device ${TAP_DEV} on ${BRIDGE}"
ip link del "${TAP_DEV}" >/dev/null 2>&1 || true
ip tuntap add dev "${TAP_DEV}" mode tap
ip link set "${TAP_DEV}" master "${BRIDGE}"
ip link set "${TAP_DEV}" up

boot_vm() {
  rm -f "${SOCK_PATH}" "${FC_LOG}" "${FC_STDIN}"
  mkfifo "${FC_STDIN}"
  firecracker --api-sock "${SOCK_PATH}" < "${FC_STDIN}" > "${FC_LOG}" 2>&1 &
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
    echo "error: firecracker API socket did not appear" >&2
    cat "${FC_LOG}" >&2 || true
    exit 1
  fi

  curl -fsS --unix-socket "${SOCK_PATH}" -X PUT "http://localhost/machine-config" \
    -H "Content-Type: application/json" \
    -d '{"vcpu_count": 1, "mem_size_mib": 512}'

  curl -fsS --unix-socket "${SOCK_PATH}" -X PUT "http://localhost/boot-source" \
    -H "Content-Type: application/json" \
    -d "{\"kernel_image_path\": \"${LISAVM_ROOT}/vmlinux\", \"boot_args\": \"console=ttyS0 reboot=k panic=1 pci=off ip=${VM_IP}::${GW_IP}:255.255.0.0:lisavm-verify:eth0:off\"}"

  curl -fsS --unix-socket "${SOCK_PATH}" -X PUT "http://localhost/drives/rootfs" \
    -H "Content-Type: application/json" \
    -d "{\"drive_id\": \"rootfs\", \"path_on_host\": \"${DISK_PATH}\", \"is_root_device\": true, \"is_read_only\": false}"

  curl -fsS --unix-socket "${SOCK_PATH}" -X PUT "http://localhost/network-interfaces/eth0" \
    -H "Content-Type: application/json" \
    -d "{\"iface_id\": \"eth0\", \"guest_mac\": \"06:00:AC:10:00:0A\", \"host_dev_name\": \"${TAP_DEV}\"}"

  curl -fsS --unix-socket "${SOCK_PATH}" -X PUT "http://localhost/actions" \
    -H "Content-Type: application/json" \
    -d '{"action_type": "InstanceStart"}'

  local health=""
  for i in $(seq 1 120); do
    if health="$(curl -fsS --max-time 2 "http://${VM_IP}:8000/health" 2>/dev/null)"; then
      echo "VM healthy: ${health}"
      return 0
    fi
    sleep 0.25
  done
  echo "error: VM health endpoint did not become ready" >&2
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
      return 0
    fi
    sleep 0.25
  done
  echo "error: firecracker VM did not exit cleanly after shutdown" >&2
  cat "${FC_LOG}" >&2 || true
  exit 1
}

echo "==> 6. First boot: verify /health and create persisted conversation"
boot_vm
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
echo "verified boot-count=${boot_count} persisted on disk.ext4"
echo "ALL CHECKS PASSED"
