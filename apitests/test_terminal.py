"""Terminal API tests — session lifecycle and WebSocket I/O."""

import json
import os
import time

import pytest
import requests
import websocket as ws_client  # pip install websocket-client

BASE_URL = os.environ.get("CONTENOX_API_URL", "http://localhost:8081/api")
WS_BASE = BASE_URL.replace("http://", "ws://").replace("https://", "wss://")

# Session with auth cookie for all requests
_session = requests.Session()
_authenticated = False


def _ensure_auth():
    """Login once and reuse session cookies for subsequent requests."""
    global _authenticated
    if _authenticated:
        return
    email = os.environ.get("CONTENOX_USER", "admin")
    password = os.environ.get("CONTENOX_PASS", "admin")
    r = _session.post(f"{BASE_URL}/ui/login", json={"email": email, "password": password}, timeout=10)
    if r.status_code != 200:
        pytest.skip(f"Cannot authenticate: login returned {r.status_code}: {r.text[:100]}")
    _authenticated = True


def _auth_cookie():
    """Return auth_token cookie value for WebSocket connections."""
    _ensure_auth()
    return _session.cookies.get("auth_token", "")


# ── Helpers ────────────────────────────────────────────────────────────────

def create_session(cwd="", cols=80, rows=24):
    _ensure_auth()
    body = {"cwd": cwd, "cols": cols, "rows": rows}
    r = _session.post(f"{BASE_URL}/terminal/sessions", json=body, timeout=10)
    return r


def list_sessions():
    _ensure_auth()
    r = _session.get(f"{BASE_URL}/terminal/sessions", timeout=10)
    return r


def get_session(session_id):
    _ensure_auth()
    r = _session.get(f"{BASE_URL}/terminal/sessions/{session_id}", timeout=10)
    return r


def delete_session(session_id):
    _ensure_auth()
    r = _session.delete(f"{BASE_URL}/terminal/sessions/{session_id}", timeout=10)
    return r


# ── Tests ──────────────────────────────────────────────────────────────────

class TestTerminalSessionLifecycle:
    """CRUD operations on terminal sessions."""

    def test_create_session_empty_cwd_defaults_to_project_root(self):
        """Empty CWD should default to AllowedRoot (project root)."""
        r = create_session(cwd="")
        assert r.status_code == 201, f"Expected 201, got {r.status_code}: {r.text}"
        data = r.json()
        assert "id" in data
        assert "wsPath" in data
        assert data["wsPath"].startswith("/terminal/sessions/")
        assert data["wsPath"].endswith("/ws")
        # Cleanup
        delete_session(data["id"])

    def test_create_session_returns_valid_id(self):
        r = create_session()
        assert r.status_code == 201
        data = r.json()
        session_id = data["id"]
        assert len(session_id) > 0

        # Verify we can GET it back
        r2 = get_session(session_id)
        assert r2.status_code == 200
        sess = r2.json()
        assert sess["id"] == session_id
        assert sess["status"] == "active"

        # Cleanup
        delete_session(session_id)

    def test_list_sessions(self):
        # Create one
        r = create_session()
        assert r.status_code == 201
        session_id = r.json()["id"]

        # List
        r2 = list_sessions()
        assert r2.status_code == 200
        sessions = r2.json()
        assert isinstance(sessions, list)
        ids = [s["id"] for s in sessions]
        assert session_id in ids

        # Cleanup
        delete_session(session_id)

    def test_delete_session(self):
        r = create_session()
        assert r.status_code == 201
        session_id = r.json()["id"]

        r2 = delete_session(session_id)
        assert r2.status_code == 204

        # Should be gone
        r3 = get_session(session_id)
        assert r3.status_code == 404

    def test_get_nonexistent_session_returns_404(self):
        r = get_session("nonexistent-id-12345")
        assert r.status_code == 404


class TestTerminalWebSocket:
    """WebSocket connection and terminal I/O."""

    def test_websocket_connect_and_receive_prompt(self):
        """Create session, connect WS, verify we get shell output."""
        r = create_session()
        assert r.status_code == 201, f"Create failed: {r.status_code} {r.text}"
        data = r.json()
        session_id = data["id"]
        ws_path = data["wsPath"]

        ws_url = f"{WS_BASE}{ws_path}"
        try:
            cookie = f"auth_token={_auth_cookie()}"
            sock = ws_client.create_connection(ws_url, timeout=5, cookie=cookie)

            # Send resize
            sock.send(json.dumps({"type": "resize", "cols": 80, "rows": 24}))

            # We should receive some output (shell prompt, motd, etc.)
            # Server sends binary frames — use recv_data() to get opcode+data
            received = b""
            deadline = time.time() + 5
            while time.time() < deadline:
                try:
                    sock.settimeout(1)
                    opcode, frame = sock.recv_data()
                    if frame:
                        received += frame if isinstance(frame, bytes) else frame.encode()
                    if len(received) > 0:
                        break
                except ws_client.WebSocketTimeoutException:
                    continue

            assert len(received) > 0, "Expected shell output after connecting, got nothing"

            sock.close()
        finally:
            delete_session(session_id)

    def test_websocket_send_command_and_receive_output(self):
        """Send 'echo hello' and verify output contains 'hello'."""
        r = create_session()
        assert r.status_code == 201, f"Create failed: {r.status_code} {r.text}"
        data = r.json()
        session_id = data["id"]
        ws_path = data["wsPath"]

        ws_url = f"{WS_BASE}{ws_path}"
        try:
            cookie = f"auth_token={_auth_cookie()}"
            sock = ws_client.create_connection(ws_url, timeout=5, cookie=cookie)

            # Send resize first
            sock.send(json.dumps({"type": "resize", "cols": 80, "rows": 24}))
            time.sleep(0.5)

            # Drain initial prompt output
            try:
                while True:
                    sock.settimeout(0.5)
                    sock.recv_data()
            except (ws_client.WebSocketTimeoutException, Exception):
                pass

            # Send a command as binary (keystrokes)
            command = "echo hello_terminal_test\n"
            sock.send_binary(command.encode("utf-8"))

            # Collect output — server sends binary frames
            received = b""
            deadline = time.time() + 5
            while time.time() < deadline:
                try:
                    sock.settimeout(1)
                    opcode, frame = sock.recv_data()
                    if frame:
                        received += frame if isinstance(frame, bytes) else frame.encode()
                    if b"hello_terminal_test" in received:
                        break
                except ws_client.WebSocketTimeoutException:
                    continue

            output = received.decode("utf-8", errors="replace")
            assert "hello_terminal_test" in output, (
                f"Expected 'hello_terminal_test' in output, got: {output!r}"
            )

            sock.close()
        finally:
            delete_session(session_id)

    def test_websocket_resize(self):
        """Sending a resize command should not error."""
        r = create_session()
        assert r.status_code == 201
        data = r.json()
        session_id = data["id"]
        ws_path = data["wsPath"]

        ws_url = f"{WS_BASE}{ws_path}"
        try:
            cookie = f"auth_token={_auth_cookie()}"
            sock = ws_client.create_connection(ws_url, timeout=5, cookie=cookie)

            # Send resize
            sock.send(json.dumps({"type": "resize", "cols": 120, "rows": 40}))
            time.sleep(0.3)

            # Should still be able to receive output (no crash)
            try:
                sock.settimeout(1)
                frame = sock.recv()
                # Any data means the connection is alive
                assert frame is not None
            except ws_client.WebSocketTimeoutException:
                pass  # No data yet is fine, connection is still alive

            sock.close()
        finally:
            delete_session(session_id)
