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


def test_list_issues_passes_filters_and_defaults_limit() -> None:
    tools, client = _registered_tools()

    tools["list_issues"](project="glimmung", repo="nelsong6/glimmung", limit=10)

    assert client.calls[-1] == (
        "GET",
        "/v1/issues",
        {
            "project": "glimmung",
            "repo": "nelsong6/glimmung",
            "limit": 10,
        },
        None,
    )


def test_list_issues_plain_call_caps_results() -> None:
    tools, client = _registered_tools()

    tools["list_issues"]()

    assert client.calls[-1] == ("GET", "/v1/issues", {"limit": 50}, None)


def test_list_reports_passes_filters_and_defaults_limit() -> None:
    tools, client = _registered_tools()

    tools["list_reports"](
        project="glimmung",
        repo="nelsong6/glimmung",
        state="needs_review",
        limit=10,
    )

    assert client.calls[-1] == (
        "GET",
        "/v1/reports",
        {
            "project": "glimmung",
            "repo": "nelsong6/glimmung",
            "state": "needs_review",
            "limit": 10,
        },
        None,
    )


def test_list_reports_plain_call_caps_results() -> None:
    tools, client = _registered_tools()

    tools["list_reports"]()

    assert client.calls[-1] == ("GET", "/v1/reports", {"limit": 50}, None)


def test_create_pr_posts_registration_payload() -> None:
    tools, client = _registered_tools()

    result = tools["create_report"](
        project="glimmung",
        repo="nelsong6/glimmung",
        number=123,
        title="MCP parity",
        branch="codex/mcp-parity",
        linked_issue_id="issue-1",
        linked_run_id="run-1",
    )

    assert result["path"] == "/v1/reports"
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
    assert client.calls[-1] == ("POST", "/v1/reports", None, result["json"])


def test_report_version_tools_call_http_surface() -> None:
    tools, client = _registered_tools()

    listed = tools["list_report_versions"]("glimmung", "report-1")
    one = tools["get_report_version"]("glimmung", "report-1", 2)
    created = tools["create_report_version"](
        project="glimmung",
        report_id="report-1",
        title="snapshot",
        body="body",
        state="needs_review",
        linked_run_id="run-1",
        github_repo="nelsong6/glimmung",
        github_pr_number=123,
        github_html_url="https://github.com/nelsong6/glimmung/pull/123",
        version=2,
    )

    assert listed["path"] == "/v1/reports/by-id/glimmung/report-1/versions"
    assert one["path"] == "/v1/reports/by-id/glimmung/report-1/versions/2"
    assert created["path"] == "/v1/reports/by-id/glimmung/report-1/versions"
    assert created["json"] == {
        "title": "snapshot",
        "body": "body",
        "state": "needs_review",
        "linked_run_id": "run-1",
        "github_repo": "nelsong6/glimmung",
        "github_pr_number": 123,
        "github_html_url": "https://github.com/nelsong6/glimmung/pull/123",
        "version": 2,
    }
    assert client.calls[-3:] == [
        ("GET", "/v1/reports/by-id/glimmung/report-1/versions", None, None),
        ("GET", "/v1/reports/by-id/glimmung/report-1/versions/2", None, None),
        ("POST", "/v1/reports/by-id/glimmung/report-1/versions", None, created["json"]),
    ]


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


def test_playbook_tools_call_http_surface() -> None:
    tools, client = _registered_tools()

    created = tools["create_playbook"](
        project="glimmung",
        title="Coordinated rollout",
        description="storage slice",
        entries=[{
            "id": "one",
            "issue": {
                "title": "Land substrate",
                "body": "models and API",
                "labels": ["issue-agent"],
            },
        }],
        concurrency_limit=1,
        metadata={"source": "mcp-test"},
    )
    tools["list_playbooks"](project="glimmung")
    tools["get_playbook"]("glimmung", "pb-1")
    tools["run_playbook"]("glimmung", "pb-1")

    assert created["path"] == "/v1/playbooks"
    assert created["json"] == {
        "project": "glimmung",
        "title": "Coordinated rollout",
        "description": "storage slice",
        "entries": [{
            "id": "one",
            "issue": {
                "title": "Land substrate",
                "body": "models and API",
                "labels": ["issue-agent"],
            },
        }],
        "metadata": {"source": "mcp-test"},
        "concurrency_limit": 1,
    }
    assert client.calls[-4:] == [
        ("POST", "/v1/playbooks", None, created["json"]),
        ("GET", "/v1/playbooks", {"project": "glimmung"}, None),
        ("GET", "/v1/playbooks/glimmung/pb-1", None, None),
        ("POST", "/v1/playbooks/glimmung/pb-1/run", None, None),
    ]


def test_browser_inspector_tool_uses_shared_inspector(monkeypatch) -> None:
    tools, _client = _registered_tools()
    calls: list[dict[str, Any]] = []

    def fake_inspect_url(**kwargs: Any) -> dict[str, Any]:
        calls.append(kwargs)
        return {"final_url": kwargs["url"], "elements": []}

    monkeypatch.setattr("mcp_glimmung.tools.inspect_url", fake_inspect_url)

    result = tools["inspect_browser_url"](
        "https://example.test/app",
        viewport={"width": 390, "height": 844},
        wait_ms=100,
        screenshot=False,
    )

    assert result == {"final_url": "https://example.test/app", "elements": []}
    assert calls == [{
        "url": "https://example.test/app",
        "viewport": {"width": 390, "height": 844},
        "wait_ms": 100,
        "timeout_ms": 30000,
        "screenshot": False,
        "full_page": True,
        "capture_accessibility": False,
        "capture_console": True,
        "capture_network": True,
        "max_elements": 80,
        "body_text_limit": 4000,
    }]


def test_resume_run_posts_native_step_boundary_payload() -> None:
    tools, client = _registered_tools()

    result = tools["resume_run"](
        project="glimmung",
        run_id="run-1",
        entrypoint_phase="agent-execute",
        entrypoint_job_id="agent",
        entrypoint_step_slug="run-agent",
        input_overrides={"namespace": "preview-override"},
        artifact_refs={"repo": "blob://artifacts/source.tgz"},
        context={"operator_note": "resume at agent step"},
        trigger_source={"actor": "codex"},
    )

    assert result["path"] == "/v1/runs/glimmung/run-1/resume"
    assert result["json"] == {
        "entrypoint_phase": "agent-execute",
        "entrypoint_job_id": "agent",
        "entrypoint_step_slug": "run-agent",
        "input_overrides": {"namespace": "preview-override"},
        "artifact_refs": {"repo": "blob://artifacts/source.tgz"},
        "context": {"operator_note": "resume at agent step"},
        "trigger_source": {
            "kind": "resume_via_mcp",
            "resumed_from_run_id": "run-1",
            "actor": "codex",
        },
    }


def test_get_native_run_events_calls_hot_log_surface() -> None:
    tools, client = _registered_tools()

    result = tools["get_native_run_events"](
        project="ambience",
        run_id="run-1",
        attempt_index=2,
        job_id="agent",
        limit=25,
    )

    assert result["path"] == "/v1/runs/ambience/run-1/native/events"
    assert client.calls[-1] == (
        "GET",
        "/v1/runs/ambience/run-1/native/events",
        {"attempt_index": 2, "job_id": "agent", "limit": 25},
        None,
    )
