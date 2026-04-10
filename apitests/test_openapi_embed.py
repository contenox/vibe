"""Embedded OpenAPI + RapiDoc served on the root mux (same as FastAPI-style /openapi.json + /docs)."""

import requests

from helpers import assert_status_code


def test_openapi_json_embedded(beam_origin):
    """GET /openapi.json returns embedded spec (no /api prefix, no JWT)."""
    r = requests.get(f"{beam_origin}/openapi.json", timeout=30)
    assert_status_code(r, 200)
    assert r.headers.get("Content-Type", "").startswith("application/json")
    doc = r.json()
    assert doc.get("openapi") in ("3.0.0", "3.0.3", "3.1.0")
    assert "paths" in doc
    assert len(doc.get("paths") or {}) > 0


def test_openapi_docs_page(beam_origin):
    """GET /docs serves RapiDoc HTML that loads /openapi.json."""
    r = requests.get(f"{beam_origin}/docs", timeout=30)
    assert_status_code(r, 200)
    assert "text/html" in (r.headers.get("Content-Type") or "")
    body = r.text
    assert "rapi-doc" in body.lower() or "rapidoc" in body.lower()


def test_beam_spa_root_still_serves(beam_origin):
    """GET / still serves the Beam SPA (embedded UI), not broken by /openapi routes."""
    r = requests.get(f"{beam_origin}/", timeout=30)
    assert_status_code(r, 200)
    assert "text/html" in (r.headers.get("Content-Type") or "")
    # Embedded Vite build serves index with root mount
    assert len(r.text) > 100
