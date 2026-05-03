import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from mcp_glimmung.tools import register_tools


class FakeMCP:
    def __init__(self) -> None:
        self.tools: dict[str, Any] = {}

    def tool(self) -> Any:
        def decorate(fn: Any) -> Any:
            self.tools[fn.__name__] = fn
            return fn

        return decorate


class StubClient:
    def __init__(self) -> None:
        self.calls: list[tuple[str, str, dict[str, Any] | None, dict[str, Any] | None]] = []

    def get(self, path: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
        self.calls.append(("GET", path, params, None))
        return {"path": path}

    def patch(self, path: str, json: dict[str, Any]) -> dict[str, Any]:
        self.calls.append(("PATCH", path, None, json))
        return {"path": path, "json": json}

    def post(
        self,
        path: str,
        params: dict[str, Any] | None = None,
        json: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        self.calls.append(("POST", path, params, json))
        return {"path": path, "params": params, "json": json}


def _registered_tools() -> tuple[dict[str, Any], StubClient]:
    mcp = FakeMCP()
    client = StubClient()
    register_tools(mcp, client)  # type: ignore[arg-type]
    return mcp.tools, client


def test_create_issue_posts_native_issue_payload() -> None:
    tools, client = _registered_tools()

    result = tools["create_issue"]("glimmung", "Cut issue tracking over")

    assert result == {
        "path": "/v1/issues",
        "params": None,
        "json": {
            "project": "glimmung",
            "title": "Cut issue tracking over",
            "body": "",
            "labels": [],
        },
    }
    assert client.calls[-1] == ("POST", "/v1/issues", None, result["json"])


def test_create_pr_posts_registration_payload() -> None:
    tools, client = _registered_tools()

    result = tools["create_pr"](
        project="glimmung",
        repo="nelsong6/glimmung",
        number=123,
        title="MCP parity",
        branch="codex/mcp-parity",
        linked_issue_id="issue-1",
        linked_run_id="run-1",
    )

    assert result["path"] == "/v1/prs"
    assert result["json"] == {
        "project": "glimmung",
        "repo": "nelsong6/glimmung",
        "number": 123,
        "title": "MCP parity",
        "branch": "codex/mcp-parity",
        "body": "",
        "base_ref": "main",
        "head_sha": "",
        "html_url": "",
        "linked_issue_id": "issue-1",
        "linked_run_id": "run-1",
    }
    assert client.calls[-1] == ("POST", "/v1/prs", None, result["json"])


def test_enqueue_signal_posts_drain_loop_payload() -> None:
    tools, client = _registered_tools()

    result = tools["enqueue_signal"](
        target_type="pr",
        target_repo="nelsong6/glimmung",
        target_id="123",
        payload={"kind": "reject", "feedback": "tighten tests"},
    )

    assert result["path"] == "/v1/signals"
    assert result["json"] == {
        "target_type": "pr",
        "target_repo": "nelsong6/glimmung",
        "target_id": "123",
        "source": "glimmung_ui",
        "payload": {"kind": "reject", "feedback": "tighten tests"},
    }
    assert client.calls[-1] == ("POST", "/v1/signals", None, result["json"])


def test_register_project_and_host_post_admin_payloads() -> None:
    tools, client = _registered_tools()

    project = tools["register_project"](
        "glimmung",
        "nelsong6/glimmung",
        metadata={"tier": "control-plane"},
    )
    host = tools["register_host"](
        "runner-1",
        capabilities={"gpu": False},
        drained=True,
    )

    assert project["json"] == {
        "name": "glimmung",
        "github_repo": "nelsong6/glimmung",
        "metadata": {"tier": "control-plane"},
    }
    assert host["json"] == {
        "name": "runner-1",
        "capabilities": {"gpu": False},
        "drained": True,
    }
    assert client.calls[-2:] == [
        ("POST", "/v1/projects", None, project["json"]),
        ("POST", "/v1/hosts", None, host["json"]),
    ]
