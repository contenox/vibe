import pytest
import os
import uuid
import requests
import logging
import time
from pytest_httpserver import HTTPServer
from typing import Generator, Any
import json
from werkzeug.wrappers import Response


BASE_URL = os.environ.get("CONTENOX_API_URL", "http://localhost:8081/api")

# Ollama base URL for health checks and POST /backends — only from OLLAMA_HOST (see Makefile).
# Default must stay in sync with OLLAMA_HOST in the Makefile.
def _ollama_base_url() -> str:
    host = os.environ.get("OLLAMA_HOST", "127.0.0.1:11434").strip()
    if host.startswith("http://") or host.startswith("https://"):
        return host.rstrip("/")
    return f"http://{host}"


DEFAULT_POLL_INTERVAL = 3
DEFAULT_TIMEOUT = 420

# Configure the root logger
logging.basicConfig(level=logging.INFO,
                    format='%(asctime)s - %(levelname)s - %(name)s - %(message)s')

# Create a logger object for your test module
logger = logging.getLogger(__name__)


def _get_json(url: str):
    r = requests.get(url, timeout=60)
    r.raise_for_status()
    return r.json()


def _find_backend_by_base_url(base_url: str, ollama_base: str):
    for b in _get_json(f"{base_url}/backends"):
        if b.get("baseUrl") == ollama_base and b.get("type") == "ollama":
            return b
    return None


def _ensure_backend(base_url: str, payload: dict) -> dict:
    """Create backend if missing, else reuse (idempotent across pytest runs on same DB)."""
    ollama = payload["baseUrl"]
    existing = _find_backend_by_base_url(base_url, ollama)
    if existing:
        return existing
    r = requests.post(f"{base_url}/backends", json=payload, timeout=60)
    if r.status_code == 201:
        return r.json()
    if r.status_code == 409:
        existing = _find_backend_by_base_url(base_url, ollama)
        if existing:
            return existing
    r.raise_for_status()


def _backend_in_group(base_url: str, group_id: str, backend_id: str) -> bool:
    for b in _get_json(f"{base_url}/backend-affinity/{group_id}/backends"):
        if b.get("id") == backend_id:
            return True
    return False


def _ensure_backend_in_group(base_url: str, group_id: str, backend_id: str):
    if _backend_in_group(base_url, group_id, backend_id):
        return
    r = requests.post(
        f"{base_url}/backend-affinity/{group_id}/backends/{backend_id}", timeout=60
    )
    r.raise_for_status()
    assert r.json() == "backend assigned"


def _observed_model_name(entry: dict) -> str:
    return (entry.get("model") or entry.get("name") or "").strip()


def _choose_runtime_model(base_url: str, backend_id: str) -> dict:
    url = f"{base_url}/backends/{backend_id}"
    start_time = time.time()

    while True:
        data = _get_json(url)
        pulled_models = data.get("pulledModels", [])
        if pulled_models:
            chat_models = [m for m in pulled_models if m.get("canChat")]
            chosen = chat_models[0] if chat_models else pulled_models[0]
            model_name = _observed_model_name(chosen)
            if model_name:
                return {"id": model_name, "model": model_name}

        elapsed = time.time() - start_time
        if elapsed > DEFAULT_TIMEOUT:
            pytest.fail(
                f"Timed out waiting for an observed model on backend '{backend_id}'. Last backend payload: {data}"
            )

        logger.info("⏳ Waiting for observed runtime models on backend '%s'...", backend_id)
        time.sleep(DEFAULT_POLL_INTERVAL)


def _model_in_group(base_url: str, group_id: str, model_id: str) -> bool:
    for m in _get_json(f"{base_url}/model-affinity/{group_id}/models"):
        if m.get("id") == model_id:
            return True
    return False


def _ensure_model_in_group(base_url: str, group_id: str, model_id: str):
    if _model_in_group(base_url, group_id, model_id):
        return
    r = requests.post(
        f"{base_url}/model-affinity/{group_id}/models/{model_id}", timeout=60
    )
    r.raise_for_status()
    assert r.json() == "model assigned"


def check_function_call_request(request):
    """Asserts the request body is a valid OpenAI-style FunctionCall."""
    try:
        body = request.get_json()
        assert "name" in body, "Request body missing 'name'"
        assert "arguments" in body, "Request body missing 'arguments'"

        # Arguments should be a string containing valid JSON
        try:
            json.loads(body["arguments"])
        except (json.JSONDecodeError, TypeError):
            pytest.fail("'arguments' field is not a valid JSON string")

        return True
    except Exception as e:
        pytest.fail(f"Request body is not a valid FunctionCall: {e}")


@pytest.fixture(scope="session")
def base_url():
    logger.debug("Providing base URL: %s", BASE_URL)
    return BASE_URL


def _beam_origin_url() -> str:
    """Origin for routes outside /api (embedded OpenAPI, /docs, SPA)."""
    api = os.environ.get("CONTENOX_API_URL", "http://localhost:8081/api").rstrip("/")
    if api.endswith("/api"):
        return api[: -len("/api")] or "http://localhost:8081"
    return os.environ.get("CONTENOX_BEAM_ORIGIN", "http://localhost:8081").rstrip("/")


@pytest.fixture(scope="session")
def beam_origin():
    o = _beam_origin_url()
    logger.debug("Beam origin (non-/api routes): %s", o)
    return o


@pytest.fixture(scope="session")
def with_ollama_backend():
    """Check if the Ollama backend is reachable and return its base URL."""
    base = _ollama_base_url()
    try:
        response = requests.get(f"{base}/api/tags", timeout=5)
        response.raise_for_status()
        logger.info("Ollama backend is reachable at %s", base)
    except requests.RequestException as e:
        logger.error("Ollama backend not reachable: %s", e)
        pytest.fail(
            f"Ollama backend check failed: {e}. "
            f"Ensure Ollama is running and OLLAMA_HOST matches (e.g. curl http://$OLLAMA_HOST/api/tags)."
        )
    return base


@pytest.fixture(scope="session")
def create_backend_and_assign_to_group(base_url, with_ollama_backend):
    """
    Fixture that creates a backend and assigns it to the 'internal_embed_group'.
    Returns: dict containing backend_id and group_id
    """
    ollama_url = with_ollama_backend

    payload = {
        "name": "Test Embedder Backend",
        "baseUrl": ollama_url,
        "type": "ollama",
    }
    backend = _ensure_backend(base_url, payload)
    backend_id = backend["id"]
    backend_url = backend["baseUrl"]

    for group_id in (
        "internal_embed_group",
        "internal_chat_group",
        "internal_tasks_group",
    ):
        _ensure_backend_in_group(base_url, group_id, backend_id)
        logger.info("Backend %s assigned to group %s", backend_id, group_id)

    group_id = "internal_tasks_group"

    yield {
        "backend_id": backend_id,
        "backend_url": backend_url,
        "group_id": group_id,
        "backend": backend,
    }

@pytest.fixture(scope="session")
def create_model_and_assign_to_group(base_url, create_backend_and_assign_to_group):
    """
    Choose one runtime-observed model from the test backend and expose its name.
    The fixture name is kept for compatibility with existing tests.
    """
    backend_id = create_backend_and_assign_to_group["backend_id"]
    group_id = "internal_tasks_group"
    model = _choose_runtime_model(base_url, backend_id)
    model_id = model["id"]
    model_name = model["model"]
    logger.info("Using observed runtime model %s from backend %s", model_name, backend_id)

    yield {
        "model_id": model_id,
        "model_name": model_name,
        "group_id": group_id,
    }

@pytest.fixture(scope="session")
def wait_for_model_in_backend(base_url):
    """
    Enhanced fixture that waits for model download with error handling and progress tracking
    """
    def _wait_for_model(*, model_name, backend_id, timeout=DEFAULT_TIMEOUT, poll_interval=DEFAULT_POLL_INTERVAL):
        url = f"{base_url}/backends/{backend_id}"
        start_time = time.time()
        last_status = None
        download_started = False

        while True:
            try:
                response = requests.get(url)
                if response.status_code != 200:
                    logger.warning("Failed to fetch backend info: %s", response.text)
                    time.sleep(poll_interval)
                    continue

                data = response.json()
                pulled_models = data.get("pulledModels", [])

                # Check for backend errors
                if data.get("error"):
                    error_msg = data["error"]
                    logger.error("Backend error: %s", error_msg)

                # Check if download has started
                if not download_started and any(
                    _observed_model_name(m) == model_name for m in pulled_models
                ):
                    logger.info("✅ Model download started for '%s'", model_name)
                    download_started = True

                # Check for completed download
                model_details = next(
                    (m for m in pulled_models if _observed_model_name(m) == model_name),
                    None,
                )
                if model_details:
                    # Verify successful download
                    if model_details.get("size", 1) > 0 and not model_details.get("error"):
                        logger.info("✅ Model '%s' fully downloaded to backend '%s'", model_name, backend_id)
                        return data
                    elif model_details.get("error"):
                        pytest.fail(f"Model download failed: {model_details['error']}")

                # Report progress if available
                if model_details and model_details.get("progress"):
                    progress = model_details["progress"]
                    if progress != last_status:
                        logger.info("📥 Download progress: %s", progress)
                        last_status = progress

                # Handle timeout
                elapsed = time.time() - start_time
                if elapsed > timeout:
                    pytest.fail(
                        f"⏰ Timed out waiting for model '{model_name}' in backend '{backend_id}'\n"
                        f"Elapsed: {elapsed:.0f}s | Last backend status: {data}"
                    )

                logger.info("⏳ Waiting for model '%s' in backend '%s'...", model_name, backend_id)
                time.sleep(poll_interval)

            except requests.RequestException as e:
                logger.warning("Network error while polling backend: %s", str(e))
                time.sleep(poll_interval)

    return _wait_for_model

@pytest.fixture(scope="session")
def httpserver(request) -> Generator[HTTPServer, Any, Any]:
    """
    Session-scoped httpserver fixture.
    This overrides the default function-scoped httpserver from pytest-httpserver.
    It allows other session-scoped fixtures to configure the server.
    """
    server = HTTPServer(host="0.0.0.0", port=0) # Initialize with 0.0.0.0 and dynamic port
    logger.info(f"Attempting to start HTTPServer on {server.host}:{server.port}...")
    server.start()
    logger.info(f"HTTPServer is running and accessible at: {server.url_for('/')}")
    yield server
    server.stop()
    server.clear()

@pytest.fixture(scope="function")
def mock_hook_server(httpserver: HTTPServer):
    endpoint = "/test-hook-endpoint"
    # Default response is a simple JSON object now
    httpserver.expect_request(endpoint, method="POST").respond_with_json({"status": "ok"})
    full_mock_url = httpserver.url_for(suffix=endpoint)
    logger.info(f"Mock hook server endpoint registered at: {full_mock_url}")
    return {
        "url": full_mock_url,
        "server": httpserver
    }

@pytest.fixture
def configurable_mock_hook_server(httpserver: HTTPServer):
    def _setup_mock(status_code=200, response_json=None, delay_seconds=0, expected_headers=None, request_validator=None, tool_name=""):
        if response_json is None:
            response_json = {"status": "default_ok"}

        if expected_headers is None:
            expected_headers = {}

        # Clear any existing expectations to prevent test pollution
        httpserver.clear()

        # Create a unique base path for this test
        base_path = f"/test-hook-endpoint-{uuid.uuid4().hex[:8]}"
        print(f"Setting up mock endpoint: {base_path} for test: {os.environ.get('PYTEST_CURRENT_TEST')}")

        def handler(request):
            if delay_seconds > 0:
                time.sleep(delay_seconds)

            # Validate request format if a validator is provided
            if request_validator:
                try:
                    request_validator(request)
                except Exception as e:
                    return Response(
                        json.dumps({"error": f"Request validation failed: {str(e)}"}),
                        400,
                        {"Content-Type": "application/json"}
                    )

            if expected_headers:
                for header_name, expected_value in expected_headers.items():
                    actual_value = request.headers.get(header_name)
                    if actual_value != expected_value:
                        return Response(
                            json.dumps({
                                "error": f"Header validation failed: {header_name} expected={expected_value}, got={actual_value}"
                            }),
                            400,
                            {"Content-Type": "application/json"}
                        )

            return Response(json.dumps(response_json), status_code, {"Content-Type": "application/json"})

        # Set up the tool endpoint
        tool_path = f"{base_path}/{tool_name}" if tool_name else base_path
        httpserver.expect_request(
            tool_path,
            method="POST"
        ).respond_with_handler(handler)

        return {
            "url": httpserver.url_for(base_path),
            "base_path": base_path,
            "tool_path": tool_path,
            "server": httpserver,
            "expected_headers": expected_headers
        }

    return _setup_mock

API_TOKEN = "my-secret-test-token"

@pytest.fixture(scope="session")
def auth_headers():
    """
    Fixture that provides authentication headers with a constant token.
    """
    headers = {
        "X-API-Key": API_TOKEN
    }
    return headers

EMBED_MODEL_NAME = "nomic-embed-text:latest"
TASK_MODEL_NAME = "phi3:3.8b"

@pytest.fixture(scope="session")
def wait_for_declared_models(
    create_backend_and_assign_to_group,
    create_model_and_assign_to_group,
    wait_for_model_in_backend,
):
    """Wait until the chosen runtime model is visible on the session test backend."""
    backend_id = create_backend_and_assign_to_group["backend_id"]
    model_name = create_model_and_assign_to_group["model_name"]
    logger.info("⏳ Waiting for session test model %r on backend %s", model_name, backend_id)
    wait_for_model_in_backend(model_name=model_name, backend_id=backend_id)
    logger.info("✅ Session test model ready: %s", model_name)
