# Lisa runtime configuration

Lisa is configured through environment variables. Runtime data is stored in Lisa-owned PostgreSQL and filesystem roots. The service accepts session bundles directly and has no required TheLake configuration.

| Variable | Purpose |
|---|---|
| `LISA_WORKSPACE_ID` | UUID scope for one Lisa runtime deployment. |
| `LISA_DATABASE_URL` | PostgreSQL writer connection used by Lisa's app and workers. |
| `LISA_DATABASE_READ_URL` | Read-only PostgreSQL connection used by the Lisa database tool. |
| `LISA_DATABASE_PROVISION_URL` | Deployment-only administrator connection used by `scripts/provision_lisa.py`. |
| `LISA_DATABASE_WRITER_PASSWORD` / `LISA_DATABASE_READER_PASSWORD` | Provision the runtime roles when creating a database. |
| `LISA_SERVICE_TOKEN` | Bearer token for authenticated internal API routes. |
| `LISA_MODEL` | Chat model identifier; defaults to `gemini/gemini-3.8-flash`. |
| `GOOGLE_API_KEY` / `GEMINI_API_KEY` | Gemini access for chat and policy judgments. |
| `OPENAI_API_KEY` | OpenAI Decisions policy triage. |
| `GEMINI_MODEL_NAME` | Judge model; defaults to `gemini-3.8-flash`. |
| `LISA_DATA_DIR` | Root for OpenHands and conversation state. |
| `LISA_AGENT_WORKSPACE_ROOT` | Root for pushed OTLP session bundles. |
| `LISA_MAX_SESSION_TRACE_BYTES` | Maximum body size for pushed session traces; defaults to 16 MiB. |
| `LISA_CORS_ORIGINS` | Comma-separated browser origins allowed by CORS. |
| `LISA_OWNER_USER_IDS` | Lisa actor IDs authorized to approve policy changes. |
| `SOFTPROBE_ASSERTION_HMAC_SECRET` | HMAC key for signed workspace assertions when enabled. |
| `LISA_SLACK_*` | Optional Slack event and notification configuration; see [Slack conversations](../how-to/slack-evaluator.md). |
| `LISA_STATIC_DIR` | Frontend bundle directory; defaults to `/app/static` in the image. |

The session intake `POST /internal/v1/session-traces` is unauthenticated and must remain on a private network. Producers submit a complete OTLP JSON bundle and retry failed submissions. See [dogfood](../how-to/policy-dogfood.md) for a full flow.
