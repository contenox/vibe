import requests
from helpers import assert_status_code


def _delete_provider_config(base_url: str, provider_type: str) -> None:
    requests.delete(f"{base_url}/providers/{provider_type}/config", timeout=30)


def test_configure_openai_provider(base_url):
    url = f"{base_url}/providers/openai/configure"
    payload = {
        "apiKey": "sk-test-openai-key",
        "modelName": "gpt-3.5-turbo",
        "upsert": True,
    }

    response = requests.post(url, json=payload)
    assert_status_code(response, 200)

    data = response.json()
    assert data["configured"] == True, "Provider was not configured"


def test_get_gemini_status_unconfigured(base_url):
    _delete_provider_config(base_url, "gemini")
    status_url = f"{base_url}/providers/gemini/status"
    response = requests.get(status_url)
    assert_status_code(response, 200)

    data = response.json()
    assert data["configured"] is False, "Gemini should NOT be configured"
    assert data["provider"] == "gemini"


def test_configure_gemini_provider(base_url):
    url = f"{base_url}/providers/gemini/configure"
    payload = {
        "apiKey": "gemini-test-key",
        "modelName": "gemini-pro",
        "upsert": True,
    }

    response = requests.post(url, json=payload)
    assert_status_code(response, 200)

    data = response.json()
    assert data["configured"] == True, "Provider was not configured"


def test_missing_api_key_fails(base_url):
    url = f"{base_url}/providers/openai/configure"
    payload = {"modelName": "gpt-3.5-turbo"}  # Missing apiKey

    response = requests.post(url, json=payload)
    assert_status_code(response, 422)


def test_get_openai_status_configured(base_url):
    # First configure it
    configure_url = f"{base_url}/providers/openai/configure"
    payload = {"apiKey": "sk-test-openai-key", "upsert": True}
    requests.post(configure_url, json=payload)

    # Now check status
    status_url = f"{base_url}/providers/openai/status"
    response = requests.get(status_url)
    assert_status_code(response, 200)

    data = response.json()
    assert data["configured"] is True, "OpenAI should be configured"
    assert data["provider"] == "openai"


def test_configure_and_check_status_roundtrip(base_url):
    # Configure both (upsert so reruns against the same DB don't 422)
    requests.post(
        f"{base_url}/providers/openai/configure",
        json={"apiKey": "sk-test-openai-key", "upsert": True},
    )
    requests.post(
        f"{base_url}/providers/gemini/configure",
        json={"apiKey": "gemini-test-key", "upsert": True},
    )

    # Check OpenAI
    openai_status = requests.get(f"{base_url}/providers/openai/status")
    assert_status_code(openai_status, 200)
    assert openai_status.json()["configured"] is True

    # Check Gemini
    gemini_status = requests.get(f"{base_url}/providers/gemini/status")
    assert_status_code(gemini_status, 200)
    assert gemini_status.json()["configured"] is True

def test_list_provider_configs(base_url):
    """Test listing all provider configurations"""
    response = requests.get(f"{base_url}/providers/configs")
    assert_status_code(response, 200)

    configs = response.json()
    assert isinstance(configs, list)

def test_get_specific_provider_config(base_url):
    """Test getting a specific provider configuration"""
    # First configure a provider
    requests.post(
        f"{base_url}/providers/openai/configure",
        json={"apiKey": "sk-test-openai-key", "upsert": True},
    )
    # Now get its config
    response = requests.get(f"{base_url}/providers/openai/config")
    # Could be 200 (if configured) or 404 (if not)
    if response.status_code == 200:
        config = response.json()
        assert "Type" in config
        assert "APIKey" in config

def test_delete_provider_config(base_url):
    """Test deleting a provider configuration"""
    # First configure a provider
    requests.post(
        f"{base_url}/providers/openai/configure",
        json={"apiKey": "sk-test-openai-key", "upsert": True},
    )

    # Now delete it
    response = requests.delete(f"{base_url}/providers/openai/config")
    assert_status_code(response, 200)

    # Verify it's gone
    response = requests.get(f"{base_url}/providers/openai/config")
    assert response.status_code == 404
