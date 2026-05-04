import shutil
from pathlib import Path
from typing import Any

import pytest

from mcp_glimmung.browser_inspector import _slug, _truncate, inspect_url


def test_slug_and_truncate_keep_artifact_names_stable() -> None:
    assert _slug("https://example.test/a b?x=1") == "https-example.test-a-b-x-1"
    assert _truncate("abcdef", 4) == "a..."
    assert _truncate("abc", 4) == "abc"


def test_inspect_url_smoke_with_data_url(tmp_path: Path) -> None:
    if shutil.which("node") is None:
        pytest.skip("node is not installed")

    try:
        result: dict[str, Any] = inspect_url(
            url=(
                "data:text/html,"
                "<title>Inspector</title>"
                "<main><h1>Hello</h1><button>Run</button><canvas width='4' height='4'></canvas></main>"
            ),
            wait_ms=0,
            screenshot=True,
            artifact_dir=tmp_path,
        )
    except Exception as exc:
        if "Cannot find package 'playwright'" in str(exc) or "Executable doesn't exist" in str(exc):
            pytest.skip("Playwright browser is not installed")
        raise

    assert result["title"] == "Inspector"
    assert result["final_url"].startswith("data:text/html")
    assert any(el["role"] == "button" for el in result["elements"])
    assert result["canvas"][0]["width"] == 4
    assert result["screenshot_path"]
    assert Path(result["screenshot_path"]).exists()
