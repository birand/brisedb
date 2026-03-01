"""brisedb Python client — line-based TCP protocol."""

import socket
import threading


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
