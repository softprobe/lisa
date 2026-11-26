#!/usr/bin/env bash
set -euo pipefail

umask 077

BOOTSTRAP_JSON="${LISAVM_BOOTSTRAP_JSON:-/etc/lisavm/bootstrap.json}"
ENV_OUT="${LISAVM_ENV_OUT:-/etc/lisavm/lisa.env}"
PLUGINS_DIR="${LISAVM_PLUGINS_DIR:-/etc/lisavm/plugins}"

mkdir -p "$(dirname "${BOOTSTRAP_JSON}")" "$(dirname "${ENV_OUT}")" "${PLUGINS_DIR}"
chmod 0700 "$(dirname "${BOOTSTRAP_JSON}")" "$(dirname "${ENV_OUT}")" "${PLUGINS_DIR}"

if [[ ! -f "${BOOTSTRAP_JSON}" ]]; then
  install -m 0600 /dev/null "${ENV_OUT}"
  printf '{}\n' > "${PLUGINS_DIR}/mcp-servers.json"
  chmod 0600 "${PLUGINS_DIR}/mcp-servers.json"
  exit 0
fi

chmod 0600 "${BOOTSTRAP_JSON}"

python3 - "${BOOTSTRAP_JSON}" "${ENV_OUT}" "${PLUGINS_DIR}" <<'PY'
import json
import os
import shlex
import sys
from pathlib import Path

bootstrap_path = Path(sys.argv[1])
env_out = Path(sys.argv[2])
plugins_dir = Path(sys.argv[3])


def write_private(target: Path, content: str) -> None:
    tmp = target.with_name(f".{target.name}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(content)
        os.chmod(tmp, 0o600)
        os.replace(tmp, target)
    finally:
        if tmp.exists():
            tmp.unlink(missing_ok=True)


data = json.loads(bootstrap_path.read_text(encoding="utf-8"))
if not isinstance(data, dict):
    raise SystemExit("bootstrap.json must be a JSON object")

env_map = data.get("env") or {}
if not isinstance(env_map, dict):
    raise SystemExit("bootstrap.json .env must be an object")

lines = []
for key, value in sorted(env_map.items()):
    if not isinstance(key, str) or not key.isidentifier():
        raise SystemExit(f"Invalid env key: {key!r}")
    lines.append(f"{key}={shlex.quote(str(value))}")
write_private(env_out, "\n".join(lines) + ("\n" if lines else ""))

plugins = data.get("plugins") or {}
if not isinstance(plugins, dict):
    raise SystemExit("bootstrap.json .plugins must be an object")

write_private(
    plugins_dir / "mcp-servers.json",
    json.dumps(plugins, indent=2, sort_keys=True) + "\n",
)
PY
