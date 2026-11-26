#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.10.1}"
KERNEL_VERSION="${KERNEL_VERSION:-6.1.102}"
LISAVM_ROOT="${LISAVM_ROOT:-/var/lib/lisavm}"
LISAVM_BRIDGE="${LISAVM_BRIDGE:-lisabr0}"
LISAVM_BRIDGE_CIDR="${LISAVM_BRIDGE_CIDR:-172.16.0.1/16}"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"
SYSTEMD_DIR="${SYSTEMD_DIR:-/etc/systemd/system}"

if [[ "${EUID}" -ne 0 ]]; then
  echo "error: setup-kvm-host.sh must run as root (try sudo)" >&2
  exit 1
fi

if [[ ! -c /dev/kvm ]]; then
  echo "error: /dev/kvm is missing; enable KVM virtualization on this host" >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64) ARCH="x86_64" ;;
  *)
    echo "error: unsupported architecture $(uname -m) (only x86_64/amd64 is supported)" >&2
    exit 1
    ;;
esac

mkdir -p "${LISAVM_ROOT}/images"
mkdir -p -m 0700 "${LISAVM_ROOT}/instances" "${LISAVM_ROOT}/snapshots" "${LISAVM_ROOT}/jailer"
chmod 0700 "${LISAVM_ROOT}/instances" "${LISAVM_ROOT}/snapshots" "${LISAVM_ROOT}/jailer"

install_firecracker() {
  local expected_ver="${FIRECRACKER_VERSION#v}"
  if [[ -x "${BIN_DIR}/firecracker" && -x "${BIN_DIR}/jailer" ]]; then
    if "${BIN_DIR}/firecracker" --version 2>/dev/null | grep -q "${expected_ver}" && \
       "${BIN_DIR}/jailer" --version 2>/dev/null | grep -q "${expected_ver}"; then
      echo "firecracker ${FIRECRACKER_VERSION} already installed in ${BIN_DIR}"
      return 0
    fi
  fi

  local tmpdir
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir}"' RETURN

  local tgz="firecracker-${FIRECRACKER_VERSION}-${ARCH}.tgz"
  local base_url="https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}"

  echo "downloading ${tgz}..."
  curl -fsSL "${base_url}/${tgz}" -o "${tmpdir}/${tgz}"
  curl -fsSL "${base_url}/${tgz}.sha256.txt" -o "${tmpdir}/${tgz}.sha256.txt"
  (cd "${tmpdir}" && sha256sum -c "${tgz}.sha256.txt")

  tar -xzf "${tmpdir}/${tgz}" -C "${tmpdir}"
  local release_dir="${tmpdir}/release-${FIRECRACKER_VERSION}-${ARCH}"
  install -m 0755 "${release_dir}/firecracker-${FIRECRACKER_VERSION}-${ARCH}" "${BIN_DIR}/firecracker"
  install -m 0755 "${release_dir}/jailer-${FIRECRACKER_VERSION}-${ARCH}" "${BIN_DIR}/jailer"
  echo "installed firecracker and jailer ${FIRECRACKER_VERSION} to ${BIN_DIR}"
}

install_kernel() {
  local kernel_path="${LISAVM_ROOT}/vmlinux"
  local marker_path="${LISAVM_ROOT}/.vmlinux-version"
  local desired="${ARCH}-${KERNEL_VERSION}"

  if [[ -s "${kernel_path}" && -f "${marker_path}" ]] && [[ "$(cat "${marker_path}")" == "${desired}" ]]; then
    echo "kernel ${desired} already present at ${kernel_path}"
    return 0
  fi

  local fc_series="${FIRECRACKER_VERSION%.*}"
  local url="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/${fc_series}/${ARCH}/vmlinux-${KERNEL_VERSION}"
  local tmp_kernel
  tmp_kernel="$(mktemp "${LISAVM_ROOT}/.vmlinux.XXXXXX")"

  echo "downloading guest kernel from ${url}..."
  curl -fsSL "${url}" -o "${tmp_kernel}"
  chmod 0644 "${tmp_kernel}"
  mv "${tmp_kernel}" "${kernel_path}"
  printf '%s\n' "${desired}" > "${marker_path}"
  echo "installed guest kernel to ${kernel_path}"
}

configure_persistent_networking() {
  local net_script_src=""
  if [[ -f "${SCRIPT_DIR}/lisavm-network.sh" ]]; then
    net_script_src="${SCRIPT_DIR}/lisavm-network.sh"
  else
    echo "error: missing ${SCRIPT_DIR}/lisavm-network.sh alongside setup-kvm-host.sh" >&2
    exit 1
  fi
  install -m 0755 "${net_script_src}" "${BIN_DIR}/lisavm-network"

  local net_unit_src=""
  if [[ -f "${SCRIPT_DIR}/templates/lisavm-network.service" ]]; then
    net_unit_src="${SCRIPT_DIR}/templates/lisavm-network.service"
  elif [[ -f "${SCRIPT_DIR}/lisavm-network.service" ]]; then
    net_unit_src="${SCRIPT_DIR}/lisavm-network.service"
  else
    echo "error: missing lisavm-network.service alongside setup-kvm-host.sh" >&2
    exit 1
  fi

  if [[ -d "${SYSTEMD_DIR}" ]]; then
    install -m 0644 "${net_unit_src}" "${SYSTEMD_DIR}/lisavm-network.service"
  fi

  LISAVM_BRIDGE="${LISAVM_BRIDGE}" LISAVM_BRIDGE_CIDR="${LISAVM_BRIDGE_CIDR}" "${BIN_DIR}/lisavm-network"

  if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
    systemctl daemon-reload
    systemctl enable lisavm-network.service >/dev/null
  fi
}

install_firecracker
install_kernel
configure_persistent_networking
echo "kvm host ready: ${LISAVM_ROOT}"
