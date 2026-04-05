import requests
from helpers import assert_status_code, hook_mock_endpoint_url
import uuid
import json


def check_openapi_request(request):
    """Asserts the request body is valid for our mock OpenAPI endpoint."""
    body = request.get_json()
    assert "input" in body, "Request body missing 'input'"
    assert "param1" in body, "Request body missing 'param1'"
    assert "param2" in body, "Request body missing 'param2'"
    return True

def test_hook_task_in_chain(
    base_url,
    with_ollama_backend,
    create_model_and_assign_to_group,
    create_backend_and_assign_to_group,
    wait_for_model_in_backend,
    configurable_mock_hook_server
):
    # Setup model and backend (unchanged)
    model_info = create_model_and_assign_to_group
    backend_info = create_backend_and_assign_to_group
    model_name = model_info["model_name"]
    backend_id = backend_info["backend_id"]
    _ = wait_for_model_in_backend(model_name=model_name, backend_id=backend_id)

    expected_hook_response = {"status": "ok", "data": "Hook executed successfully"}
    tool_name = "test_hook_task"

    mock_server = configurable_mock_hook_server(
        status_code=200,
        response_json=expected_hook_response,
        request_validator=check_openapi_request,
        tool_name=tool_name
    )

    # Define OpenAPI schema
    openapi_schema = {
        "openapi": "3.0.0",
        "info": {
            "title": "Test Hook API",
            "version": "1.0.0"
        },
        "paths": {
            f"/{tool_name}": {
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
                                        "param1": {"type": "string"},
                                        "param2": {"type": "string"}
                                    },
                                    "required": ["input", "param1", "param2"]
                                }
                            }
                        }
                    },
                    "responses": {
                        "200": {
                            "description": "Successful response",
                            "content": {
                                "application/json": {
                                    "schema": {
                                        "type": "object"
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    # Serve the OpenAPI schema at the base path
    mock_server["server"].expect_request(
        f"{mock_server['base_path']}/openapi.json",
        method="GET"
    ).respond_with_json(openapi_schema)

    # Create a remote hook
    hook_name = f"test-hook-{uuid.uuid4().hex[:8]}"
    endpoint = hook_mock_endpoint_url(mock_server["url"], mock_server["base_path"])

    create_response = requests.post(
        f"{base_url}/hooks/remote",
        json={
            "name": hook_name,
            "endpointUrl": endpoint,
            "timeoutMs": 5000,
        }
    )
    assert_status_code(create_response, 201)

    # Define a task chain that uses the hook
    task_chain = {
        "id": "hook-test-chain",
        "debug": True,
        "description": "Test chain with hook execution",
        "tasks": [
            {
                "id": "hook_task",
                "handler": "hook",
                "hook": {
                    "name": hook_name,
                    "tool_name": tool_name,
                    "args": {
                        "param1": "value1",
                        "param2": "value2"
                    }
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
            "input": "Trigger hook",
            "chain": task_chain,
            "inputType": "string"
        }
    )
    assert_status_code(response, 200)

    # Verify the final response
    data = response.json()
    assert "output" in data, "Response missing output field"
    assert data["output"] == expected_hook_response, "Unexpected hook output"

    # Verify task execution history
    assert len(data["state"]) == 1, "Should have one task in state"
    hook_task_state = data["state"][0]
    assert hook_task_state["taskHandler"] == "hook", "Wrong task handler"
    assert hook_task_state["inputType"] == "string", "Wrong input type"
    assert hook_task_state["outputType"] == "json", "Wrong output type"
    assert json.loads(hook_task_state["output"]) == expected_hook_response, "Task output mismatch"
    assert hook_task_state["transition"] == "ok", "Task transition mismatch"

    # Verify the mock server was called correctly
    assert len(mock_server["server"].log) >= 2, "Mock server not called enough times"

    # Check OpenAPI schema request
    schema_request = mock_server["server"].log[0][0]
    assert schema_request.path.endswith("/openapi.json"), "Wrong path for schema request"

    # Check tool execution request
    tool_request = mock_server["server"].log[1][0]
    assert tool_request.path.endswith(f"/{tool_name}"), "Wrong path for tool request"

    request_data = tool_request.get_json()
    assert request_data["input"] == "Trigger hook", "Input mismatch"
    assert request_data["param1"] == "value1", "Hook arg mismatch"
    assert request_data["param2"] == "value2", "Hook arg mismatch"
    assert "name" not in request_data, "OpenAPI request should not have 'name' field"
    assert "arguments" not in request_data, "OpenAPI request should not have 'arguments' field"
