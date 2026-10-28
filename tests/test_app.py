import asyncio
from contextlib import asynccontextmanager
from pathlib import Path
from types import SimpleNamespace

from fastapi import HTTPException

from lisa import app


def test_lifespan_starts_push_evaluation_without_a_scheduler(monkeypatch):
    started = asyncio.Event()
    stopped = []

    class Worker:
        async def run_forever(self, workspace_id):
            assert workspace_id == "workspace-1"
            started.set()
            await asyncio.Event().wait()

        async def shutdown(self):
            stopped.append(True)

    services = SimpleNamespace(
        slack_worker=Worker(),
        context=object(),
        activity_store=object(),
        policy_discovery_worker=None,
        session_evaluation_worker=None,
        conversation_checkpoint_worker=None,
    )
    application = SimpleNamespace(state=SimpleNamespace())

    @asynccontextmanager
    async def no_recording(_context):
        yield

    monkeypatch.setenv("LISA_WORKSPACE_ID", "workspace-1")
    monkeypatch.setattr(app, "initialize_backend", lambda: None)
    monkeypatch.setattr(app, "build_services", lambda _context: services)
    monkeypatch.setattr(app, "recording_maintenance", no_recording)

    async def exercise_lifespan():
        async with app.lifespan(application):
            await asyncio.wait_for(started.wait(), timeout=1)

    asyncio.run(exercise_lifespan())
    assert stopped == [True]
    assert not hasattr(services, "thelake_sampling_worker")


def test_legacy_thelake_settings_do_not_change_lisa_context(monkeypatch, tmp_path):
    monkeypatch.setenv("LISA_THELAKE_URL", "https://lake.example")
    monkeypatch.setenv("LISA_THELAKE_BEARER_TOKEN", "old-token")
    monkeypatch.setenv("LISA_THELAKE_SAMPLING_INTERVAL_SECONDS", "bad")
    monkeypatch.setenv("LISA_DATA_DIR", str(tmp_path))

    context = app.WorkspaceContext.from_environment()

    assert not hasattr(context, "thelake_url")
    assert not hasattr(context, "thelake_bearer_token")


def test_standalone_app_serves_the_frontend_from_root(monkeypatch, tmp_path):
    static_dir = tmp_path / "static"
    static_dir.mkdir()
    (static_dir / "index.html").write_text(
        '<script>window.__LISA_API_BASE_PATH__="__LISA_API_BASE_PATH__";</script>',
        encoding="utf-8",
    )
    monkeypatch.setenv("LISA_STATIC_DIR", str(static_dir))

    index = asyncio.run(app.lisa_frontend_index())
    route = asyncio.run(app.lisa_frontend_asset("activities/activity-1"))

    assert 'window.__LISA_API_BASE_PATH__="/v1"' in index.body.decode()
    assert route.body == index.body
    try:
        asyncio.run(app.lisa_frontend_asset("v1/missing-route"))
    except HTTPException as error:
        assert error.status_code == 404
    else:
        raise AssertionError("unmatched API routes must remain 404 responses")
