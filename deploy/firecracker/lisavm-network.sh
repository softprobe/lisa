#!/usr/bin/env bash
set -euo pipefail

LISAVM_BRIDGE="${LISAVM_BRIDGE:-lisabr0}"
if [[ -z "${LISAVM_BRIDGE_CIDR:-}" ]]; then
  LISAVM_BRIDGE_CIDR="${LISAVM_GATEWAY_IP:-172.16.0.1}/16"
fi
LISAVM_SYSCTL_FILE="${LISAVM_SYSCTL_FILE:-/etc/sysctl.d/99-lisavm.conf}"

if ! ip link show "${LISAVM_BRIDGE}" >/dev/null 2>&1; then
  ip link add name "${LISAVM_BRIDGE}" type bridge
fi

if ! ip -4 addr show dev "${LISAVM_BRIDGE}" | grep -qF "${LISAVM_BRIDGE_CIDR}"; then
  ip addr replace "${LISAVM_BRIDGE_CIDR}" dev "${LISAVM_BRIDGE}"
fi
ip link set dev "${LISAVM_BRIDGE}" up

mkdir -p "$(dirname "${LISAVM_SYSCTL_FILE}")"
printf 'net.ipv4.ip_forward = 1\n' > "${LISAVM_SYSCTL_FILE}"
sysctl -w net.ipv4.ip_forward=1 >/dev/null

subnet="$(python3 -c 'import ipaddress, sys; print(ipaddress.ip_interface(sys.argv[1]).network)' "${LISAVM_BRIDGE_CIDR}")"
egress_iface="$(ip -4 route show default | awk '{for (i = 1; i <= NF; i++) if ($i == "dev") {print $(i + 1); exit}}')"

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

echo "configured persistent bridge ${LISAVM_BRIDGE} (${LISAVM_BRIDGE_CIDR})"
