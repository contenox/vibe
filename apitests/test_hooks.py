import pytest
import requests
import uuid
from helpers import assert_status_code

# Unique per process so repeated runs / shared DBs do not hit UNIQUE on remote_hooks.name
_RUN = uuid.uuid4().hex[:10]

# Test data constants (names include _RUN for idempotency)
VALID_HOOK = {
    "name": f"test-hook-{_RUN}",
    "endpointUrl": "https://example.com/webhook",
    "timeoutMs": 5000,
}

VALID_HOOK_2 = {
    "name": f"test-hook_2-{_RUN}",
    "endpointUrl": "https://example.com/webhook",
    "timeoutMs": 5000,
}


INVALID_HOOKS = [
    ({**VALID_HOOK, "name": ""}, "empty name"),
    ({**VALID_HOOK, "endpointUrl": ""}, "empty endpointUrl"),
    ({**VALID_HOOK, "timeoutMs": 0}, "zero timeout"),
    ({**VALID_HOOK, "timeoutMs": -100}, "negative timeout"),
]


def test_get_supported_hooks(base_url):
    """Test getting supported hook types"""
    response = requests.get(f"{base_url}/supported")
    assert_status_code(response, 200)

    hooks = response.json()
    assert isinstance(hooks, list)

def test_create_remote_hook_success(base_url):
    """Test successful creation of a remote hook"""
    url = f"{base_url}/hooks/remote"
    payload = VALID_HOOK.copy()

    response = requests.post(url, json=payload)
    assert_status_code(response, 201)

    data = response.json()
    assert "id" in data, "Response should contain hook ID"
    assert data["name"] == payload["name"], "Name should match"
    assert data["endpointUrl"] == payload["endpointUrl"], "Endpoint URL should match"
    assert data["timeoutMs"] == payload["timeoutMs"], "Timeout should match"

@pytest.mark.parametrize("payload,description", INVALID_HOOKS)
def test_create_remote_hook_validation_failures(base_url, payload, description):
    """Test validation failures during hook creation"""
    url = f"{base_url}/hooks/remote"
    response = requests.post(url, json=payload)
    assert_status_code(response, 422)

def test_get_remote_hook_by_id(base_url):
    """Test retrieving a hook by its ID"""
    # Create hook first
    create_url = f"{base_url}/hooks/remote"
    create_response = requests.post(create_url, json=VALID_HOOK_2)
    assert_status_code(create_response, 201)
    hook_id = create_response.json()["id"]

    # Test GET
    get_url = f"{base_url}/hooks/remote/{hook_id}"
    get_response = requests.get(get_url)
    assert_status_code(get_response, 200)

    data = get_response.json()
    assert data["id"] == hook_id, "Hook ID should match"
    assert data["name"] == VALID_HOOK_2["name"], "Name should match"

def test_list_remote_hooks(base_url):
    """Test listing all remote hooks"""
    # Clear existing hooks
    list_url = f"{base_url}/hooks/remote"
    list_response = requests.get(list_url)
    for hook in list_response.json():
        delete_url = f"{base_url}/hooks/remote/{hook['id']}"
        requests.delete(delete_url)

    # Create two hooks (names unique to this run)
    n1, n2 = f"hook-1-{_RUN}", f"hook-2-{_RUN}"
    hook1 = {**VALID_HOOK, "name": n1}
    hook2 = {**VALID_HOOK, "name": n2}

    requests.post(list_url, json=hook1)
    requests.post(list_url, json=hook2)

    # Test listing
    list_response = requests.get(list_url)
    assert_status_code(list_response, 200)

    hooks = list_response.json()
    assert len(hooks) == 2, "Should return 2 hooks"
    names = {h["name"] for h in hooks}
    assert n1 in names and n2 in names, "Both hooks should be present"

def test_update_remote_hook(base_url):
    """Test updating an existing hook"""
    # Create hook
    create_url = f"{base_url}/hooks/remote"
    create_response = requests.post(create_url, json=VALID_HOOK)
    hook_id = create_response.json()["id"]

    # Update hook
    update_url = f"{base_url}/hooks/remote/{hook_id}"
    new_name = f"updated-name-{_RUN}"
    update_payload = {
        "name": new_name,
        "endpointUrl": "https://new.example.com/webhook",
        "timeoutMs": 10000,
    }
    update_response = requests.put(update_url, json=update_payload)
    assert_status_code(update_response, 200)

    # Verify update
    get_response = requests.get(update_url)
    data = get_response.json()
    assert data["name"] == new_name, "Name should be updated"
    assert data["endpointUrl"] == update_payload["endpointUrl"], "Endpoint should be updated"
    assert data["timeoutMs"] == 10000, "Timeout should be updated"

def test_delete_remote_hook(base_url):
    """Test deleting a remote hook"""
    # Create hook
    create_url = f"{base_url}/hooks/remote"
    create_response = requests.post(create_url, json=VALID_HOOK)
    hook_id = create_response.json()["id"]

    # Delete hook
    delete_url = f"{base_url}/hooks/remote/{hook_id}"
    delete_response = requests.delete(delete_url)
    assert_status_code(delete_response, 200)

    # Verify deletion
    get_response = requests.get(delete_url)
    assert_status_code(get_response, 404)

def test_get_nonexistent_hook_returns_404(base_url):
    """Test getting a non-existent hook returns 404"""
    fake_id = str(uuid.uuid4())
    url = f"{base_url}/hooks/remote/{fake_id}"
    response = requests.get(url)
    assert_status_code(response, 404)

def test_update_nonexistent_hook_returns_404(base_url):
    """Test updating a non-existent hook returns 404"""
    fake_id = str(uuid.uuid4())
    url = f"{base_url}/hooks/remote/{fake_id}"
    response = requests.put(url, json=VALID_HOOK)
    assert_status_code(response, 404)

def test_delete_nonexistent_hook_returns_404(base_url):
    """Test deleting a non-existent hook returns 404"""
    fake_id = str(uuid.uuid4())
    url = f"{base_url}/hooks/remote/{fake_id}"
    response = requests.delete(url)
    assert_status_code(response, 404)

def test_duplicate_name_validation(base_url):
    """Test that hook names must be unique"""
    # Create first hook
    response1 = requests.post(f"{base_url}/hooks/remote", json=VALID_HOOK)
    assert_status_code(response1, 201)

    # Try to create second hook with same name
    duplicate_hook = VALID_HOOK.copy()
    duplicate_hook["endpointUrl"] = "https://different.com/hook"
    response2 = requests.post(f"{base_url}/hooks/remote", json=duplicate_hook)

    assert_status_code(response2, 409)

def test_get_remote_hook_by_name(base_url):
    """Test retrieving a hook by its unique name"""
    # Setup: Create a hook with a unique name to fetch
    hook_name = f"unique-name-for-get-test-{uuid.uuid4()}"
    payload = {**VALID_HOOK, "name": hook_name}

    create_url = f"{base_url}/hooks/remote"
    create_response = requests.post(create_url, json=payload)
    assert_status_code(create_response, 201)
    hook_id = create_response.json()["id"]

    # Act: Test the GET by-name endpoint
    get_url = f"{base_url}/hooks/remote/by-name/{hook_name}"
    get_response = requests.get(get_url)

    # Assert: Verify the response is correct
    assert_status_code(get_response, 200)
    data = get_response.json()
    assert data["id"] == hook_id
    assert data["name"] == hook_name
    assert data["endpointUrl"] == payload["endpointUrl"]

def test_get_remote_hook_by_name_not_found(base_url):
    """Test getting a hook by a non-existent name returns 404"""
    non_existent_name = f"this-name-does-not-exist-{uuid.uuid4()}"
    url = f"{base_url}/hooks/remote/by-name/{non_existent_name}"
    response = requests.get(url)
    assert_status_code(response, 404)


# ─── Inject params & headers ──────────────────────────────────────────────────

def test_create_remote_hook_with_headers(base_url):
    """Test creating a hook with HTTP headers stored and returned."""
    payload = {
        **VALID_HOOK,
        "name": f"hook-with-headers-{uuid.uuid4()}",
        "headers": {"Authorization": "Bearer secret123", "X-Tenant": "acme"},
    }
    r = requests.post(f"{base_url}/hooks/remote", json=payload)
    assert_status_code(r, 201)
    data = r.json()
    # Headers must be returned (values included at rest; CLI hides them on display)
    assert "headers" in data
    assert "Authorization" in data["headers"]
    assert "X-Tenant" in data["headers"]


def test_create_remote_hook_with_inject_params(base_url):
    """Test creating a hook with injectParams stored and returned."""
    payload = {
        **VALID_HOOK,
        "name": f"hook-with-inject-{uuid.uuid4()}",
        "injectParams": {"tenant_id": "acme", "env": "production"},
    }
    r = requests.post(f"{base_url}/hooks/remote", json=payload)
    assert_status_code(r, 201)
    data = r.json()
    assert "injectParams" in data
    assert data["injectParams"]["tenant_id"] == "acme"
    assert data["injectParams"]["env"] == "production"


def test_update_remote_hook_inject_params_replaces_all(base_url):
    """Test that updating injectParams replaces the entire map."""
    # Create with two injected params
    payload = {
        **VALID_HOOK,
        "name": f"hook-inject-update-{uuid.uuid4()}",
        "injectParams": {"tenant_id": "old", "extra": "value"},
    }
    create_r = requests.post(f"{base_url}/hooks/remote", json=payload)
    assert_status_code(create_r, 201)
    hook_id = create_r.json()["id"]

    # Update with a different set (should replace entirely)
    updated = {**payload, "injectParams": {"tenant_id": "new"}}
    update_r = requests.put(f"{base_url}/hooks/remote/{hook_id}", json=updated)
    assert_status_code(update_r, 200)

    get_r = requests.get(f"{base_url}/hooks/remote/{hook_id}")
    data = get_r.json()
    assert data["injectParams"]["tenant_id"] == "new"
    assert "extra" not in data.get("injectParams", {})
