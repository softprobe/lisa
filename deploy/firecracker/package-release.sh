#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LISA_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
LISAVM_DIR="${LISA_ROOT}/lisavm"
LISAVM_ROOT="${LISAVM_ROOT:-/var/lib/lisavm}"

OUT_DIR="${1:-${OUT_DIR:-${SCRIPT_DIR}/dist}}"
ROOTFS_PATH="${ROOTFS_PATH:-${LISAVM_ROOT}/images/rootfs.ext4}"
SKIP_ROOTFS="${SKIP_ROOTFS:-0}"

host_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "amd64" ;;
    *)
      echo "error: unsupported host architecture $(uname -m) (only amd64 is supported)" >&2
      exit 1
      ;;
  esac
}

TARGET_ARCH="${TARGET_ARCH:-$(host_arch)}"

if [[ -n "${COMPRESSOR:-}" ]]; then
  case "${COMPRESSOR}" in
    zstd | gzip) ;;
    *)
      echo "error: unsupported COMPRESSOR=${COMPRESSOR} (expected zstd or gzip)" >&2
      exit 1
      ;;
  esac
elif command -v zstd >/dev/null 2>&1; then
  COMPRESSOR="zstd"
elif command -v gzip >/dev/null 2>&1; then
  COMPRESSOR="gzip"
else
  echo "error: neither zstd nor gzip is installed" >&2
  exit 1
fi

mkdir -p "${OUT_DIR}"

build_binary() {
  local arch="$1"
  local out_bin="${OUT_DIR}/lisavm-linux-${arch}"
  echo "building ${out_bin} (CGO_ENABLED=0 GOOS=linux GOARCH=${arch})..."
  (
    cd "${LISAVM_DIR}"
    CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" \
      go build -trimpath -ldflags="-s -w" -o "${out_bin}" ./cmd/lisavm
  )
  chmod 0755 "${out_bin}"
}

package_rootfs() {
  local arch="$1"
  if [[ ! -f "${ROOTFS_PATH}" ]]; then
    echo "error: rootfs image not found at ${ROOTFS_PATH} (run sudo ${SCRIPT_DIR}/build-rootfs.sh first, or set SKIP_ROOTFS=1)" >&2
    exit 1
  fi

  case "${COMPRESSOR}" in
    zstd)
      local out_rootfs="${OUT_DIR}/rootfs-${arch}.ext4.zst"
      echo "compressing ${ROOTFS_PATH} -> ${out_rootfs} (zstd)..."
      zstd -T0 -q -f -o "${out_rootfs}" "${ROOTFS_PATH}"
      ;;
    gzip)
      local out_rootfs="${OUT_DIR}/rootfs-${arch}.ext4.gz"
      echo "compressing ${ROOTFS_PATH} -> ${out_rootfs} (gzip)..."
      gzip -c "${ROOTFS_PATH}" > "${out_rootfs}"
      ;;
  esac
}

case "${TARGET_ARCH}" in
  amd64)
    build_binary "amd64"
    if [[ "${SKIP_ROOTFS}" != "1" ]]; then
      package_rootfs "amd64"
    fi
    ;;
  all)
    build_binary "amd64"
    if [[ "${SKIP_ROOTFS}" != "1" ]]; then
      package_rootfs "amd64"
    fi
    ;;
  *)
    echo "error: unsupported TARGET_ARCH=${TARGET_ARCH} (only amd64 is supported)" >&2
    exit 1
    ;;
esac

cp "${SCRIPT_DIR}/setup-kvm-host.sh" "${OUT_DIR}/setup-kvm-host.sh"
cp "${SCRIPT_DIR}/lisavm-network.sh" "${OUT_DIR}/lisavm-network.sh"
cp "${SCRIPT_DIR}/templates/lisavm-network.service" "${OUT_DIR}/lisavm-network.service"
chmod 0755 "${OUT_DIR}/setup-kvm-host.sh" "${OUT_DIR}/lisavm-network.sh"
chmod 0644 "${OUT_DIR}/lisavm-network.service"

(
  cd "${OUT_DIR}"
  rm -f SHA256SUMS
  find . -maxdepth 1 -type f ! -name 'SHA256SUMS' -printf '%f\n' | sort | xargs sha256sum > SHA256SUMS
)

echo "packaged release artifacts in ${OUT_DIR}:"
ls -lh "${OUT_DIR}"
