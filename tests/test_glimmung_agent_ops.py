from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "mcp"))

from glimmung_agent import ops


def test_build_preview_image_skips_existing_acr_tag(monkeypatch):
    commands: list[list[str]] = []

    def fake_run_command(command: list[str], *, cwd: Path | None = None) -> str:
        commands.append(command)
        if command[:4] == ["az", "acr", "repository", "show-tags"]:
            return "issue-1"
        raise AssertionError(f"unexpected command: {command}")

    monkeypatch.setattr(ops, "run_command", fake_run_command)

    result = ops.build_preview_image(image_tag="issue-1")

    assert result["image"] == "romainecr.azurecr.io/glimmung:issue-1"
    assert result["skipped_build"] is True
    assert not any(command[:3] == ["az", "acr", "build"] for command in commands)


def test_build_preview_image_builds_missing_acr_tag(monkeypatch):
    commands: list[list[str]] = []
    show_calls = 0

    def fake_run_command(command: list[str], *, cwd: Path | None = None) -> str:
        nonlocal show_calls
        commands.append(command)
        if command[:4] == ["az", "acr", "repository", "show-tags"]:
            show_calls += 1
            return "" if show_calls == 1 else "issue-1"
        if command[:3] == ["az", "acr", "build"]:
            return ""
        raise AssertionError(f"unexpected command: {command}")

    monkeypatch.setattr(ops, "run_command", fake_run_command)

    result = ops.build_preview_image(image_tag="issue-1")

    assert result["skipped_build"] is False
    assert any(command[:3] == ["az", "acr", "build"] for command in commands)


def test_rebuild_validation_image_uses_kubectl_rollout_and_skips_existing_build(monkeypatch):
    commands: list[list[str]] = []

    def fake_run_command(command: list[str], *, cwd: Path | None = None) -> str:
        commands.append(command)
        if command[:4] == ["az", "acr", "repository", "show-tags"]:
            return "issue-1-r2"
        if command[:4] == ["kubectl", "-n", "glimmung", "set"]:
            return ""
        if command[:4] == ["kubectl", "-n", "glimmung", "rollout"]:
            return ""
        raise AssertionError(f"unexpected command: {command}")

    monkeypatch.setattr(ops, "run_command", fake_run_command)

    result = ops.rebuild_validation_image(
        release="issue-1",
        namespace="glimmung",
        branch="glimmung/run-1",
        image_tag="issue-1-r2",
    )

    assert result["skipped_build"] is True
    assert not any(command[:3] == ["az", "acr", "build"] for command in commands)
    assert [
        "kubectl", "-n", "glimmung", "set", "image",
        "deployment/issue-1", "glimmung=romainecr.azurecr.io/glimmung:issue-1-r2",
    ] in commands
    assert [
        "kubectl", "-n", "glimmung", "rollout", "status",
        "deployment/issue-1", "--timeout=5m",
    ] in commands
    assert not any(command[:2] == ["helm", "upgrade"] for command in commands)
