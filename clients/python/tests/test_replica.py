"""Integration tests for brisedb Python ReplicaConn."""

import time
import threading

import brisedb


# ------------------------------------------------------------------ #
# Helpers
# ------------------------------------------------------------------ #

def wait_for_key(client, key, expected, timeout=2.0):
    """Poll until client.get(key) == expected or raise TimeoutError."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if client.get(key) == expected:
            return
        time.sleep(0.02)
    raise TimeoutError(f"key {key!r} never became {expected!r}")


# ------------------------------------------------------------------ #
# Tests
# ------------------------------------------------------------------ #

def test_replica_snapshot(server):
    """Keys written before the replica connects appear in the snapshot."""
    host, port = server
    with brisedb.Client(host, port) as c:
        c.set("snap_a", "1")
        c.set("snap_b", "2")

    with brisedb.ReplicaConn(host, port) as rep:
        # Collect snapshot entries (we know exactly 2 keys)
        entries = {}
        deadline = time.monotonic() + 2.0
        while len(entries) < 2 and time.monotonic() < deadline:
            entry = rep.poll(timeout=0.2)
            if entry is None:
                break
            if entry.op == "SET":
                entries[entry.key] = entry.value

    assert entries.get("snap_a") == "1"
    assert entries.get("snap_b") == "2"


def test_replica_live_entries(server):
    """Writes after the replica connects arrive as live WAL entries."""
    host, port = server

    received = {}
    ready = threading.Event()

    def consume(rep):
        ready.set()
        for entry in rep:
            if entry.op == "SET":
                received[entry.key] = entry.value

    with brisedb.ReplicaConn(host, port) as rep:
        t = threading.Thread(target=consume, args=(rep,), daemon=True)
        t.start()
        ready.wait(timeout=2)

        with brisedb.Client(host, port) as c:
            c.set("live_x", "hello")
            c.set("live_y", "world")

        deadline = time.monotonic() + 2.0
        while time.monotonic() < deadline:
            if "live_x" in received and "live_y" in received:
                break
            time.sleep(0.02)

    assert received.get("live_x") == "hello"
    assert received.get("live_y") == "world"


def test_replica_delete_entry(server):
    """DELETE operations are forwarded as WALEntry with op='DELETE'."""
    host, port = server

    with brisedb.Client(host, port) as c:
        c.set("to_del", "v")

    deletes = []

    with brisedb.ReplicaConn(host, port) as rep:
        # drain snapshot
        entry = rep.poll(timeout=0.5)
        while entry is not None:
            entry = rep.poll(timeout=0.1)

        t = threading.Thread(
            target=lambda: [
                deletes.append(e)
                for e in rep
                if e.op == "DELETE"
            ],
            daemon=True,
        )
        t.start()

        with brisedb.Client(host, port) as c:
            c.delete("to_del")

        deadline = time.monotonic() + 2.0
        while not deletes and time.monotonic() < deadline:
            time.sleep(0.02)

    assert any(e.key == "to_del" for e in deletes)


def test_replica_callback(server):
    """on_entry callback is called for every WAL entry."""
    host, port = server

    received = {}
    done = threading.Event()

    def on_entry(entry):
        if entry.op == "SET":
            received[entry.key] = entry.value
        if len(received) >= 2:
            done.set()

    with brisedb.Client(host, port) as c:
        c.set("cb_1", "a")
        c.set("cb_2", "b")

    rep = brisedb.ReplicaConn(host, port, on_entry=on_entry)
    try:
        done.wait(timeout=2)
    finally:
        rep.close()

    assert received.get("cb_1") == "a"
    assert received.get("cb_2") == "b"


def test_replica_ttl_entry(server):
    """SET entries with a TTL carry a non-zero expires_at."""
    host, port = server

    with brisedb.Client(host, port) as c:
        c.set_ex("ttl_key", "v", 60)

    with brisedb.ReplicaConn(host, port) as rep:
        entry = rep.poll(timeout=2.0)

    assert entry is not None
    assert entry.key == "ttl_key"
    assert entry.expires_at > 0


def test_replica_connection_closes_iterator(server):
    """Closing the ReplicaConn stops the iterator."""
    host, port = server

    rep = brisedb.ReplicaConn(host, port)
    finished = threading.Event()

    def drain():
        for _ in rep:
            pass
        finished.set()

    t = threading.Thread(target=drain, daemon=True)
    t.start()
    time.sleep(0.1)
    rep.close()
    assert finished.wait(timeout=2), "iterator did not stop after close()"
