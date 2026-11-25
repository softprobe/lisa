#!/usr/bin/env bash
set -euo pipefail

FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.10.1}"
KERNEL_VERSION="${KERNEL_VERSION:-6.1.102}"
LISAVM_ROOT="${LISAVM_ROOT:-/var/lib/lisavm}"
LISAVM_BRIDGE="${LISAVM_BRIDGE:-lisabr0}"
LISAVM_BRIDGE_CIDR="${LISAVM_BRIDGE_CIDR:-172.16.0.1/16}"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"

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
  aarch64 | arm64) ARCH="aarch64" ;;
  *)
    echo "error: unsupported architecture $(uname -m)" >&2
    exit 1
    ;;
esac

mkdir -p "${LISAVM_ROOT}/images" "${LISAVM_ROOT}/instances" "${LISAVM_ROOT}/snapshots"

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

  local mm_ver="${KERNEL_VERSION%.*}"
  if [[ "${mm_ver}" == *.* ]]; then
    # Use major.minor for Firecracker CI S3 path (e.g., v1.10).
    :
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

configure_networking() {
  if ! ip link show "${LISAVM_BRIDGE}" >/dev/null 2>&1; then
    ip link add name "${LISAVM_BRIDGE}" type bridge
  fi

  if ! ip -4 addr show dev "${LISAVM_BRIDGE}" | grep -qF "${LISAVM_BRIDGE_CIDR}"; then
    ip addr replace "${LISAVM_BRIDGE_CIDR}" dev "${LISAVM_BRIDGE}"
  fi
  ip link set dev "${LISAVM_BRIDGE}" up

  sysctl -w net.ipv4.ip_forward=1 >/dev/null
  mkdir -p /etc/sysctl.d
  printf 'net.ipv4.ip_forward = 1\n' > /etc/sysctl.d/99-lisavm.conf

  local subnet
  subnet="$(python3 -c 'import ipaddress, sys; print(ipaddress.ip_interface(sys.argv[1]).network)' "${LISAVM_BRIDGE_CIDR}")"
  local egress_iface
  egress_iface="$(ip -4 route show default | awk '{print $5; exit}')"

  iptables -C FORWARD -i "${LISAVM_BRIDGE}" -o "${LISAVM_BRIDGE}" -j ACCEPT 2>/dev/null || \
    iptables -A FORWARD -i "${LISAVM_BRIDGE}" -o "${LISAVM_BRIDGE}" -j ACCEPT

  if [[ -n "${egress_iface}" ]]; then
    iptables -t nat -C POSTROUTING -s "${subnet}" -o "${egress_iface}" -j MASQUERADE 2>/dev/null || \
      iptables -t nat -A POSTROUTING -s "${subnet}" -o "${egress_iface}" -j MASQUERADE

    iptables -C FORWARD -i "${LISAVM_BRIDGE}" -o "${egress_iface}" -j ACCEPT 2>/dev/null || \
      iptables -A FORWARD -i "${LISAVM_BRIDGE}" -o "${egress_iface}" -j ACCEPT

    iptables -C FORWARD -i "${egress_iface}" -o "${LISAVM_BRIDGE}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
      iptables -A FORWARD -i "${egress_iface}" -o "${LISAVM_BRIDGE}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
  fi

  echo "configured bridge ${LISAVM_BRIDGE} (${LISAVM_BRIDGE_CIDR})"
}

install_firecracker
install_kernel
configure_networking
echo "kvm host ready: ${LISAVM_ROOT}"
