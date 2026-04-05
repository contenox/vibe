import requests
from helpers import assert_status_code
import uuid

def test_create_backend(base_url):
    # Use a test-specific URL
    payload = {
        "name": f"Test backend {uuid.uuid4().hex[:10]}",
        "baseUrl": "http://test-backend:11434",
        "type": "ollama",
    }
    response = requests.post(f"{base_url}/backends", json=payload)
    assert_status_code(response, 201)
    backend = response.json()
    backend_id = backend["id"]

    # Clean up immediately
    delete_url = f"{base_url}/backends/{backend_id}"
    del_response = requests.delete(delete_url)
    assert_status_code(del_response, 200)

def test_backend_assigned_to_group(base_url, create_backend_and_assign_to_group):
    data = create_backend_and_assign_to_group
    backend_id = data["backend_id"]
    group_id = data["group_id"]

    # Verify assignment
    list_url = f"{base_url}/backend-affinity/{group_id}/backends"
    response = requests.get(list_url)
    assert_status_code(response, 200)
    backends = response.json()
    assert any(b['id'] == backend_id for b in backends), "Backend not found in group"

def test_list_backends(base_url):
    response = requests.get(f"{base_url}/backends")
    assert_status_code(response, 200)
    backends = response.json()
    assert isinstance(backends, list)

def test_update_backend(base_url, with_ollama_backend):
    """Test updating a backend with VALID URL"""
    # Create unique URL using UUID
    unique_url = f"http://{uuid.uuid4().hex}-test:11434"

    # 1. Create a temporary backend
    payload = {
        "name": f"Temp backend {uuid.uuid4().hex[:10]}",
        "baseUrl": unique_url,  # Unique URL
        "type": "ollama",
    }
    create_response = requests.post(f"{base_url}/backends", json=payload)
    assert_status_code(create_response, 201)
    backend = create_response.json()
    backend_id = backend["id"]

    # 2. Update with new valid URL (using the same URL is fine)
    new_name = f"Updated Backend {uuid.uuid4().hex[:10]}"
    update_payload = {
        "name": new_name,
        "baseUrl": unique_url,  # Keep same URL
        "type": "ollama"
    }
    update_response = requests.put(f"{base_url}/backends/{backend_id}", json=update_payload)
    assert_status_code(update_response, 200)
    updated = update_response.json()
    assert updated["name"] == new_name

    # 3. Clean up
    delete_response = requests.delete(f"{base_url}/backends/{backend_id}")
    assert_status_code(delete_response, 200)

def test_backend_state_details(base_url, create_backend_and_assign_to_group):
    backend_id = create_backend_and_assign_to_group["backend_id"]
    response = requests.get(f"{base_url}/backends/{backend_id}")
    assert_status_code(response, 200)
    backend = response.json()
    assert "models" in backend
    assert "pulledModels" in backend
