from __future__ import annotations

import os
import subprocess
import sys
from types import SimpleNamespace

import pytest
from psycopg.conninfo import conninfo_to_dict

import scripts.run_sandbox as run_sandbox


def _sandbox_environment(monkeypatch, tmp_path):
    monkeypatch.setenv("LISA_DATA_DIR", str(tmp_path / "workspaces"))
    monkeypatch.setenv("LISA_AGENT_WORKSPACE_ROOT", str(tmp_path / "agents"))
    monkeypatch.setenv("LISA_EMBEDDED_DATABASE_DIR", str(tmp_path / "postgres"))
    monkeypatch.setenv("LISA_WORKSPACE_ID", "11111111-1111-4111-8111-111111111111")
    monkeypatch.setenv("LISA_DATABASE_URL", "")
    monkeypatch.setenv("LISA_DATABASE_READ_URL", "")
    monkeypatch.setenv("LISA_DATABASE_PROVISION_URL", "")


def test_sandbox_starts_pgembed_provisions_pgmq_and_runs_lisa(monkeypatch, tmp_path):
    _sandbox_environment(monkeypatch, tmp_path)
    calls = []

    class FakeServer:
        def get_uri(self, database=None):
            calls.append(("get_uri", database))
            return "postgresql://postgres@/postgres?host=%2Ftmp%2Flisa-pg"

        def cleanup(self):
            calls.append(("cleanup",))

    server = FakeServer()
    monkeypatch.setitem(
        sys.modules,
        "pgembed",
        SimpleNamespace(
            get_server=lambda path, **kwargs: calls.append(("get_server", path, kwargs))
            or server
        ),
    )

    def run(command, *, cwd, check, env):
        calls.append(("provision", command, cwd, check, env.copy()))
        return subprocess.CompletedProcess(command, 0)

    def run_uvicorn(app, **kwargs):
        calls.append(("uvicorn", app, kwargs))

    monkeypatch.setattr(run_sandbox.subprocess, "run", run)
    monkeypatch.setattr(run_sandbox.uvicorn, "run", run_uvicorn)

    run_sandbox.main()

    get_server_call = next(call for call in calls if call[0] == "get_server")
    assert get_server_call[1] == tmp_path / "postgres"
    assert get_server_call[2] == {"cleanup_mode": "stop"}
    provision = next(call for call in calls if call[0] == "provision")
    assert provision[1][-1].endswith("scripts/provision_lisa.py")
    assert provision[4]["LISA_PGMQ_INSTALL_MODE"] == "sql"
    assert provision[4]["LISA_DATABASE_WRITER_PASSWORD"]
    assert provision[4]["LISA_DATABASE_READER_PASSWORD"]
    assert conninfo_to_dict(os.environ["LISA_DATABASE_URL"])["user"] == "lisa_writer"
    assert conninfo_to_dict(os.environ["LISA_DATABASE_URL"])["dbname"] == "lisa"
    assert conninfo_to_dict(os.environ["LISA_DATABASE_READ_URL"])["user"] == "lisa_reader"
    assert calls[-2][0] == "uvicorn"
    assert calls[-1] == ("cleanup",)


def test_sandbox_does_not_override_an_external_database(monkeypatch, tmp_path):
    _sandbox_environment(monkeypatch, tmp_path)
    monkeypatch.setenv("LISA_DATABASE_URL", "postgresql://developer/db")
    started = []
    monkeypatch.setitem(
        sys.modules,
        "pgembed",
        SimpleNamespace(get_server=lambda *_args, **_kwargs: started.append(True)),
    )

    with pytest.raises(SystemExit, match="LISA_DATABASE_URL"):
        run_sandbox.main()

    assert started == []


def test_sandbox_stops_pgembed_if_provisioning_fails(monkeypatch, tmp_path):
    _sandbox_environment(monkeypatch, tmp_path)
    stopped = []

    class FakeServer:
        def get_uri(self, database=None):
            return "postgresql://postgres@/postgres?host=%2Ftmp%2Flisa-pg"

        def cleanup(self):
            stopped.append(True)

    monkeypatch.setitem(
        sys.modules,
        "pgembed",
        SimpleNamespace(get_server=lambda *_args, **_kwargs: FakeServer()),
    )

    def fail_provisioning(*_args, **_kwargs):
        raise subprocess.CalledProcessError(1, ["provision_lisa.py"])

    monkeypatch.setattr(run_sandbox.subprocess, "run", fail_provisioning)
    monkeypatch.setattr(
        run_sandbox.uvicorn,
        "run",
        lambda *_args, **_kwargs: pytest.fail("Lisa must not start after failed provisioning"),
    )

    with pytest.raises(subprocess.CalledProcessError):
        run_sandbox.main()

    assert stopped == [True]
