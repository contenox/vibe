"""
API tests for HITL policy file management via the VFS file API.

Coverage:
- Upload / download / delete roundtrip for a valid policy JSON
- Minimal policy (empty rules array) accepted and returned intact
- Policy with when-conditions and timeout fields roundtrips cleanly
- default_action field survives upload/download
- Listing files includes the uploaded file
- Uploading syntactically invalid JSON still stores the bytes (validation
  is load-time, not write-time, so storage must succeed)
- Overwrite via PUT /files/{id} replaces content
"""

import io
import json
import uuid

import requests
from helpers import assert_status_code

# ─── helpers ──────────────────────────────────────────────────────────────────


def _upload_policy(base_url: str, policy: dict, filename: str | None = None) -> dict:
    """Upload policy JSON via POST /files, return parsed FileResponse JSON."""
    name = filename or f"hitl-policy-{uuid.uuid4().hex[:8]}.json"
    data = json.dumps(policy).encode()
    resp = requests.post(
        f"{base_url}/files",
        files={"file": (name, io.BytesIO(data), "application/json")},
    )
    assert_status_code(resp, 201)
    return resp.json()


def _download(base_url: str, file_id: str) -> bytes:
    resp = requests.get(f"{base_url}/files/{file_id}/download")
    assert_status_code(resp, 200)
    return resp.content


def _delete(base_url: str, file_id: str):
    resp = requests.delete(f"{base_url}/files/{file_id}")
    assert resp.status_code in (200, 404)


# ─── tests ────────────────────────────────────────────────────────────────────


def test_upload_and_download_minimal_policy(base_url):
    """Upload a policy with an empty rules list and download the same bytes back."""
    policy = {"rules": []}
    meta = _upload_policy(base_url, policy)
    file_id = meta["id"]
    try:
        raw = _download(base_url, file_id)
        assert json.loads(raw) == policy
    finally:
        _delete(base_url, file_id)


def test_upload_and_download_policy_with_rules(base_url):
    """A policy with allow/approve/deny rules roundtrips without data loss."""
    policy = {
        "default_action": "deny",
        "rules": [
            {"hook": "local_fs", "tool": "write_file", "action": "approve"},
            {"hook": "local_fs", "tool": "read_file", "action": "allow"},
            {"hook": "local_shell", "tool": "local_shell", "action": "deny"},
        ],
    }
    meta = _upload_policy(base_url, policy)
    file_id = meta["id"]
    try:
        got = json.loads(_download(base_url, file_id))
        assert got["default_action"] == "deny"
        assert len(got["rules"]) == 3
        assert got["rules"][0]["action"] == "approve"
    finally:
        _delete(base_url, file_id)


def test_upload_policy_with_when_conditions(base_url):
    """when-conditions and timeout fields are stored and retrieved verbatim."""
    policy = {
        "rules": [
            {
                "hook": "local_fs",
                "tool": "write_file",
                "when": [
                    {"key": "path", "op": "glob", "value": "src/**"},
                ],
                "action": "approve",
                "timeout_s": 120,
                "on_timeout": "deny",
            }
        ]
    }
    meta = _upload_policy(base_url, policy)
    file_id = meta["id"]
    try:
        got = json.loads(_download(base_url, file_id))
        rule = got["rules"][0]
        assert rule["when"][0]["op"] == "glob"
        assert rule["when"][0]["value"] == "src/**"
        assert rule["timeout_s"] == 120
        assert rule["on_timeout"] == "deny"
    finally:
        _delete(base_url, file_id)


def test_file_metadata_fields_present(base_url):
    """Uploaded file metadata contains expected fields with sensible values."""
    policy = {"rules": []}
    meta = _upload_policy(base_url, policy)
    file_id = meta["id"]
    try:
        assert meta.get("id")
        assert meta.get("name")
        assert meta.get("size", 0) > 0
        assert meta.get("createdAt")
    finally:
        _delete(base_url, file_id)


def test_list_files_includes_uploaded_file(base_url):
    """GET /files returns a list that includes the file we just uploaded."""
    policy = {"rules": []}
    meta = _upload_policy(base_url, policy)
    file_id = meta["id"]
    try:
        resp = requests.get(f"{base_url}/files")
        assert_status_code(resp, 200)
        ids = [f["id"] for f in resp.json()]
        assert file_id in ids
    finally:
        _delete(base_url, file_id)


def test_overwrite_policy_via_put(base_url):
    """PUT /files/{id} replaces the content; subsequent download reflects new content."""
    policy_v1 = {"rules": []}
    meta = _upload_policy(base_url, policy_v1)
    file_id = meta["id"]
    try:
        policy_v2 = {
            "default_action": "allow",
            "rules": [{"hook": "local_shell", "tool": "local_shell", "action": "deny"}],
        }
        data = json.dumps(policy_v2).encode()
        resp = requests.put(
            f"{base_url}/files/{file_id}",
            files={"file": ("hitl-policy.json", io.BytesIO(data), "application/json")},
        )
        assert_status_code(resp, 200)

        got = json.loads(_download(base_url, file_id))
        assert got["default_action"] == "allow"
        assert len(got["rules"]) == 1
        assert got["rules"][0]["action"] == "deny"
    finally:
        _delete(base_url, file_id)


def test_delete_policy_file(base_url):
    """DELETE /files/{id} removes the file; subsequent download returns 404."""
    policy = {"rules": []}
    meta = _upload_policy(base_url, policy)
    file_id = meta["id"]

    del_resp = requests.delete(f"{base_url}/files/{file_id}")
    assert_status_code(del_resp, 200)

    get_resp = requests.get(f"{base_url}/files/{file_id}/download")
    assert_status_code(get_resp, 404)


def test_get_metadata_returns_404_for_nonexistent(base_url):
    """GET /files/{id} for an unknown ID returns 404."""
    resp = requests.get(f"{base_url}/files/nonexistent-{uuid.uuid4().hex}")
    assert_status_code(resp, 404)


def test_upload_syntactically_invalid_json_still_stores(base_url):
    """Storage layer accepts any bytes; JSON validity is checked at policy load time."""
    invalid_bytes = b"not json {{{ broken"
    name = f"hitl-invalid-{uuid.uuid4().hex[:8]}.json"
    resp = requests.post(
        f"{base_url}/files",
        files={"file": (name, io.BytesIO(invalid_bytes), "application/json")},
    )
    assert_status_code(resp, 201)
    file_id = resp.json()["id"]
    try:
        raw = _download(base_url, file_id)
        assert raw == invalid_bytes
    finally:
        _delete(base_url, file_id)


def test_upload_policy_with_eq_condition(base_url):
    """eq op condition roundtrips without modification."""
    policy = {
        "rules": [
            {
                "hook": "local_fs",
                "tool": "delete_file",
                "when": [{"key": "path", "op": "eq", "value": "/etc/passwd"}],
                "action": "deny",
            }
        ]
    }
    meta = _upload_policy(base_url, policy)
    file_id = meta["id"]
    try:
        got = json.loads(_download(base_url, file_id))
        assert got["rules"][0]["when"][0]["op"] == "eq"
        assert got["rules"][0]["action"] == "deny"
    finally:
        _delete(base_url, file_id)


# ─── /hitl-policies CRUD API ──────────────────────────────────────────────────


def test_list_hitl_policies_includes_embedded_presets(base_url):
    """GET /hitl-policies/list returns the embedded preset files shipped with the binary."""
    resp = requests.get(f"{base_url}/hitl-policies/list")
    assert_status_code(resp, 200)
    names = resp.json()
    assert isinstance(names, list)
    assert "hitl-policy-default.json" in names
    assert "hitl-policy-strict.json" in names
    assert "hitl-policy-dev.json" in names


def test_create_get_delete_policy(base_url):
    """POST /hitl-policies creates a policy; GET retrieves it; DELETE removes it."""
    name = f"hitl-policy-test-{uuid.uuid4().hex[:8]}.json"
    policy = {
        "rules": [{"hook": "local_fs", "tool": "write_file", "action": "approve"}]
    }

    resp = requests.post(f"{base_url}/hitl-policies?name={name}", json=policy)
    assert_status_code(resp, 201)
    created = resp.json()
    assert created["rules"][0]["action"] == "approve"

    resp = requests.get(f"{base_url}/hitl-policies?name={name}")
    assert_status_code(resp, 200)
    got = resp.json()
    assert got["rules"][0]["tool"] == "write_file"

    resp = requests.delete(f"{base_url}/hitl-policies?name={name}")
    assert_status_code(resp, 200)

    resp = requests.get(f"{base_url}/hitl-policies?name={name}")
    assert resp.status_code == 404


def test_update_policy(base_url):
    """PUT /hitl-policies updates an existing policy."""
    name = f"hitl-policy-upd-{uuid.uuid4().hex[:8]}.json"
    original = {"rules": [{"hook": "*", "tool": "*", "action": "allow"}]}
    updated = {"default_action": "deny", "rules": []}

    requests.post(f"{base_url}/hitl-policies?name={name}", json=original)
    try:
        resp = requests.put(f"{base_url}/hitl-policies?name={name}", json=updated)
        assert_status_code(resp, 200)

        resp = requests.get(f"{base_url}/hitl-policies?name={name}")
        got = resp.json()
        assert got.get("default_action") == "deny"
        assert got["rules"] == []
    finally:
        requests.delete(f"{base_url}/hitl-policies?name={name}")


def test_set_active_policy_via_cli_config(base_url):
    """PUT /cli-config with hitl-policy-name persists the active policy selection."""
    name = "hitl-policy-default.json"
    resp = requests.put(f"{base_url}/cli-config", json={"hitl-policy-name": name})
    assert_status_code(resp, 200)
    body = resp.json()
    assert body.get("hitlPolicyName") == name

    resp = requests.get(f"{base_url}/setup-status")
    assert_status_code(resp, 200)
    status = resp.json()
    assert status.get("hitlPolicyName") == name
