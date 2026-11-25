#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: $0 <target-disk.ext4> [source-rootfs.ext4]" >&2
  exit 2
fi

TARGET_DISK="$1"
SOURCE_ROOTFS="${2:-/var/lib/lisavm/images/rootfs.ext4}"

if [[ ! -f "${SOURCE_ROOTFS}" ]]; then
  echo "error: source rootfs not found: ${SOURCE_ROOTFS}" >&2
  exit 1
fi

mkdir -p "$(dirname "${TARGET_DISK}")"
tmp_disk="$(mktemp "${TARGET_DISK}.XXXXXX")"
trap 'rm -f "${tmp_disk}"' EXIT

cp --reflink=auto --sparse=always "${SOURCE_ROOTFS}" "${tmp_disk}"
mv "${tmp_disk}" "${TARGET_DISK}"
echo "cloned ${SOURCE_ROOTFS} -> ${TARGET_DISK}"
