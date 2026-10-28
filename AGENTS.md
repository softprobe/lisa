# Lisa engineering guidance

Lisa is an independently deployable public repository. Do not add a TheLake or Explorer runtime dependency. Lisa accepts pushed session evidence through its own API and persists its own operational state in PostgreSQL.

- Keep production runtime code under `src/lisa/`; datasets and benchmark runners stay outside the installable runtime package.
- Runtime skills belong in `skills/` and are copied into the service image. Developer workflows belong in `.agents/skills/`.
- Follow Lisa’s API, activity, policy, and evaluation contracts in `docs/architecture/` and `docs/how-to/`.
- Write a failing test before changing behavior. Cover the affected unit/API/integration boundary and run the relevant live labeled evaluation for prompt, skill, tool, or workflow changes.
- All automated stateful tests and evaluations use transient, run-owned Postgres databases and temporary workspaces. `conftest.py` owns lifecycle setup; `scripts/test_stack.py` is the sole Docker lifecycle owner. Reports stay under `target/test-artifacts/`.
- Keep docs and developer skills in sync with behavior. Treat dataset provenance, license notes, revisions, and checksums as part of the labeled resource.
- This repository is public. Review complete outgoing file contents and commits for credentials, internal information, and dataset disclosure before publishing.
