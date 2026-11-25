#!/usr/bin/env bash
set -euo pipefail

BOOTSTRAP_JSON="${LISAVM_BOOTSTRAP_JSON:-/etc/lisavm/bootstrap.json}"
ENV_OUT="${LISAVM_ENV_OUT:-/etc/lisavm/lisa.env}"
PLUGINS_DIR="${LISAVM_PLUGINS_DIR:-/etc/lisavm/plugins}"

mkdir -p "$(dirname "${ENV_OUT}")" "${PLUGINS_DIR}"

if [[ ! -f "${BOOTSTRAP_JSON}" ]]; then
  touch "${ENV_OUT}"
  exit 0
fi

python3 - "${BOOTSTRAP_JSON}" "${ENV_OUT}" "${PLUGINS_DIR}" <<'PY'
import json
import shlex
import sys
from pathlib import Path

bootstrap_path = Path(sys.argv[1])
env_out = Path(sys.argv[2])
plugins_dir = Path(sys.argv[3])

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
env_out.write_text("\n".join(lines) + ("\n" if lines else ""), encoding="utf-8")

plugins = data.get("plugins") or {}
if not isinstance(plugins, dict):
    raise SystemExit("bootstrap.json .plugins must be an object")

(plugins_dir / "mcp-servers.json").write_text(
    json.dumps(plugins, indent=2, sort_keys=True) + "\n",
    encoding="utf-8",
)
PY
