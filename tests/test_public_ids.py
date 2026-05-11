from __future__ import annotations

import json
from pathlib import Path

import pytest
from glimmung import public_ids


PUBLIC_ID_CASES = json.loads(
    (Path(__file__).resolve().parents[1] / "testdata" / "public_id_cases.json").read_text()
)


@pytest.mark.parametrize("case", PUBLIC_ID_CASES, ids=lambda case: case["name"])
def test_public_id_golden_cases(case):
    match case["function"]:
        case "issue_ref":
            got = public_ids.issue_ref(case["project"], case["number"])
        case "run_ref":
            got = public_ids.run_ref(case["project"], case["issue_number"], case["run_display"])
        case "report_ref":
            got = public_ids.report_ref(case["repo"], case["number"])
        case "lease_ref":
            got = public_ids.lease_ref(
                case["project"],
                slot_name=case["slot_name"],
                lease_number=case["lease_number"],
            )
        case _:
            raise AssertionError(f"unknown public ID function {case['function']!r}")
    assert got == case["want"]
