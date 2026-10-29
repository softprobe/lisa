# Local Lisa development

Run Lisa and its web app from this repository. Lisa evaluates complete OTLP sessions pushed to its private-network intake endpoint; it does not require TheLake.

## Start Lisa

Copy `.env.example` to `.env`, configure `GOOGLE_API_KEY` or `GEMINI_API_KEY` for chat and policy judging, and configure `OPENAI_API_KEY` for policy triage.

```bash
cp .env.example .env
# Add provider credentials to .env.
docker compose up --build
```

The app and API are available at `http://127.0.0.1:8082/lisa/`. PostgreSQL is persistent in the `lisa-postgres-data` volume, and Lisa's evidence workspace is stored in the `lisa-data` volume. Keep `.env` private.

To stop containers without deleting data, run `docker compose down`. To remove the local database and evidence data, run `docker compose down --volumes`.

## Lightweight sandbox

For a local run without Docker Compose or a separately configured database, install
the embedded-server extra and launch Lisa directly:

```bash
uv sync --extra openhands --extra sandbox
uv run --extra openhands --extra sandbox python scripts/run_sandbox.py
```

The launcher starts PostgreSQL through `pgembed`, provisions Lisa, installs the
official PGMQ SQL snapshot, and runs the same app, workers, and PGMQ repositories
used by the regular deployment. The database persists under
`~/.local/share/lisa/postgres` by default; set `LISA_EMBEDDED_DATABASE_DIR` to
change it. Lisa's other local files default under `~/.local/share/lisa/` as well.
The launcher listens on `127.0.0.1:8082`; use `LISA_SANDBOX_HOST` and
`LISA_SANDBOX_PORT` to change that. Stop with Ctrl+C; the PostgreSQL process is
stopped cleanly and its data directory is retained.

PGMQ's SQL-only install is a pinned snapshot, without extension-managed upgrades.
The regular Compose and production deployment paths continue using the PGMQ
PostgreSQL extension.

## Send sessions

Send complete OTLP JSON session bundles to `POST /internal/v1/session-traces` over a private network. Lisa validates and stores each bundle, then queues evaluation durably in PostgreSQL. See the [dogfood guide](policy-dogfood.md) for an isolated end-to-end procedure.

## Development and tests

```bash
uv sync --extra test
uv run --extra test pytest -q tests
npm ci --prefix frontend
npm --prefix frontend test
npm --prefix frontend run build
```

The test suite starts and stops a disposable PostgreSQL container through `scripts/test_stack.py`; it never attaches to the development database. See [tests and evaluations](test-and-eval.md) and the [policy evaluation guide](policy-triage-evaluation.md).
