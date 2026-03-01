"""brisedb Python client — line-based TCP protocol."""

import queue
import socket
import threading
from dataclasses import dataclass, field


class BriseDBError(Exception):
    """Raised when the server returns an error response."""


class Client:
    """Thread-safe client for a brisedb TCP server.

    Usage::

        with brisedb.Client("localhost", 6380) as c:
            c.set("name", "Alice")
            print(c.get("name"))   # "Alice"
            print(c.get("ghost"))  # None

        # Transactions
        with brisedb.Client("localhost", 6380) as c:
            c.begin()
            c.set("x", "1")
            c.commit()
    """

    def __init__(self, host: str = "localhost", port: int = 6380):
        self._sock = socket.create_connection((host, port))
        self._file = self._sock.makefile("r", encoding="utf-8")
        self._lock = threading.Lock()

    # ------------------------------------------------------------------ #
    # Context manager
    # ------------------------------------------------------------------ #

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()

    def close(self):
        """Close the connection."""
        try:
            self._sock.sendall(b"STOP\n")
        except OSError:
            pass
        self._sock.close()

    # ------------------------------------------------------------------ #
    # Core operations
    # ------------------------------------------------------------------ #

    def set(self, key: str, value: str) -> None:
        """Set *key* to *value*, clearing any existing TTL."""
        self._cmd("SET", key, value)

    def set_ex(self, key: str, value: str, ttl: int) -> None:
        """Set *key* to *value* with a TTL of *ttl* seconds."""
        if ttl <= 0:
            raise ValueError("ttl must be a positive integer")
        self._cmd("SET", key, value, "EX", str(ttl))

    def get(self, key: str) -> str | None:
        """Return the value for *key*, or ``None`` if absent / expired."""
        val = self._cmd("GET", key)
        return None if val == "nil" else val

    def delete(self, key: str) -> None:
        """Delete *key* from the store."""
        self._cmd("DELETE", key)

    def count(self, value: str) -> int:
        """Return the number of keys whose value equals *value*."""
        return int(self._cmd("COUNT", value))

    def ttl(self, key: str) -> int:
        """Return the remaining TTL of *key* in seconds.

        * ``-1`` — key exists, no expiry
        * ``-2`` — key does not exist or has expired
        * ``>= 0`` — remaining seconds
        """
        return int(self._cmd("TTL", key))

    def persist(self, key: str) -> bool:
        """Remove the TTL from *key*.  Returns ``True`` if a TTL was removed."""
        return self._cmd("PERSIST", key) == "1"

    def keys(self, pattern: str = "*") -> list[str]:
        """Return all keys matching *pattern*. Supports * and ? wildcards."""
        return self._cmd_list("KEYS", pattern)

    def scan(self, cursor: int = 0, count: int = 10) -> tuple[int, list[str]]:
        """Return a paginated batch of keys.

        Returns ``(next_cursor, keys)``. When *next_cursor* is 0, iteration
        is complete.
        """
        line = f"SCAN {cursor} COUNT {count}\n"
        with self._lock:
            self._sock.sendall(line.encode("utf-8"))
            # First line: "+<next_cursor> <N>"
            resp = self._file.readline().rstrip("\n")
        if resp.startswith("-"):
            raise BriseDBError(resp[1:])
        if not resp.startswith("+"):
            raise BriseDBError(f"malformed SCAN response: {resp!r}")
        parts = resp[1:].split()
        if len(parts) < 2:
            raise BriseDBError(f"malformed SCAN response: {resp!r}")
        next_cursor, n = int(parts[0]), int(parts[1])
        batch = self._read_lines(n)
        return next_cursor, batch

    def publish(self, channel: str, message: str) -> int:
        """Publish *message* to *channel*. Returns the number of receivers."""
        return int(self._cmd("PUBLISH", channel, message))

    def compact(self) -> None:
        """Rewrite the server WAL to contain only live keys."""
        self._cmd("COMPACT")

    # ------------------------------------------------------------------ #
    # Transactions
    # ------------------------------------------------------------------ #

    def begin(self) -> None:
        """Start a transaction."""
        self._cmd("BEGIN")

    def commit(self) -> None:
        """Commit the current transaction."""
        self._cmd("COMMIT")

    def rollback(self) -> None:
        """Roll back the current transaction."""
        self._cmd("ROLLBACK")

    def transaction(self):
        """Context manager that commits on success and rolls back on error.

        Usage::

            with c.transaction():
                c.set("a", "1")
                c.set("b", "2")
        """
        return _Transaction(self)

    # ------------------------------------------------------------------ #
    # Internal
    # ------------------------------------------------------------------ #

    def _cmd_list(self, *parts: str) -> list[str]:
        """Send a command and read a count-prefixed list of values."""
        line = " ".join(parts) + "\n"
        with self._lock:
            self._sock.sendall(line.encode("utf-8"))
            resp = self._file.readline().rstrip("\n")
            if resp.startswith("-"):
                raise BriseDBError(resp[1:])
            if not resp.startswith("+"):
                raise BriseDBError(f"malformed response: {resp!r}")
            n = int(resp[1:])
            return self._read_lines(n)

    def _read_lines(self, n: int) -> list[str]:
        """Read n '+value' lines. Must be called while self._lock is held."""
        result = []
        for _ in range(n):
            line = self._file.readline().rstrip("\n")
            if not line.startswith("+"):
                raise BriseDBError(f"malformed list item: {line!r}")
            result.append(line[1:])
        return result

    def _cmd(self, *parts: str) -> str:
        line = " ".join(parts) + "\n"
        with self._lock:
            self._sock.sendall(line.encode("utf-8"))
            resp = self._file.readline()
        if not resp:
            raise BriseDBError("connection closed by server")
        resp = resp.rstrip("\n")
        if resp.startswith("+"):
            return resp[1:]
        if resp.startswith("-"):
            raise BriseDBError(resp[1:])
        raise BriseDBError(f"malformed response: {resp!r}")


@dataclass
class PubSubMessage:
    """A message received over a pub/sub connection.

    kind    — "subscribe", "unsubscribe", or "message"
    channel — the channel name
    payload — the message body (only set for kind == "message")
    count   — active subscription count (set for subscribe/unsubscribe)
    """
    kind: str
    channel: str
    payload: str = ""
    count: int = 0


class PubSubConn:
    """Dedicated connection for pub/sub.

    Must not be used for regular key-value commands.

    Usage::

        with brisedb.PubSubConn("localhost", 6380) as ps:
            ps.subscribe("news", "sports")
            for msg in ps:            # blocks until Close()
                if msg.kind == "message":
                    print(msg.channel, msg.payload)
    """

    def __init__(self, host: str = "localhost", port: int = 6380):
        self._sock = socket.create_connection((host, port))
        self._file = self._sock.makefile("r", encoding="utf-8")
        self._wlock = threading.Lock()
        self._queue: queue.Queue[PubSubMessage | None] = queue.Queue(maxsize=256)
        self._thread = threading.Thread(target=self._read_loop, daemon=True)
        self._thread.start()

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()

    def __iter__(self):
        """Yield PubSubMessage objects until the connection is closed."""
        while True:
            msg = self._queue.get()
            if msg is None:
                break
            yield msg

    def subscribe(self, *channels: str) -> None:
        """Subscribe to one or more channels."""
        self._send("SUBSCRIBE", *channels)

    def unsubscribe(self, *channels: str) -> None:
        """Unsubscribe from channels. No args = unsubscribe from all."""
        self._send("UNSUBSCRIBE", *channels)

    def messages(self):
        """Iterator over incoming PubSubMessage objects (same as iterating the conn)."""
        return iter(self)

    def poll(self, timeout: float | None = None) -> "PubSubMessage | None":
        """Return the next message, or ``None`` if *timeout* seconds elapse.

        Useful for checking whether a message arrived without blocking forever.
        """
        try:
            msg = self._queue.get(timeout=timeout)
            return msg  # may be None sentinel on connection close
        except queue.Empty:
            return None

    def close(self) -> None:
        """Close the connection."""
        try:
            self._sock.close()
        except OSError:
            pass

    def _send(self, cmd: str, *args: str) -> None:
        line = " ".join([cmd, *args]) + "\n"
        with self._wlock:
            self._sock.sendall(line.encode("utf-8"))

    def _read_loop(self) -> None:
        try:
            for line in self._file:
                line = line.rstrip("\n")
                msg = self._parse(line)
                if msg:
                    self._queue.put(msg)
        except OSError:
            pass
        finally:
            self._queue.put(None)  # sentinel

    @staticmethod
    def _parse(line: str) -> PubSubMessage | None:
        if not line.startswith("+"):
            return None
        parts = line[1:].split(" ", 2)
        if not parts:
            return None
        kind = parts[0].lower()
        if kind == "message" and len(parts) == 3:
            return PubSubMessage(kind="message", channel=parts[1], payload=parts[2])
        if kind in ("subscribe", "unsubscribe") and len(parts) == 3:
            try:
                count = int(parts[2])
            except ValueError:
                count = 0
            return PubSubMessage(kind=kind, channel=parts[1], count=count)
        return None


class _Transaction:
    """Internal context manager returned by Client.transaction()."""

    def __init__(self, client: Client):
        self._c = client

    def __enter__(self):
        self._c.begin()
        return self._c

    def __exit__(self, exc_type, *_):
        if exc_type is None:
            self._c.commit()
        else:
            self._c.rollback()
