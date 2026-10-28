"""Production Lisa evaluation: trajectory, triage, judge, and scores.

Anti-pattern (do not repeat): labeled resources and loaders belong in
``datasets/``; live runners belong in ``benchmark/``; software tests
belong in ``tests/``. Do not put those under this installable runtime
package. Runtime code must never import ``benchmark`` or ``datasets``.
"""
