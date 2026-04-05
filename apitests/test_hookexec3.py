import uuid
import requests

from helpers import hook_mock_endpoint_url


def check_openapi_tool_call_request(request):
    """Asserts the request body is valid for an OpenAPI tool call."""
    body = request.get_json()
    assert "input" in body, "Request body missing 'input'"
    return True


def test_remote_hook_with_headers(
    base_url,
    auth_headers,
    configurable_mock_hook_server
):
    """Test that remote hooks can define custom headers that are sent to the target endpoint"""
    expected_headers = {
        "X-GitHub-Access-Token": "ghp_test1234567890",
        "X-Custom-Header": "custom-value",
        "Authorization": "Bearer token123"
    }

    expected_response_json = {
        "status": "ok",
        "message": "Hook executed successfully with correct headers"
    }
    tool_name = f"test_operation_{uuid.uuid4().hex[:6]}"

    # Set up mock server with header and request format validation.
    mock_server = configurable_mock_hook_server(
        status_code=200,
        response_json=expected_response_json,
        expected_headers=expected_headers,
        request_validator=check_openapi_tool_call_request,
        tool_name=tool_name  # Changed from tool_handler_name to tool_name
    )

    # Define and serve OpenAPI schema
    openapi_schema = {
        "openapi": "3.0.0",
        "info": {"title": "Test API", "version": "1.0.0"},
        "paths": {
            f"/{tool_name}": {  # Added leading slash
                "post": {
                    "operationId": tool_name,
                    "requestBody": {
                        "required": True,
                        "content": {
                            "application/json": {
                                "schema": {
                                    "type": "object",
                                    "properties": {
                                        "input": {"type": "string"},
                                        "test_param": {"type": "string"}
                                    },
                                    "required": ["input"]
                                }
                            }
                        }
                    },
                    "responses": {
                        "200": {
                            "description": "OK",
                            "content": {"application/json": {"schema": {"type": "object"}}}
                        }
                    }
                }
            }
        }
    }

    # Serve the OpenAPI schema at the base path
    mock_server["server"].expect_request(
        f"{mock_server['base_path']}/openapi.json",  # Use base_path
        method="GET"
    ).respond_with_json(openapi_schema)

    # Serve the tool endpoint at the tool path
    mock_server["server"].expect_request(
        f"{mock_server['base_path']}/{tool_name}",  # Use base_path + tool_name
        method="POST"
    ).respond_with_json(expected_response_json)

    hook_name = f"test-headers-hook-{uuid.uuid4().hex[:8]}"
    endpoint_url = hook_mock_endpoint_url(mock_server["url"], mock_server["base_path"])

    create_response = requests.post(
        f"{base_url}/hooks/remote",
        json={
            "name": hook_name,
            "endpointUrl": endpoint_url,
            "timeoutMs": 5000,
            "headers": expected_headers,
        },
        headers=auth_headers
    )
    assert create_response.status_code == 201

    get_response = requests.get(
        f"{base_url}/hooks/remote/{create_response.json()['id']}",
        headers=auth_headers
    )
    assert get_response.status_code == 200
    hook = get_response.json()
    assert hook["headers"] == expected_headers

    # Define a task chain that uses the hook
    task_chain = {
        "id": "headers-test-chain",
        "debug": True,
        "description": "Test chain with header validation",
        "tasks": [
            {
                "id": "header_hook_task",
                "handler": "hook",
                "hook": {
                    "name": hook_name,
                    "tool_name": tool_name,
                    "args": {"test_param": "test_value"}
                },
                "transition": {
                    "branches": [{"operator": "default", "goto": "end"}]
                }
            }
        ],
        "token_limit": 4096
    }

    # Execute the task chain
    response = requests.post(
        f"{base_url}/tasks",
        json={
            "input": "Test header validation",
            "chain": task_chain,
            "inputType": "string"
        },
        headers=auth_headers
    )

    # Verify the response indicates success
    assert response.status_code == 200
    data = response.json()
    assert "output" in data
    assert data["output"] == expected_response_json

    # Verify the mock server was called
    assert len(mock_server["server"].log) >= 2, "Mock server not called enough times"


def test_remote_hook_without_headers(
    base_url,
    auth_headers,
    configurable_mock_hook_server
):
    """Test that remote hooks work correctly without custom headers"""
    expected_response_json = {"message": "Hook executed successfully without custom headers"}
    tool_name = f"test_no_headers_{uuid.uuid4().hex[:6]}"

    mock_server = configurable_mock_hook_server(
        status_code=200,
        response_json=expected_response_json,
        request_validator=check_openapi_tool_call_request,
        tool_name=tool_name  # Changed from tool_handler_name to tool_name
    )

    # Define and serve OpenAPI schema
    openapi_schema = {
        "openapi": "3.0.0",
        "info": {"title": "Test API", "version": "1.0.0"},
        "paths": {
            f"/{tool_name}": {  # Added leading slash
                "post": {
                    "operationId": tool_name,
                    "requestBody": {
                        "required": True,
                        "content": {
                            "application/json": {
                                "schema": {
                                    "type": "object",
                                    "properties": {
                                        "input": {"type": "string"}
                                    },
                                    "required": ["input"]
                                }
                            }
                        }
                    },
                    "responses": {
                        "200": {
                            "description": "OK",
                            "content": {"application/json": {"schema": {"type": "object"}}}
                        }
                    }
                }
            }
        }
    }

    # Serve the OpenAPI schema at the base path
    mock_server["server"].expect_request(
        f"{mock_server['base_path']}/openapi.json",  # Use base_path
        method="GET"
    ).respond_with_json(openapi_schema)

    # Serve the tool endpoint at the tool path
    mock_server["server"].expect_request(
        f"{mock_server['base_path']}/{tool_name}",  # Use base_path + tool_name
        method="POST"
    ).respond_with_json(expected_response_json)

    hook_name = f"test-no-headers-hook-{uuid.uuid4().hex[:8]}"
    endpoint_url = hook_mock_endpoint_url(mock_server["url"], mock_server["base_path"])

    create_response = requests.post(
        f"{base_url}/hooks/remote",
        json={
            "name": hook_name,
            "endpointUrl": endpoint_url,
            "timeoutMs": 5000,
        },
        headers=auth_headers
    )
    assert create_response.status_code == 201

    get_response = requests.get(
        f"{base_url}/hooks/remote/{create_response.json()['id']}",
        headers=auth_headers
    )
    assert get_response.status_code == 200
    hook = get_response.json()
    assert "headers" not in hook or not hook["headers"]

    task_chain = {
        "id": "no-headers-test-chain",
        "debug": True,
        "tasks": [{
            "id": "no_header_hook_task",
            "handler": "hook",
            "hook": {"name": hook_name, "tool_name": tool_name, "args": {}},
            "transition": {"branches": [{"operator": "default", "goto": "end"}]}
        }],
        "token_limit": 4096
    }

    response = requests.post(
        f"{base_url}/tasks",
        json={"input": "Test without custom headers", "chain": task_chain, "inputType": "string"},
        headers=auth_headers
    )

    assert response.status_code == 200
    data = response.json()
    assert "output" in data
    assert data["output"] == expected_response_json
    assert len(mock_server["server"].log) >= 2, "Mock server not called enough times"
