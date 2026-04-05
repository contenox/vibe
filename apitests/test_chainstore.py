import uuid
import urllib.parse
from typing import Any, Dict

import requests

from helpers import assert_status_code


def _taskchains_url(base_url: str, vfs_path: str) -> str:
    """base_url already includes /api (see CONTENOX_API_URL)."""
    root = base_url.rstrip("/")
    q = urllib.parse.urlencode({"path": vfs_path})
    return f"{root}/taskchains?{q}"


def generate_test_chain(id_suffix: str = "") -> Dict[str, Any]:
    if id_suffix is None:
        id_suffix = str(uuid.uuid4())[:8]
    chain_id = f"test-chain-{id_suffix}"
    return {
        "id": chain_id,
        "debug": False,
        "description": "A test task chain for API testing",
        "token_limit": 4096,
        "tasks": [
            {
                "id": "task1",
                "handler": "prompt_to_string",
                "prompt_template": "Hello, world!",
                "transition": {
                    "branches": [{"when": "", "operator": "default", "goto": "end"}],
                },
            }
        ],
    }


def test_create_task_chain(base_url):
    chain = generate_test_chain()
    vfs_path = f"apitest-{chain['id']}.json"

    response = requests.post(_taskchains_url(base_url, vfs_path), json=chain)
    assert_status_code(response, 201)

    created_chain = response.json()
    assert created_chain["id"] == chain["id"], "ID mismatch in created task chain"
    assert created_chain["description"] == chain["description"], "Description mismatch"
    assert len(created_chain["tasks"]) == 1, "Task count mismatch"
    assert created_chain["tasks"][0]["id"] == "task1", "Task ID mismatch"

    delete_response = requests.delete(_taskchains_url(base_url, vfs_path))
    assert_status_code(delete_response, 200)


def test_get_task_chain(base_url):
    chain = generate_test_chain()
    vfs_path = f"apitest-{chain['id']}.json"
    create_response = requests.post(_taskchains_url(base_url, vfs_path), json=chain)
    assert_status_code(create_response, 201)

    get_response = requests.get(_taskchains_url(base_url, vfs_path))
    assert_status_code(get_response, 200)

    retrieved_chain = get_response.json()
    assert retrieved_chain["id"] == chain["id"], "ID mismatch in retrieved task chain"
    assert retrieved_chain["description"] == chain["description"], "Description mismatch"
    assert len(retrieved_chain["tasks"]) == 1, "Task count mismatch"

    delete_response = requests.delete(_taskchains_url(base_url, vfs_path))
    assert_status_code(delete_response, 200)


def test_update_task_chain(base_url):
    chain = generate_test_chain()
    vfs_path = f"apitest-{chain['id']}.json"
    create_response = requests.post(_taskchains_url(base_url, vfs_path), json=chain)
    assert_status_code(create_response, 201)
    created_chain = create_response.json()

    updated_chain = {
        "id": created_chain["id"],
        "debug": False,
        "description": "Updated description",
        "token_limit": 4096,
        "tasks": [
            {
                "id": "task1",
                "handler": "prompt_to_string",
                "prompt_template": "Hello, updated world!",
                "transition": {
                    "branches": [{"when": "", "operator": "default", "goto": "end"}],
                },
            },
            {
                "id": "task2",
                "handler": "noop",
                "prompt_template": "",
                "transition": {
                    "branches": [{"when": "", "operator": "default", "goto": "end"}],
                },
            },
        ],
    }

    update_response = requests.put(_taskchains_url(base_url, vfs_path), json=updated_chain)
    assert_status_code(update_response, 200)

    actual_updated = update_response.json()
    assert actual_updated["description"] == "Updated description"
    assert len(actual_updated["tasks"]) == 2
    assert actual_updated["tasks"][1]["id"] == "task2"

    delete_response = requests.delete(_taskchains_url(base_url, vfs_path))
    assert_status_code(delete_response, 200)


def test_delete_task_chain(base_url):
    chain = generate_test_chain()
    vfs_path = f"apitest-{chain['id']}.json"
    create_response = requests.post(_taskchains_url(base_url, vfs_path), json=chain)
    assert_status_code(create_response, 201)

    delete_response = requests.delete(_taskchains_url(base_url, vfs_path))
    assert_status_code(delete_response, 200)

    get_response = requests.get(_taskchains_url(base_url, vfs_path))
    assert_status_code(get_response, 404)


def test_taskchains_get_requires_path(base_url):
    root = base_url.rstrip("/")
    r = requests.get(f"{root}/taskchains")
    assert_status_code(r, 400)


def test_get_nonexistent_task_chain(base_url):
    vfs_path = f"nonexistent-chain-{uuid.uuid4()}.json"
    response = requests.get(_taskchains_url(base_url, vfs_path))
    assert_status_code(response, 404)


def test_delete_nonexistent_task_chain(base_url):
    vfs_path = f"nonexistent-chain-{uuid.uuid4()}.json"
    response = requests.delete(_taskchains_url(base_url, vfs_path))
    assert_status_code(response, 404)


def test_task_chain_full_workflow(base_url):
    chain = generate_test_chain()
    vfs_path = f"apitest-{chain['id']}.json"
    create_response = requests.post(_taskchains_url(base_url, vfs_path), json=chain)
    assert_status_code(create_response, 201)
    created_chain = create_response.json()

    assert created_chain["id"] == chain["id"]
    assert len(created_chain["tasks"]) == 1

    created_chain["tasks"].append(
        {
            "id": "task2",
            "handler": "noop",
            "prompt_template": "",
            "transition": {
                "branches": [{"when": "", "operator": "default", "goto": "end"}],
            },
        }
    )

    update_response = requests.put(_taskchains_url(base_url, vfs_path), json=created_chain)
    assert_status_code(update_response, 200)
    updated_chain = update_response.json()

    assert len(updated_chain["tasks"]) == 2

    get_response = requests.get(_taskchains_url(base_url, vfs_path))
    assert_status_code(get_response, 200)
    fetched_chain = get_response.json()
    assert len(fetched_chain["tasks"]) == 2

    delete_response = requests.delete(_taskchains_url(base_url, vfs_path))
    assert_status_code(delete_response, 200)

    final_get_response = requests.get(_taskchains_url(base_url, vfs_path))
    assert_status_code(final_get_response, 404)
