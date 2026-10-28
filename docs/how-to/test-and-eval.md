# Tests and evaluations

Stateful tests and evaluations create run-owned PostgreSQL databases, roles, schemas, and temporary workspaces through `conftest.py`. The lifecycle helper uses `scripts/test_stack.py` to start and stop a uniquely named Compose project. Do not connect a test to a developer’s running service or database.

Run the software tests and frontend checks from the repository root:

```sh
uv run --extra test pytest -q
npm --prefix frontend test
npm --prefix frontend run build
```

Live agent evaluations use the real configured model and OpenHands runtime. They require an explicit `--live` flag and write reports under `target/test-artifacts/`. See [benchmark/README.md](../../benchmark/README.md) and the relevant dataset documentation for exact gates.

Before claiming model credentials are unavailable, inspect `.env` and the process environment without printing secret values. Never commit `.env`, generated reports, local caches, or virtual environments.
