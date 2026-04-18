import uuid
import requests
from helpers import assert_status_code


def test_respond_unknown_id_returns_not_found(base_url):
    """POST /approvals/{id} with an ID that has no pending approval returns 404."""
    unknown_id = str(uuid.uuid4())
    resp = requests.post(
        f"{base_url}/approvals/{unknown_id}",
        json={"approved": True},
    )
    assert_status_code(resp, 404)


def test_respond_without_body_returns_error(base_url):
    """POST /approvals/{id} with no JSON body returns a 4xx error."""
    unknown_id = str(uuid.uuid4())
    resp = requests.post(
        f"{base_url}/approvals/{unknown_id}",
        data="not-json",
        headers={"Content-Type": "text/plain"},
    )
    assert resp.status_code >= 400


def test_respond_deny_unknown_id_returns_not_found(base_url):
    """POST /approvals/{id} with approved=false and unknown ID also returns 404."""
    unknown_id = str(uuid.uuid4())
    resp = requests.post(
        f"{base_url}/approvals/{unknown_id}",
        json={"approved": False},
    )
    assert_status_code(resp, 404)
