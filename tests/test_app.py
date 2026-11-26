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


def test_runtime_busy_reports_worker_active_jobs(monkeypatch):
    from fastapi.testclient import TestClient

    @asynccontextmanager
    async def no_lifespan(_app):
        yield

    monkeypatch.setattr(app.app.router, "lifespan_context", no_lifespan)
    if hasattr(app.app.state, "workspace_services"):
        delattr(app.app.state, "workspace_services")

    with TestClient(app.app) as client:
        uninitialized = client.get("/internal/v1/runtime/busy")
        assert uninitialized.status_code == 200
        assert uninitialized.json() == {"busy": False, "active_jobs": 0, "workers": {}}

        app.app.state.workspace_services = SimpleNamespace(
            session_evaluation_worker=SimpleNamespace(active_jobs=0),
            conversation_checkpoint_worker=SimpleNamespace(active_jobs=0),
            policy_discovery_worker=SimpleNamespace(active_jobs=0),
            slack_worker=SimpleNamespace(active_jobs=0),
        )
        idle = client.get("/internal/v1/runtime/busy")
        assert idle.status_code == 200
        assert idle.json() == {
            "busy": False,
            "active_jobs": 0,
            "workers": {
                "session_evaluation": 0,
                "conversation_checkpoint": 0,
                "policy_discovery": 0,
                "slack": 0,
            },
        }

        app.app.state.workspace_services.session_evaluation_worker.active_jobs = 2
        app.app.state.workspace_services.slack_worker.active_jobs = 1
        busy = client.get("/internal/v1/runtime/busy")
        assert busy.status_code == 200
        assert busy.json() == {
            "busy": True,
            "active_jobs": 3,
            "workers": {
                "session_evaluation": 2,
                "conversation_checkpoint": 0,
                "policy_discovery": 0,
                "slack": 1,
            },
        }


def test_workers_track_active_jobs_during_execution(monkeypatch):
    from lisa.services.session_evaluations import SessionEvaluationWorker
    from lisa.services.slack_events import SlackEventWorker

    async def exercise():
        eval_entered = asyncio.Event()
        eval_release = asyncio.Event()

        async def fake_process_message(_message):
            eval_entered.set()
            await eval_release.wait()

        eval_worker = SessionEvaluationWorker(queue=None, traces=None, services=None)
        monkeypatch.setattr(eval_worker, "_process_message", fake_process_message)
        assert eval_worker.active_jobs == 0
        assert not eval_worker.is_busy

        eval_task = asyncio.create_task(eval_worker._process({"msg_id": 1, "message": {}}))
        await eval_entered.wait()
        assert eval_worker.active_jobs == 1
        assert eval_worker.is_busy
        eval_release.set()
        await eval_task
        assert eval_worker.active_jobs == 0
        assert not eval_worker.is_busy

        slack_entered = asyncio.Event()
        slack_release = asyncio.Event()

        class FakeEvents:
            async def process(self, _path, **_kwargs):
                slack_entered.set()
                await slack_release.wait()

        slack_worker = SlackEventWorker(events=FakeEvents(), slack=None, run_turn=lambda: None)
        assert slack_worker.active_jobs == 0
        assert not slack_worker.is_busy
        slack_task = asyncio.create_task(slack_worker._process(Path("/tmp/event.json")))
        slack_worker._tasks["event"] = slack_task
        await slack_entered.wait()
        assert slack_worker.active_jobs == 1
        assert slack_worker.is_busy
        slack_release.set()
        await slack_task
        assert slack_worker.active_jobs == 0
        assert not slack_worker.is_busy

    asyncio.run(exercise())
