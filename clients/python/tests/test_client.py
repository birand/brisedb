"""Integration tests for the brisedb Python client."""

import time
import threading

import pytest
import brisedb


@pytest.fixture()
def client(server):
    host, port = server
    with brisedb.Client(host, port) as c:
        yield c


# ------------------------------------------------------------------ #
# Basic operations
# ------------------------------------------------------------------ #

def test_set_get(client):
    client.set("name", "Alice")
    assert client.get("name") == "Alice"


def test_get_missing_returns_none(client):
    assert client.get("ghost") is None


def test_delete(client):
    client.set("x", "1")
    client.delete("x")
    assert client.get("x") is None


def test_count(client):
    client.set("a", "foo")
    client.set("b", "foo")
    client.set("c", "bar")
    assert client.count("foo") == 2
    assert client.count("bar") == 1
    assert client.count("baz") == 0


# ------------------------------------------------------------------ #
# TTL
# ------------------------------------------------------------------ #

def test_set_ex_expires(client):
    client.set_ex("tmp", "val", 1)
    assert client.get("tmp") == "val"
    time.sleep(1.1)
    assert client.get("tmp") is None


def test_ttl_no_expiry(client):
    client.set("k", "v")
    assert client.ttl("k") == -1


def test_ttl_missing(client):
    assert client.ttl("ghost") == -2


def test_ttl_with_expiry(client):
    client.set_ex("k", "v", 10)
    assert 0 < client.ttl("k") <= 10


def test_persist(client):
    client.set_ex("k", "v", 10)
    assert client.persist("k") is True
    assert client.ttl("k") == -1


def test_set_clears_ttl(client):
    client.set_ex("k", "v1", 10)
    client.set("k", "v2")
    assert client.ttl("k") == -1


# ------------------------------------------------------------------ #
# Transactions — manual begin/commit/rollback
# ------------------------------------------------------------------ #

def test_transaction_commit(client):
    client.begin()
    client.set("k", "v")
    client.commit()
    assert client.get("k") == "v"


def test_transaction_rollback(client):
    client.begin()
    client.set("k", "v")
    client.rollback()
    assert client.get("k") is None


def test_transaction_isolation(server):
    host, port = server
    with brisedb.Client(host, port) as c1, brisedb.Client(host, port) as c2:
        c1.begin()
        c1.set("iso", "yes")
        # c2 must not see the uncommitted write
        assert c2.get("iso") is None
        c1.commit()
        # now c2 sees it
        assert c2.get("iso") == "yes"


def test_commit_without_transaction_raises(client):
    with pytest.raises(brisedb.BriseDBError):
        client.commit()


def test_rollback_without_transaction_raises(client):
    with pytest.raises(brisedb.BriseDBError):
        client.rollback()


# ------------------------------------------------------------------ #
# transaction() context manager
# ------------------------------------------------------------------ #

def test_transaction_context_manager_commits(client):
    with client.transaction():
        client.set("k", "v")
    assert client.get("k") == "v"


def test_transaction_context_manager_rolls_back_on_error(client):
    with pytest.raises(ValueError):
        with client.transaction():
            client.set("k", "v")
            raise ValueError("oops")
    assert client.get("k") is None


# ------------------------------------------------------------------ #
# Compact
# ------------------------------------------------------------------ #

def test_compact(client):
    client.set("a", "1")
    client.set("b", "2")
    client.delete("a")
    client.compact()
    assert client.get("a") is None
    assert client.get("b") == "2"


# ------------------------------------------------------------------ #
# Error handling
# ------------------------------------------------------------------ #

def test_set_ex_invalid_ttl(client):
    with pytest.raises(ValueError):
        client.set_ex("k", "v", 0)


def test_unknown_command_raises(client):
    with pytest.raises(brisedb.BriseDBError):
        client._cmd("FOOBAR")


# ------------------------------------------------------------------ #
# Concurrency
# ------------------------------------------------------------------ #

def test_concurrent_clients(server):
    host, port = server
    errors = []

    def worker(i):
        with brisedb.Client(host, port) as c:
            key, val = f"key{i}", f"val{i}"
            c.set(key, val)
            got = c.get(key)
            if got != val:
                errors.append(f"worker {i}: want {val!r}, got {got!r}")

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(20)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    assert errors == [], "\n".join(errors)
