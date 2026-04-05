import os
import uuid
from datetime import datetime, timezone
from urllib.parse import urlparse


def hook_mock_endpoint_url(mock_url: str, base_path: str) -> str:
    """URL the API server uses to reach the pytest HTTPServer (remote hook target).

    The mock may advertise ``0.0.0.0``; the runtime must call loopback or a host IP
    that reaches the same process. Default ``127.0.0.1`` matches host-local
    ``contenox beam`` + pytest.

    Set ``APITEST_HOOK_MOCK_HOST`` (e.g. ``host.docker.internal``) when the server
    runs in Docker and must reach a mock opened on the host.
    """
    parsed = urlparse(mock_url)
    if parsed.port is None:
        raise ValueError(f"hook mock URL has no port: {mock_url!r}")
    host = os.environ.get("APITEST_HOOK_MOCK_HOST", "127.0.0.1")
    return f"http://{host}:{parsed.port}{base_path}"


def assert_status_code(response, expected_status):
    if response.status_code != expected_status:
        print("\nResponse body on failure:")
        print(response.text)
    assert response.status_code == expected_status


def get_auth_headers(token):
    """Return the authorization header for a given token."""
    return {"Authorization": f"Bearer {token}"}


def generate_unique_name(prefix: str) -> str:
    """Generate a unique name with the given prefix."""
    return f"{prefix}-{str(uuid.uuid4())[:8]}"


def generate_test_event_payload():
    return {
        "id": str(uuid.uuid4()),
        "type": "user.created",
        "source": "auth-service",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "data": {
            "user_id": str(uuid.uuid4()),
            "email": "test@example.com",
            "name": "Test User",
        },
        "metadata": {"ip": "192.168.1.1", "user_agent": "test-client/1.0"},
    }
