"""pytest fixtures: build the Go server binary and start one instance per test."""

import os
import socket
import subprocess
import sys
import tempfile
import time

import pytest

# Absolute path to the repo root (two levels up from this file)
REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
SERVER_BIN = os.path.join(REPO_ROOT, "_test_server")


def pytest_configure(config):
    """Build the server binary once before the test session."""
    result = subprocess.run(
        ["go", "build", "-o", SERVER_BIN, "./cmd/server/"],
        cwd=REPO_ROOT,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        print("go build failed:\n" + result.stderr, file=sys.stderr)
        raise RuntimeError("failed to build brisedb server")


def pytest_unconfigure(config):
    """Remove the compiled binary after the test session."""
    try:
        os.unlink(SERVER_BIN)
    except FileNotFoundError:
        pass


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture()
def server():
    """Start a fresh brisedb server for each test; yield its (host, port)."""
    port = _free_port()
    wal = tempfile.mktemp(suffix=".wal")
    proc = subprocess.Popen(
        [SERVER_BIN, "--addr", f"127.0.0.1:{port}", "--wal", wal],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )

    # Wait until the port is open (up to 3 s)
    deadline = time.monotonic() + 3
    while time.monotonic() < deadline:
        try:
            socket.create_connection(("127.0.0.1", port), timeout=0.1).close()
            break
        except OSError:
            time.sleep(0.05)
    else:
        proc.terminate()
        raise RuntimeError(f"server did not start on port {port}")

    yield "127.0.0.1", port

    proc.terminate()
    proc.wait()
    try:
        os.unlink(wal)
    except FileNotFoundError:
        pass
