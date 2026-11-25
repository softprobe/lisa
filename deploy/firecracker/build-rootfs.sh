#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LISA_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
LISAVM_ROOT="${LISAVM_ROOT:-/var/lib/lisavm}"
ROOTFS_OUT="${ROOTFS_OUT:-${LISAVM_ROOT}/images/rootfs.ext4}"
ROOTFS_SIZE_MB="${ROOTFS_SIZE_MB:-2048}"
IMAGE_TAG="${IMAGE_TAG:-lisavm-rootfs:latest}"

if [[ "${EUID}" -ne 0 ]]; then
  echo "error: build-rootfs.sh must run as root (try sudo)" >&2
  exit 1
fi

mkdir -p "$(dirname "${ROOTFS_OUT}")"

echo "building rootfs container image ${IMAGE_TAG}..."
docker build \
  -f "${SCRIPT_DIR}/Dockerfile.rootfs" \
  -t "${IMAGE_TAG}" \
  "${LISA_ROOT}"

tmpdir="$(mktemp -d)"
container_id=""
cleanup() {
  if [[ -n "${container_id}" ]]; then
    docker rm -f "${container_id}" >/dev/null 2>&1 || true
  fi
  rm -rf "${tmpdir}"
}
trap cleanup EXIT

container_id="$(docker create "${IMAGE_TAG}" /sbin/init)"
mkdir -p "${tmpdir}/rootfs"
docker export "${container_id}" | tar -xf - -C "${tmpdir}/rootfs"

tmp_img="$(mktemp "${ROOTFS_OUT}.XXXXXX")"
rm -f "${tmp_img}"
truncate -s "${ROOTFS_SIZE_MB}M" "${tmp_img}"
mkfs.ext4 -F -d "${tmpdir}/rootfs" "${tmp_img}" >/dev/null
chmod 0644 "${tmp_img}"
mv "${tmp_img}" "${ROOTFS_OUT}"

echo "built golden rootfs at ${ROOTFS_OUT} (${ROOTFS_SIZE_MB}M)"
