"""Run the local Lisa sandbox with a bundled PostgreSQL server."""

from __future__ import annotations

import os
import secrets
import subprocess
import sys
from pathlib import Path
from urllib.parse import urlparse, urlunparse
from uuid import UUID

import uvicorn
from psycopg.conninfo import make_conninfo

REPOSITORY_ROOT = Path(__file__).resolve().parents[1]
PROVISION_SCRIPT = REPOSITORY_ROOT / "scripts" / "provision_lisa.py"
DEFAULT_WORKSPACE_ID = "550e8400-e29b-41d4-a716-446655440000"


def _refuse_external_database_configuration() -> None:
    configured = [
        name
        for name in (
            "LISA_DATABASE_URL",
            "LISA_DATABASE_READ_URL",
            "LISA_DATABASE_PROVISION_URL",
        )
        if os.getenv(name, "").strip()
    ]
    if configured:
        names = ", ".join(configured)
        raise SystemExit(
            f"Refusing to replace configured database settings ({names}); "
            "clear them before starting the embedded sandbox."
        )


def _prepare_local_paths() -> Path:
    local_root = Path.home() / ".local" / "share" / "lisa"

    def configured_path(name: str, default: Path) -> Path:
        return Path(os.getenv(name, "").strip() or default).expanduser()

    data_dir = configured_path("LISA_DATA_DIR", local_root / "workspaces")
    agent_workspace = configured_path(
        "LISA_AGENT_WORKSPACE_ROOT", local_root / "agent-workspace"
    )
    postgres_data = configured_path("LISA_EMBEDDED_DATABASE_DIR", local_root / "postgres")
    data_dir.mkdir(parents=True, exist_ok=True)
    agent_workspace.mkdir(parents=True, exist_ok=True)
    postgres_data.parent.mkdir(parents=True, exist_ok=True)
    os.environ["LISA_DATA_DIR"] = str(data_dir.resolve())
    os.environ["LISA_AGENT_WORKSPACE_ROOT"] = str(agent_workspace.resolve())
    os.environ["LISA_EMBEDDED_DATABASE_DIR"] = str(postgres_data.resolve())
    return postgres_data


def _workspace_id() -> str:
    value = os.getenv("LISA_WORKSPACE_ID", DEFAULT_WORKSPACE_ID).strip()
    try:
        value = str(UUID(value))
    except ValueError as error:
        raise SystemExit("LISA_WORKSPACE_ID must be a valid UUID") from error
    os.environ["LISA_WORKSPACE_ID"] = value
    return value


def _provision_server(server, workspace_id: str) -> None:
    base_dsn = server.get_uri(database="postgres")
    provision_dsn = urlunparse(urlparse(base_dsn)._replace(path="/lisa"))
    writer_role = os.getenv("LISA_DATABASE_WRITER_ROLE", "").strip() or "lisa_writer"
    reader_role = os.getenv("LISA_DATABASE_READER_ROLE", "").strip() or "lisa_reader"
    writer_password = os.getenv("LISA_DATABASE_WRITER_PASSWORD", "").strip()
    reader_password = os.getenv("LISA_DATABASE_READER_PASSWORD", "").strip()
    writer_password = writer_password or secrets.token_urlsafe(32)
    reader_password = reader_password or secrets.token_urlsafe(32)

    provision_environment = os.environ.copy()
    provision_environment.pop("LISA_DATABASE_RECREATE", None)
    provision_environment.update(
        {
            "LISA_DATABASE_PROVISION_URL": provision_dsn,
            "LISA_DATABASE_WRITER_PASSWORD": writer_password,
            "LISA_DATABASE_READER_PASSWORD": reader_password,
            "LISA_DATABASE_WRITER_ROLE": writer_role,
            "LISA_DATABASE_READER_ROLE": reader_role,
            "LISA_PGMQ_INSTALL_MODE": "sql",
            "LISA_WORKSPACE_ID": workspace_id,
        }
    )
    subprocess.run(
        [sys.executable, str(PROVISION_SCRIPT)],
        cwd=REPOSITORY_ROOT,
        check=True,
        env=provision_environment,
    )

    os.environ["LISA_DATABASE_URL"] = make_conninfo(
        provision_dsn,
        user=writer_role,
        password=writer_password,
    )
    os.environ["LISA_DATABASE_READ_URL"] = make_conninfo(
        provision_dsn,
        user=reader_role,
        password=reader_password,
    )


def main() -> None:
    _refuse_external_database_configuration()
    try:
        import pgembed
    except ImportError as error:
        raise SystemExit(
            "The embedded sandbox requires pgembed. Install it with "
            "`uv sync --extra openhands --extra sandbox`."
        ) from error

    postgres_data = _prepare_local_paths()
    workspace_id = _workspace_id()
    server = pgembed.get_server(postgres_data, cleanup_mode="stop")
    try:
        _provision_server(server, workspace_id)
        uvicorn.run(
            "lisa.app:app",
            host=os.getenv("LISA_SANDBOX_HOST", "").strip() or "127.0.0.1",
            port=int(os.getenv("LISA_SANDBOX_PORT", "").strip() or "8082"),
            workers=1,
        )
    finally:
        server.cleanup()


if __name__ == "__main__":
    main()
