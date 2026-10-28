# Lisa

Lisa is an AI QA engineer for agent behavior. It evaluates sessions pushed to its OTLP intake, records policy and activity state in PostgreSQL, and serves its own API, chat app, skills, and website.

Lisa runs without TheLake or Explorer. Session evidence can be pushed directly to Lisa at `/internal/v1/session-traces`; evaluation then uses the same local session store.

## Repository layout

- `src/lisa/` — runtime service, API, MCP tools, repositories, and SQL.
- `frontend/` — Lisa-owned React app and API client, built into the service image.
- `website/` — Lisa marketing site, rooted at the site origin.
- `skills/` — skills packaged for Lisa’s OpenHands runtime.
- `datasets/` and `benchmark/` — labeled resources, loaders, and live evaluation runners.
- `tests/` — unit and transient PostgreSQL integration coverage.
- `docs/` — Lisa architecture, configuration, and evaluation guidance.

## Develop

Install Python 3.12+, `uv`, Node.js 22+, and Docker. Create a local `.env` from `.env.example`, then install the Python and frontend dependencies:

```sh
uv sync --extra test
npm --prefix frontend ci
```

Run the service with a Lisa PostgreSQL database provisioned through `scripts/provision_lisa.py`:

```sh
uv run uvicorn lisa.app:app --reload --app-dir src --port 8082
```

Lisa’s app is available at `http://127.0.0.1:8082/`; the marketing site in `website/` can be served by any static host at its root. The service image builds and serves the app frontend itself:

```sh
docker build -t lisa:local .
```

## Verify

Automated integration tests create run-owned PostgreSQL resources through `scripts/test_stack.py` and clean them up when the test process ends.

```sh
uv run --extra test pytest -q
npm --prefix frontend test
npm --prefix frontend run build
```

Live evaluations use the configured model credentials and must be explicitly invoked with `--live`. Read [benchmark guidance](benchmark/README.md) and the relevant dataset README before running one. Results are written under `target/test-artifacts/`.

## Documentation

- [Lisa architecture and QA flow](docs/architecture/lisa-qa.md)
- [Lisa activity model](docs/architecture/lisa-activities.md)
- [Policy memory](docs/how-to/policy-memory.md)
- [Policy triage evaluation](docs/how-to/policy-triage-evaluation.md)
- [Runtime configuration](docs/reference/configuration.md)
