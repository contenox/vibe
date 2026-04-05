import requests
from helpers import assert_status_code


def test_model_mutation_endpoints_are_not_exposed(base_url):
    payload = {
        "model": "should-not-be-created",
        "canPrompt": True,
        "contextLength": 2048,
    }
    assert requests.post(f"{base_url}/models", json=payload).status_code == 404
    assert requests.put(f"{base_url}/models/should-not-be-created", json=payload).status_code == 404
    assert requests.delete(f"{base_url}/models/should-not-be-created").status_code == 404


def test_list_models(base_url):
    response = requests.get(f"{base_url}/openai/v1/models")
    assert_status_code(response, 200)

    data = response.json()
    assert data["object"] == "list", "Expected list object"
    assert isinstance(data["data"], list), "Data should be a list"
    assert len(data["data"]) > 0, "No models found"
    for model in data["data"]:
        assert "id" in model
        assert model["object"] == "model"


def test_internal_models_endpoint_returns_observed_inventory(base_url):
    response = requests.get(f"{base_url}/models")
    assert_status_code(response, 200)

    models = response.json()
    assert isinstance(models, list)
    assert len(models) > 0

    for model in models:
        assert "id" in model
        assert "model" in model
        assert "contextLength" in model
        assert "canChat" in model


def test_internal_models_match_openai_compatible_listing(base_url):
    internal_models = requests.get(f"{base_url}/models")
    assert_status_code(internal_models, 200)
    internal_names = {model["model"] for model in internal_models.json()}

    openai_models = requests.get(f"{base_url}/openai/v1/models")
    assert_status_code(openai_models, 200)
    openai_names = {model["id"] for model in openai_models.json()["data"]}

    assert internal_names == openai_names

def test_internal_models_endpoint(base_url):
    response = requests.get(f"{base_url}/backends")
    assert_status_code(response, 200)

    backends = response.json()
    assert isinstance(backends, list)
    for backend in backends:
        if backend.get("error"):
            assert backend.get("models", []) == []
            continue

        observed_names = {model["model"] for model in backend.get("pulledModels", [])}
        assert set(backend.get("models", [])) == observed_names

def test_get_default_model(base_url):
    """Test getting the default model"""
    response = requests.get(f"{base_url}/defaultmodel")
    assert_status_code(response, 200)

    model = response.json()
    assert "modelName" in model

def test_default_model_consistency(base_url):
    """Test that the default model is consistent across calls"""
    response1 = requests.get(f"{base_url}/defaultmodel")
    assert_status_code(response1, 200)

    response2 = requests.get(f"{base_url}/defaultmodel")
    assert_status_code(response2, 200)

    # Default model should be the same across calls
    assert response1.json()["modelName"] == response2.json()["modelName"]
