"""Verification artifact extraction.

Network paths (`fetch_verification`) are exercised in integration; here
we cover the contract-shape boundary — what counts as well-formed,
what's malformed-but-recoverable-as-None, and what the producer must
emit.
"""

import io
import json
import zipfile

from glimmung.models import VerificationResult, VerificationStatus
from glimmung.verification import _extract_json


def _zip_bytes(filename: str, content: bytes) -> bytes:
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr(filename, content)
    return buf.getvalue()


def test_extract_well_formed_zip_returns_payload():
    payload = {"schema_version": 1, "status": "pass", "cost_usd": 1.5}
    raw = _zip_bytes("verification.json", json.dumps(payload).encode())
    assert _extract_json(raw) == payload


def test_extract_returns_none_for_corrupt_zip():
    assert _extract_json(b"not-a-zip") is None


def test_extract_returns_none_when_filename_missing():
    raw = _zip_bytes("not-the-right-name.json", b'{"status": "pass"}')
    assert _extract_json(raw) is None


def test_extract_returns_none_for_bad_json():
    raw = _zip_bytes("verification.json", b"this is not json")
    assert _extract_json(raw) is None


def test_pass_verdict_round_trips():
    payload = {
        "schema_version": 1,
        "status": "pass",
        "reasons": [],
        "evidence_refs": ["screenshots/01.png"],
        "cost_usd": 4.20,
        "prompt_version": "v17",
    }
    result = VerificationResult.model_validate(payload)
    assert result.status == VerificationStatus.PASS
    assert result.evidence_refs == ["screenshots/01.png"]
    assert result.cost_usd == 4.20


def test_fail_verdict_with_reasons():
    payload = {
        "status": "fail",
        "reasons": ["selector #login missing", "expected 200 got 500"],
        "cost_usd": 8.0,
    }
    result = VerificationResult.model_validate(payload)
    assert result.status == VerificationStatus.FAIL
    assert len(result.reasons) == 2


def test_error_verdict_distinct_from_fail():
    payload = {"status": "error", "reasons": ["verifier crashed: NPE"], "cost_usd": 0.5}
    result = VerificationResult.model_validate(payload)
    assert result.status == VerificationStatus.ERROR


def test_unknown_status_is_schema_error():
    payload = {"status": "maybe", "cost_usd": 1.0}
    try:
        VerificationResult.model_validate(payload)
    except Exception:
        return
    raise AssertionError("expected schema validation to reject unknown status")


def test_minimal_payload_uses_defaults():
    """Producers can emit just the required fields; rest defaults."""
    result = VerificationResult.model_validate({"status": "pass"})
    assert result.cost_usd == 0.0
    assert result.reasons == []
    assert result.evidence_refs == []
    assert result.prompt_version is None
