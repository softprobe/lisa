from __future__ import annotations

from urllib.parse import urlparse

import psycopg

from lisa.repositories.session_evaluation_queue import SessionEvaluationQueue
from scripts.provision_lisa import provision


def test_sql_only_pgmq_install_keeps_lisa_session_queue_working(lisa_test_db):
    from pgmq import install_pgmq_from_sql

    database = lisa_test_db
    with psycopg.connect(database.provision_url, autocommit=True) as connection:
        connection.execute("DROP EXTENSION pgmq CASCADE")

    installed_version = install_pgmq_from_sql(dsn=database.provision_url)
    assert installed_version

    provision(
        database.provision_url,
        "11111111-1111-4111-8111-111111111111",
        writer_role=urlparse(database.writer_url).username,
        reader_role=urlparse(database.reader_url).username,
        recreate=True,
    )

    queue = SessionEvaluationQueue(database.writer_url)
    assert queue.send("sandbox-session", "checkout-agent", "a" * 64)
    message = queue.read_one()
    assert message is not None
    assert message["message"]["session_id"] == "sandbox-session"
    queue.delete(message["msg_id"])
    assert queue.read_one() is None
