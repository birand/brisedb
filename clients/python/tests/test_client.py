"""Integration tests for the brisedb Python client."""

import queue
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

# ------------------------------------------------------------------ #
# Pub/Sub
# ------------------------------------------------------------------ #

def collect_msg(ps, kind, timeout=2.0):
    """Return the first message of *kind* from PubSubConn, or raise."""
    deadline = time.monotonic() + timeout
    for msg in ps:
        if msg.kind == kind:
            return msg
        if time.monotonic() > deadline:
            break
    raise TimeoutError(f"timed out waiting for {kind!r} message")


def test_keys_star(client):
    client.set("foo", "1")
    client.set("bar", "2")
    client.set("baz", "3")
    assert sorted(client.keys("*")) == ["bar", "baz", "foo"]


def test_keys_prefix(client):
    client.set("foo", "1")
    client.set("bar", "2")
    client.set("baz", "3")
    assert sorted(client.keys("ba*")) == ["bar", "baz"]


def test_keys_no_match(client):
    client.set("foo", "1")
    assert client.keys("xyz*") == []


def test_scan_full(client):
    for i in range(10):
        client.set(f"key{i:02d}", "v")

    collected = set()
    cursor = 0
    while True:
        cursor, batch = client.scan(cursor, count=3)
        collected.update(batch)
        if cursor == 0:
            break

    assert len(collected) == 10


def test_scan_all_at_once(client):
    for i in range(5):
        client.set(f"item{i}", "v")
    cursor, keys = client.scan(0, count=100)
    assert cursor == 0
    assert len(keys) == 5


def test_publish_no_subscribers(client):
    assert client.publish("news", "hello") == 0


def test_pubsub_subscribe_and_receive(server):
    host, port = server
    received = queue.Queue()

    def subscriber():
        with brisedb.PubSubConn(host, port) as ps:
            ps.subscribe("news")
            for msg in ps:
                if msg.kind == "subscribe":
                    received.put(("subscribed", msg.count))
                    break
            for msg in ps:
                if msg.kind == "message":
                    received.put(("message", msg.channel, msg.payload))
                    break

    t = threading.Thread(target=subscriber, daemon=True)
    t.start()

    # Wait for subscription confirmation
    event = received.get(timeout=2)
    assert event == ("subscribed", 1)

    with brisedb.Client(host, port) as pub:
        n = pub.publish("news", "hello")
    assert n == 1

    event = received.get(timeout=2)
    assert event == ("message", "news", "hello")
    t.join(timeout=2)


def test_pubsub_multiple_subscribers(server):
    host, port = server
    received = [queue.Queue(), queue.Queue()]

    def make_subscriber(q):
        def run():
            with brisedb.PubSubConn(host, port) as ps:
                ps.subscribe("sport")
                for msg in ps:
                    if msg.kind == "subscribe":
                        break
                for msg in ps:
                    if msg.kind == "message":
                        q.put(msg.payload)
                        break
        return run

    threads = [threading.Thread(target=make_subscriber(q), daemon=True) for q in received]
    for t in threads:
        t.start()
    time.sleep(0.1)  # let both subscribe

    with brisedb.Client(host, port) as pub:
        n = pub.publish("sport", "goal")
    assert n == 2

    for q in received:
        assert q.get(timeout=2) == "goal"
    for t in threads:
        t.join(timeout=2)


def test_pubsub_unsubscribe(server):
    host, port = server
    with brisedb.PubSubConn(host, port) as ps:
        ps.subscribe("news")
        for msg in ps:
            if msg.kind == "subscribe":
                break

        ps.unsubscribe("news")
        for msg in ps:
            if msg.kind == "unsubscribe":
                assert msg.channel == "news"
                assert msg.count == 0
                break

        with brisedb.Client(host, port) as pub:
            n = pub.publish("news", "late")
        assert n == 0


def test_pubsub_isolated_channels(server):
    host, port = server
    sports_q = queue.Queue()

    def sports_subscriber():
        with brisedb.PubSubConn(host, port) as ps:
            ps.subscribe("sport")
            for msg in ps:
                if msg.kind == "subscribe":
                    sports_q.put("ready")
                    break
            for msg in ps:
                if msg.kind == "message":
                    sports_q.put(msg.payload)
                    break

    t = threading.Thread(target=sports_subscriber, daemon=True)
    t.start()
    assert sports_q.get(timeout=2) == "ready"

    with brisedb.PubSubConn(host, port) as news_ps:
        news_ps.subscribe("news")
        for msg in news_ps:
            if msg.kind == "subscribe":
                break

        with brisedb.Client(host, port) as pub:
            pub.publish("sport", "goal")

        # news subscriber should NOT receive a message — poll with short timeout
        msg = news_ps.poll(timeout=0.2)
        assert msg is None or msg.kind != "message", \
            f"news got unexpected message: {msg}"

    assert sports_q.get(timeout=2) == "goal"
    t.join(timeout=2)


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
