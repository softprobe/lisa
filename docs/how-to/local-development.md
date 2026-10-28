# Local Lisa development

Run Lisa, its dedicated PostgreSQL database, and the web app from this repository. Lisa evaluates complete OTLP sessions pushed to its private-network intake endpoint; it does not require TheLake.

## Start Lisa

Copy `.env.example` to `.env`, configure `GOOGLE_API_KEY` or `GEMINI_API_KEY` for chat and policy judging, and configure `OPENAI_API_KEY` for policy triage.

```bash
cp .env.example .env
# Add provider credentials to .env.
docker compose up --build
```

The app and API are available at `http://127.0.0.1:8082/lisa/`. PostgreSQL is persistent in the `lisa-postgres-data` volume, and Lisa's evidence workspace is stored in the `lisa-data` volume. Keep `.env` private.

To stop containers without deleting data, run `docker compose down`. To remove the local database and evidence data, run `docker compose down --volumes`.

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
