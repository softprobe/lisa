#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LISA_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
LISAVM_DIR="${LISA_ROOT}/lisavm"
LISAVM_ROOT="${LISAVM_ROOT:-/var/lib/lisavm}"
LISTEN_ADDR="${LISAVM_VERIFY_ADDR:-127.0.0.1:17700}"
BASE_URL="http://${LISTEN_ADDR}"
API_TOKEN="lisavm-verify-token"
BIN_PATH="/tmp/lisavm-verify-bin"
LOG_PATH="/tmp/lisavm-verify-daemon.log"
LISAVM_PID=""
INSTANCE_ID=""

if [[ "${EUID}" -ne 0 ]]; then
  echo "error: verify-lisavm.sh must run as root (try sudo)" >&2
  exit 1
fi

cleanup() {
  if [[ -n "${INSTANCE_ID}" ]]; then
    curl -fsS -X DELETE -H "Authorization: Bearer ${API_TOKEN}" \
      "${BASE_URL}/v1/instances/${INSTANCE_ID}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${LISAVM_PID}" ]] && kill -0 "${LISAVM_PID}" 2>/dev/null; then
    kill -TERM "${LISAVM_PID}" 2>/dev/null || true
    wait "${LISAVM_PID}" 2>/dev/null || true
  fi
  rm -f "${BIN_PATH}"
}
trap cleanup EXIT

echo "==> 1. Ensuring KVM host and building lisavm"
"${SCRIPT_DIR}/setup-kvm-host.sh"
if [[ ! -f "${LISAVM_ROOT}/images/rootfs.ext4" ]]; then
  "${SCRIPT_DIR}/build-rootfs.sh"
fi

(cd "${LISAVM_DIR}" && go build -o "${BIN_PATH}" ./cmd/lisavm)

echo "==> 2. Starting lisavm on ${LISTEN_ADDR}"
LISAVM_ROOT="${LISAVM_ROOT}" \
LISAVM_LISTEN_ADDR="${LISTEN_ADDR}" \
LISAVM_API_TOKEN="${API_TOKEN}" \
"${BIN_PATH}" > "${LOG_PATH}" 2>&1 &
LISAVM_PID=$!

for _ in $(seq 1 50); do
  if curl -fsS "${BASE_URL}/health" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
curl -fsS "${BASE_URL}/health" | grep -q '"ok"'

echo "==> 3. Provisioning instance (idle_timeout_sec=4)"
create_resp="$(curl -fsS -X POST "${BASE_URL}/v1/instances" \
  -H "Authorization: Bearer ${API_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "owner_id": "11111111-1111-1111-1111-111111111111",
    "name": "verify-lisavm",
    "idle_timeout_sec": 4,
    "env": {
      "LISA_WORKSPACE_ID": "11111111-1111-1111-1111-111111111111",
      "LISA_LOCAL_ANONYMOUS": "1"
    }
  }')"
INSTANCE_ID="$(echo "${create_resp}" | jq -r '.id')"
VM_PID="$(echo "${create_resp}" | jq -r '.pid')"
if [[ -z "${INSTANCE_ID}" || "${INSTANCE_ID}" == "null" ]]; then
  echo "error: failed to create instance: ${create_resp}" >&2
  exit 1
fi
echo "provisioned instance ${INSTANCE_ID} (pid=${VM_PID})"

echo "==> 4. Creating conversation through wake proxy"
conv_1="$(curl -fsS -X POST -H "Authorization: Bearer ${API_TOKEN}" \
  "${BASE_URL}/v1/instances/${INSTANCE_ID}/proxy/v1/conversations" | jq -r '.conversation_id')"
if [[ -z "${conv_1}" || "${conv_1}" == "null" ]]; then
  echo "error: failed to create conversation through proxy" >&2
  exit 1
fi
echo "created conversation ${conv_1}"

echo "==> 5. Waiting for idle sleep"
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
  echo "error: firecracker process ${VM_PID} is still running after sleep" >&2
  exit 1
fi
echo "verified instance is sleeping and Firecracker process ${VM_PID} exited"

echo "==> 6. Waking instance transparently via HTTP proxy"
list_resp="$(curl -fsS -H "Authorization: Bearer ${API_TOKEN}" \
  "${BASE_URL}/v1/instances/${INSTANCE_ID}/proxy/v1/conversations")"
conv_2="$(echo "${list_resp}" | jq -r '.[0].conversation_id')"
if [[ "${conv_1}" != "${conv_2}" ]]; then
  echo "error: persisted conversation mismatch after wake (${conv_1} != ${conv_2})" >&2
  exit 1
fi
echo "verified snapshot wake and persisted conversation ${conv_2}"

echo "==> 7. Verifying scheduled job wakes sleeping VM and executes"
curl -fsS -X PATCH -H "Authorization: Bearer ${API_TOKEN}" \
  -H "Content-Type: application/json" \
  "${BASE_URL}/v1/instances/${INSTANCE_ID}" \
  -d '{
    "schedules": [
      {
        "id": "heartbeat",
        "cron": "@every 5s",
        "method": "POST",
        "path": "/v1/conversations",
        "enabled": true
      }
    ]
  }' >/dev/null

curl -fsS -X POST -H "Authorization: Bearer ${API_TOKEN}" \
  "${BASE_URL}/v1/instances/${INSTANCE_ID}/sleep" >/dev/null

woken_by_cron=0
for _ in $(seq 1 30); do
  inst_json="$(curl -fsS -H "Authorization: Bearer ${API_TOKEN}" "${BASE_URL}/v1/instances/${INSTANCE_ID}")"
  state="$(echo "${inst_json}" | jq -r '.state')"
  if [[ "${state}" == "running" ]]; then
    woken_by_cron=1
    break
  fi
  sleep 0.5
done
if [[ "${woken_by_cron}" -ne 1 ]]; then
  echo "error: scheduled job did not wake sleeping VM" >&2
  cat "${LOG_PATH}" >&2 || true
  exit 1
fi

last_run=""
for _ in $(seq 1 20); do
  inst_json="$(curl -fsS -H "Authorization: Bearer ${API_TOKEN}" "${BASE_URL}/v1/instances/${INSTANCE_ID}")"
  last_run="$(echo "${inst_json}" | jq -r '.schedules[0].last_run_at // ""')"
  if [[ -n "${last_run}" && "${last_run}" != "null" ]] && grep -q "schedule heartbeat for ${INSTANCE_ID} -> 200" "${LOG_PATH}"; then
    break
  fi
  sleep 0.5
done
if ! grep -q "schedule heartbeat for ${INSTANCE_ID} -> 200" "${LOG_PATH}"; then
  echo "error: expected scheduled job HTTP 200 log in ${LOG_PATH}" >&2
  cat "${LOG_PATH}" >&2 || true
  exit 1
fi
echo "verified scheduled job woke VM and executed (last_run_at=${last_run})"

echo "==> 8. Cleaning up test instance"
curl -fsS -X DELETE -H "Authorization: Bearer ${API_TOKEN}" \
  "${BASE_URL}/v1/instances/${INSTANCE_ID}" >/dev/null
INSTANCE_ID=""
echo "ALL LISAVM CHECKS PASSED"
