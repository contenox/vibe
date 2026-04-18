"""Tests for the model-registry CRUD routes and POST /model-registry/download."""

import os
import uuid

import pytest
import requests

from helpers import assert_status_code, generate_unique_name

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _url(base_url: str, *parts: str) -> str:
    return "/".join([base_url.rstrip("/"), "model-registry", *parts])


def _create_entry(base_url: str, *, name: str = None, source_url: str = None) -> dict:
    name = name or generate_unique_name("test-model")
    source_url = source_url or f"https://example.com/models/{name}.gguf"
    payload = {"name": name, "sourceUrl": source_url, "sizeBytes": 0}
    r = requests.post(_url(base_url), json=payload, timeout=30)
    assert_status_code(r, 201)
    return r.json()


def _delete_entry(base_url: str, entry_id: str) -> None:
    requests.delete(_url(base_url, entry_id), timeout=30)


# ---------------------------------------------------------------------------
# List
# ---------------------------------------------------------------------------

def test_list_model_registry_returns_200(base_url):
    r = requests.get(_url(base_url), timeout=30)
    assert_status_code(r, 200)
    entries = r.json()
    assert isinstance(entries, list)


def test_list_model_registry_includes_curated(base_url):
    r = requests.get(_url(base_url), timeout=30)
    assert_status_code(r, 200)
    entries = r.json()
    curated = [e for e in entries if e.get("curated")]
    assert len(curated) > 0, "Expected at least one curated entry"


def test_list_model_registry_curated_have_source_url(base_url):
    r = requests.get(_url(base_url), timeout=30)
    assert_status_code(r, 200)
    for entry in r.json():
        if entry.get("curated"):
            assert entry.get("sourceUrl"), f"Curated entry {entry.get('name')!r} missing sourceUrl"


# ---------------------------------------------------------------------------
# Create
# ---------------------------------------------------------------------------

def test_create_model_registry_entry(base_url):
    name = generate_unique_name("reg-create")
    payload = {
        "name": name,
        "sourceUrl": f"https://example.com/{name}.gguf",
        "sizeBytes": 1024,
    }
    r = requests.post(_url(base_url), json=payload, timeout=30)
    assert_status_code(r, 201)
    body = r.json()
    assert body["name"] == name
    assert body["sourceUrl"] == payload["sourceUrl"]
    assert "id" in body
    _delete_entry(base_url, body["id"])


def test_create_model_registry_entry_fields_persisted(base_url):
    name = generate_unique_name("reg-fields")
    payload = {
        "name": name,
        "sourceUrl": "https://hf.co/org/model.gguf",
        "sizeBytes": 500_000_000,
    }
    r = requests.post(_url(base_url), json=payload, timeout=30)
    assert_status_code(r, 201)
    body = r.json()
    assert body["sizeBytes"] == 500_000_000
    _delete_entry(base_url, body["id"])


# ---------------------------------------------------------------------------
# Get by ID
# ---------------------------------------------------------------------------

def test_get_model_registry_entry(base_url):
    entry = _create_entry(base_url)
    r = requests.get(_url(base_url, entry["id"]), timeout=30)
    assert_status_code(r, 200)
    body = r.json()
    assert body["id"] == entry["id"]
    assert body["name"] == entry["name"]
    _delete_entry(base_url, entry["id"])


def test_get_model_registry_entry_not_found(base_url):
    r = requests.get(_url(base_url, str(uuid.uuid4())), timeout=30)
    assert r.status_code == 404


# ---------------------------------------------------------------------------
# Update
# ---------------------------------------------------------------------------

def test_update_model_registry_entry(base_url):
    entry = _create_entry(base_url)
    updated_url = "https://example.com/updated-model.gguf"
    payload = {**entry, "sourceUrl": updated_url}
    r = requests.put(_url(base_url, entry["id"]), json=payload, timeout=30)
    assert_status_code(r, 200)
    body = r.json()
    assert body["sourceUrl"] == updated_url
    _delete_entry(base_url, entry["id"])


def test_update_model_registry_entry_not_found(base_url):
    missing_id = str(uuid.uuid4())
    payload = {"id": missing_id, "name": "ghost", "sourceUrl": "https://x.com/m.gguf", "sizeBytes": 0}
    r = requests.put(_url(base_url, missing_id), json=payload, timeout=30)
    assert r.status_code == 404


# ---------------------------------------------------------------------------
# Delete
# ---------------------------------------------------------------------------

def test_delete_model_registry_entry(base_url):
    entry = _create_entry(base_url)
    r = requests.delete(_url(base_url, entry["id"]), timeout=30)
    assert_status_code(r, 200)


def test_delete_model_registry_entry_idempotent_404(base_url):
    entry = _create_entry(base_url)
    _delete_entry(base_url, entry["id"])
    r = requests.delete(_url(base_url, entry["id"]), timeout=30)
    assert r.status_code == 404


def test_delete_model_registry_entry_not_found(base_url):
    r = requests.delete(_url(base_url, str(uuid.uuid4())), timeout=30)
    assert r.status_code == 404


# ---------------------------------------------------------------------------
# POST /model-registry/download — validation (always run)
# ---------------------------------------------------------------------------

def test_download_empty_name_returns_error(base_url):
    r = requests.post(_url(base_url, "download"), json={"name": ""}, timeout=30)
    assert r.status_code in (400, 422), f"Expected 400 or 422, got {r.status_code}"


def test_download_missing_name_field_returns_error(base_url):
    r = requests.post(_url(base_url, "download"), json={}, timeout=30)
    assert r.status_code in (400, 422), f"Expected 400 or 422, got {r.status_code}"


def test_download_unknown_model_returns_error(base_url):
    r = requests.post(
        _url(base_url, "download"),
        json={"name": f"definitely-not-a-real-model-{uuid.uuid4().hex[:8]}"},
        timeout=30,
    )
    assert r.status_code in (400, 404, 422), f"Expected 4xx, got {r.status_code}"


# ---------------------------------------------------------------------------
# POST /model-registry/download — actual download (opt-in, slow)
# Skipped unless APITEST_RUN_DOWNLOAD=1 is set.
# Uses "tiny" (FastThink 0.5B Q2_K, ~191 MB) — smallest curated model.
# ---------------------------------------------------------------------------

_RUN_DOWNLOAD = os.environ.get("APITEST_RUN_DOWNLOAD", "").strip() == "1"
_DOWNLOAD_MODEL = os.environ.get("APITEST_DOWNLOAD_MODEL", "tiny")


@pytest.mark.skipif(not _RUN_DOWNLOAD, reason="Set APITEST_RUN_DOWNLOAD=1 to run download tests")
def test_download_curated_model(base_url):
    """Downloads a real GGUF model — slow, requires disk space and internet."""
    r = requests.post(
        _url(base_url, "download"),
        json={"name": _DOWNLOAD_MODEL},
        timeout=600,  # large files; no client-side timeout on server side
    )
    assert_status_code(r, 200)
    assert r.json() in ("downloaded", "already downloaded")


@pytest.mark.skipif(not _RUN_DOWNLOAD, reason="Set APITEST_RUN_DOWNLOAD=1 to run download tests")
def test_download_curated_model_idempotent(base_url):
    """Second download of the same model returns 'already downloaded'."""
    for _ in range(2):
        r = requests.post(
            _url(base_url, "download"),
            json={"name": _DOWNLOAD_MODEL},
            timeout=600,
        )
        assert_status_code(r, 200)
        assert r.json() in ("downloaded", "already downloaded")


@pytest.mark.skipif(not _RUN_DOWNLOAD, reason="Set APITEST_RUN_DOWNLOAD=1 to run download tests")
def test_download_auto_creates_local_backend(base_url):
    """After download the local backend must exist in the backends list."""
    r = requests.post(
        _url(base_url, "download"),
        json={"name": _DOWNLOAD_MODEL},
        timeout=600,
    )
    assert_status_code(r, 200)

    backends = requests.get(f"{base_url}/backends", timeout=30).json()
    local_backends = [b for b in backends if b.get("type") == "local"]
    assert len(local_backends) >= 1, "Expected at least one local backend after download"


@pytest.mark.skipif(not _RUN_DOWNLOAD, reason="Set APITEST_RUN_DOWNLOAD=1 to run download tests")
def test_download_sets_default_provider(base_url):
    """After download default-provider must be 'local' (was unset or ollama)."""
    r = requests.post(
        _url(base_url, "download"),
        json={"name": _DOWNLOAD_MODEL},
        timeout=600,
    )
    assert_status_code(r, 200)

    status = requests.get(f"{base_url}/setup-status", timeout=30).json()
    assert status.get("defaultProvider") == "local"
