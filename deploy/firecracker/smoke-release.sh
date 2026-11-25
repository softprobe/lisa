#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LISAVM_ROOT="${LISAVM_ROOT:-/var/lib/lisavm}"
LISTEN_ADDR="${LISAVM_SMOKE_ADDR:-127.0.0.1:17701}"
BASE_URL="http://${LISTEN_ADDR}"
API_TOKEN="lisavm-smoke-token"

if [[ "${EUID}" -ne 0 ]]; then
  echo "error: smoke-release.sh must run as root (try sudo)" >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64 | amd64) ARCH="amd64" ;;
  aarch64 | arm64) ARCH="arm64" ;;
  *)
    echo "error: unsupported architecture $(uname -m)" >&2
    exit 1
    ;;
esac

RELEASE_DIR="$(mktemp -d)"
STAGE_DIR="$(mktemp -d)"
SMOKE_ROOT="$(mktemp -d)"
LOG_PATH="${SMOKE_ROOT}/lisavm-smoke.log"
LISAVM_PID=""
INSTANCE_ID=""

cleanup() {
  local rc=$?
  if [[ "${rc}" -ne 0 && -f "${LOG_PATH}" ]]; then
    echo "--- lisavm log ---" >&2
    cat "${LOG_PATH}" >&2 || true
  fi
  if [[ -n "${INSTANCE_ID}" ]]; then
    curl -fsS -X DELETE -H "Authorization: Bearer ${API_TOKEN}" \
      "${BASE_URL}/v1/instances/${INSTANCE_ID}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${LISAVM_PID}" ]] && kill -0 "${LISAVM_PID}" 2>/dev/null; then
    kill -TERM "${LISAVM_PID}" 2>/dev/null || true
    wait "${LISAVM_PID}" 2>/dev/null || true
  fi
  rm -rf "${RELEASE_DIR}" "${STAGE_DIR}" "${SMOKE_ROOT}"
}
trap cleanup EXIT

echo "==> 1. Ensuring KVM host and golden rootfs"
"${SCRIPT_DIR}/setup-kvm-host.sh"
if [[ ! -f "${LISAVM_ROOT}/images/rootfs.ext4" ]]; then
  "${SCRIPT_DIR}/build-rootfs.sh"
fi

echo "==> 2. Packaging release artifacts into ${RELEASE_DIR}"
OUT_DIR="${RELEASE_DIR}" TARGET_ARCH="${ARCH}" "${SCRIPT_DIR}/package-release.sh"

echo "==> 3. Verifying SHA256SUMS and extracting staged artifacts"
(cd "${RELEASE_DIR}" && sha256sum -c SHA256SUMS)

install -m 0755 "${RELEASE_DIR}/lisavm-linux-${ARCH}" "${STAGE_DIR}/lisavm"
if [[ -f "${RELEASE_DIR}/rootfs-${ARCH}.ext4.zst" ]]; then
  zstd -d -q -f -o "${STAGE_DIR}/rootfs.ext4" "${RELEASE_DIR}/rootfs-${ARCH}.ext4.zst"
elif [[ -f "${RELEASE_DIR}/rootfs-${ARCH}.ext4.gz" ]]; then
  gzip -dc "${RELEASE_DIR}/rootfs-${ARCH}.ext4.gz" > "${STAGE_DIR}/rootfs.ext4"
else
  echo "error: no compressed rootfs artifact found in ${RELEASE_DIR}" >&2
  exit 1
fi
chmod 0644 "${STAGE_DIR}/rootfs.ext4"

echo "==> 4. Validating host templates"
for tpl in \
  lisavm.service \
  lisavm.env.example \
  Caddyfile.example \
  cloudflared-config.yml.example \
  cloud-init-kvm.yaml \
  README.md; do
  if [[ ! -s "${SCRIPT_DIR}/templates/${tpl}" ]]; then
    echo "error: missing or empty template ${SCRIPT_DIR}/templates/${tpl}" >&2
    exit 1
  fi
done

if command -v systemd-analyze >/dev/null 2>&1; then
  # systemd-analyze verify checks unit syntax; rewrite ExecStart to staged binary for local check.
  sed "s|/usr/local/bin/lisavm|${STAGE_DIR}/lisavm|g" \
    "${SCRIPT_DIR}/templates/lisavm.service" > "${STAGE_DIR}/lisavm.service"
  systemd-analyze verify "${STAGE_DIR}/lisavm.service"
fi

if python3 -c 'import yaml' >/dev/null 2>&1; then
  python3 -c 'import yaml, sys; yaml.safe_load(open(sys.argv[1]))' \
    "${SCRIPT_DIR}/templates/cloud-init-kvm.yaml"
  python3 -c 'import yaml, sys; yaml.safe_load(open(sys.argv[1]))' \
    "${SCRIPT_DIR}/templates/cloudflared-config.yml.example"
fi

echo "==> 5. Starting packaged lisavm binary on ${LISTEN_ADDR}"
mkdir -p "${SMOKE_ROOT}/images" "${SMOKE_ROOT}/instances" "${SMOKE_ROOT}/snapshots"
KERNEL_PATH="${LISAVM_ROOT}/vmlinux"
LISAVM_ROOT="${SMOKE_ROOT}" \
LISAVM_ROOTFS_PATH="${STAGE_DIR}/rootfs.ext4" \
LISAVM_KERNEL_PATH="${KERNEL_PATH}" \
LISAVM_LISTEN_ADDR="${LISTEN_ADDR}" \
LISAVM_API_TOKEN="${API_TOKEN}" \
"${STAGE_DIR}/lisavm" > "${LOG_PATH}" 2>&1 &
LISAVM_PID=$!

for _ in $(seq 1 50); do
  if curl -fsS "${BASE_URL}/health" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
curl -fsS "${BASE_URL}/health" | grep -q '"ok"'

echo "==> 6. Provisioning MicroVM from packaged rootfs (idle_timeout_sec=4)"
create_resp="$(curl -sS -w '\n%{http_code}' -X POST "${BASE_URL}/v1/instances" \
  -H "Authorization: Bearer ${API_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "owner_id": "22222222-2222-2222-2222-222222222222",
    "name": "smoke-release",
    "idle_timeout_sec": 4,
    "env": {
      "LISA_WORKSPACE_ID": "22222222-2222-2222-2222-222222222222",
      "LISA_LOCAL_ANONYMOUS": "1"
    }
  }')"
create_code="$(echo "${create_resp}" | tail -n1)"
create_body="$(echo "${create_resp}" | sed '$d')"
if [[ "${create_code}" -lt 200 || "${create_code}" -ge 300 ]]; then
  echo "error: POST /v1/instances returned HTTP ${create_code}: ${create_body}" >&2
  exit 1
fi
create_resp="${create_body}"
INSTANCE_ID="$(echo "${create_resp}" | jq -r '.id')"
VM_PID="$(echo "${create_resp}" | jq -r '.pid')"
if [[ -z "${INSTANCE_ID}" || "${INSTANCE_ID}" == "null" ]]; then
  echo "error: failed to create instance: ${create_resp}" >&2
  exit 1
fi
echo "provisioned instance ${INSTANCE_ID} (pid=${VM_PID})"

echo "==> 7. Creating conversation and waiting for idle sleep"
conv_1="$(curl -fsS -X POST -H "Authorization: Bearer ${API_TOKEN}" \
  "${BASE_URL}/v1/instances/${INSTANCE_ID}/proxy/v1/conversations" | jq -r '.conversation_id')"
if [[ -z "${conv_1}" || "${conv_1}" == "null" ]]; then
  echo "error: failed to create conversation through proxy" >&2
  exit 1
fi
echo "created conversation ${conv_1}"

slept=0
for _ in $(seq 1 40); do
  inst_json="$(curl -fsS -H "Authorization: Bearer ${API_TOKEN}" "${BASE_URL}/v1/instances/${INSTANCE_ID}")"
  state="$(echo "${inst_json}" | jq -r '.state')"
  if [[ "${state}" == "sleeping" ]]; then
    slept=1
    break
  fi
  sleep 0.5
done
if [[ "${slept}" -ne 1 ]]; then
  echo "error: instance did not enter sleeping state" >&2
  cat "${LOG_PATH}" >&2 || true
  exit 1
fi
if kill -0 "${VM_PID}" 2>/dev/null; then
  echo "error: firecracker process ${VM_PID} still running after sleep" >&2
  exit 1
fi
echo "verified sleep (Firecracker process ${VM_PID} exited)"

echo "==> 8. Waking VM and verifying disk persistence"
list_resp="$(curl -fsS -H "Authorization: Bearer ${API_TOKEN}" \
  "${BASE_URL}/v1/instances/${INSTANCE_ID}/proxy/v1/conversations")"
conv_2="$(echo "${list_resp}" | jq -r '.[0].conversation_id')"
if [[ "${conv_1}" != "${conv_2}" ]]; then
  echo "error: persisted conversation mismatch after wake (${conv_1} != ${conv_2})" >&2
  exit 1
fi
echo "verified wake and disk persistence (${conv_2})"

echo "==> 9. Cleaning up test instance"
curl -fsS -X DELETE -H "Authorization: Bearer ${API_TOKEN}" \
  "${BASE_URL}/v1/instances/${INSTANCE_ID}" >/dev/null
INSTANCE_ID=""
echo "RELEASE SMOKE CHECK PASSED"
