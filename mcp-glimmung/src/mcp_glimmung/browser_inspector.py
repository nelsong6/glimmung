"""Browser inspection wrapper used by the MCP tool and CLI.

The actual browser work lives in the sibling Node/Playwright helper so the
Python package remains installable on Alpine-based session pods where Python
Playwright wheels are unavailable.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
from pathlib import Path
from typing import Any


DEFAULT_ARTIFACT_DIR = Path("/tmp/glimmung-browser-inspections")
SCRIPT_PATH = Path(__file__).with_name("browser_inspector.mjs")


def _slug(value: str) -> str:
    slug = re.sub(r"[^A-Za-z0-9._-]+", "-", value).strip("-")
    return slug[:80] or "inspection"


def _truncate(value: str, limit: int) -> str:
    if len(value) <= limit:
        return value
    return value[: max(limit - 3, 0)].rstrip() + "..."


def inspect_url(
    *,
    url: str,
    viewport: dict[str, int] | None = None,
    wait_ms: int = 2000,
    timeout_ms: int = 30000,
    screenshot: bool = False,
    full_page: bool = True,
    capture_accessibility: bool = False,
    capture_console: bool = True,
    capture_network: bool = True,
    max_elements: int = 80,
    body_text_limit: int = 4000,
    artifact_dir: str | Path = DEFAULT_ARTIFACT_DIR,
) -> dict[str, Any]:
    """Inspect a URL with Chromium and return JSON-serializable browser state."""
    width = int((viewport or {}).get("width", 1440))
    height = int((viewport or {}).get("height", 900))
    payload = {
        "url": url,
        "viewport": {"width": width, "height": height},
        "waitMs": wait_ms,
        "timeoutMs": timeout_ms,
        "screenshot": screenshot,
        "fullPage": full_page,
        "captureAccessibility": capture_accessibility,
        "captureConsole": capture_console,
        "captureNetwork": capture_network,
        "maxElements": max_elements,
        "bodyTextLimit": body_text_limit,
        "artifactDir": str(artifact_dir),
    }
    env = os.environ.copy()
    if "NODE_PATH" not in env:
        node_modules = Path.cwd() / "node_modules"
        if node_modules.exists():
            env["NODE_PATH"] = str(node_modules)
    proc = subprocess.run(
        ["node", str(SCRIPT_PATH)],
        input=json.dumps(payload),
        text=True,
        capture_output=True,
        env=env,
        check=False,
    )
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or "").strip()
        raise RuntimeError(f"browser inspection failed: {detail}")
    return json.loads(proc.stdout)


def main() -> None:
    parser = argparse.ArgumentParser(description="Inspect a URL with Chromium and emit JSON.")
    parser.add_argument("url")
    parser.add_argument("--width", type=int, default=1440)
    parser.add_argument("--height", type=int, default=900)
    parser.add_argument("--wait-ms", type=int, default=2000)
    parser.add_argument("--timeout-ms", type=int, default=30000)
    parser.add_argument("--screenshot", action="store_true")
    parser.add_argument("--no-full-page", action="store_true")
    parser.add_argument("--accessibility", action="store_true")
    parser.add_argument("--max-elements", type=int, default=80)
    parser.add_argument("--body-text-limit", type=int, default=4000)
    parser.add_argument("--artifact-dir", default=str(DEFAULT_ARTIFACT_DIR))
    args = parser.parse_args()

    result = inspect_url(
        url=args.url,
        viewport={"width": args.width, "height": args.height},
        wait_ms=args.wait_ms,
        timeout_ms=args.timeout_ms,
        screenshot=args.screenshot,
        full_page=not args.no_full_page,
        capture_accessibility=args.accessibility,
        max_elements=args.max_elements,
        body_text_limit=args.body_text_limit,
        artifact_dir=args.artifact_dir,
    )
    json.dump(result, sys.stdout, indent=2)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
