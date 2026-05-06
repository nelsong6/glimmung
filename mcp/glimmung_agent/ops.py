from __future__ import annotations

import json
import os
import ssl
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


REGISTRY_NAME = "romainecr"
IMAGE_REPOSITORY = "glimmung"
PROD_NAMESPACE = "glimmung"
ISSUE_CHART_PATH = "k8s/issue"
REPO_SLUG_DEFAULT = "nelsong6/glimmung"


class CommandError(RuntimeError):
    """Raised when an underlying command fails."""


def repo_root() -> Path:
    candidate = os.environ.get("GLIMMUNG_REPO_ROOT")
    if candidate:
        return Path(candidate).resolve()
    return Path.cwd().resolve()


def run_command(command: list[str], *, cwd: Path | None = None) -> str:
    result = subprocess.run(
        command,
        cwd=str(cwd or repo_root()),
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        raise CommandError(
            "\n".join(
                [
                    f"Command failed: {' '.join(command)}",
                    f"exit_code={result.returncode}",
                    result.stdout.strip(),
                    result.stderr.strip(),
                ]
            ).strip()
        )
    return result.stdout.strip()


def acr_repository_tag(*, image_tag: str, image_repository: str = IMAGE_REPOSITORY) -> str:
    try:
        return run_command(
            [
                "az", "acr", "repository", "show-tags",
                "--name", REGISTRY_NAME,
                "--repository", image_repository,
                "--query", f"[?@=='{image_tag}'] | [0]",
                "--output", "tsv",
            ]
        )
    except CommandError:
        return ""


# ---------------------------------------------------------------------------
# Image build
# ---------------------------------------------------------------------------

def build_preview_image(*, image_tag: str) -> dict:
    """`az acr build` from the current workspace; return the full image ref."""
    registry_server = f"{REGISTRY_NAME}.azurecr.io"
    image = f"{registry_server}/{IMAGE_REPOSITORY}:{image_tag}"

    existing_tag = acr_repository_tag(image_tag=image_tag)
    if existing_tag != image_tag:
        run_command(
            [
                "az", "acr", "build",
                "--registry", REGISTRY_NAME,
                "--image", f"{IMAGE_REPOSITORY}:{image_tag}",
                str(repo_root()),
            ]
        )

    verified = acr_repository_tag(image_tag=image_tag)
    if verified != image_tag:
        raise CommandError(f"image tag {image_tag!r} not present in {registry_server}/{IMAGE_REPOSITORY} after build")
    return {"image": image, "image_tag": image_tag, "skipped_build": existing_tag == image_tag}


def rebuild_validation_image(
    *,
    release: str,
    namespace: str,
    branch: str,
    image_tag: str,
    repo_slug: str = REPO_SLUG_DEFAULT,
) -> dict:
    """Build a fresh image from the agent's pushed branch and roll the
    issue release onto it. Distinct image tag (-r2) so the original and
    rebuild are both traceable; cleanup reaps both."""
    registry_server = f"{REGISTRY_NAME}.azurecr.io"
    image = f"{registry_server}/{IMAGE_REPOSITORY}:{image_tag}"

    existing_tag = acr_repository_tag(image_tag=image_tag)
    if existing_tag != image_tag:
        run_command(
            [
                "az", "acr", "build",
                "--registry", REGISTRY_NAME,
                "--image", f"{IMAGE_REPOSITORY}:{image_tag}",
                f"https://github.com/{repo_slug}.git#{branch}",
            ]
        )
    run_command(
        [
            "kubectl", "-n", namespace, "set", "image",
            f"deployment/{release}",
            f"glimmung={image}",
        ]
    )
    run_command(
        [
            "kubectl", "-n", namespace, "rollout", "status",
            f"deployment/{release}",
            "--timeout=5m",
        ]
    )
    return {
        "release": release,
        "namespace": namespace,
        "image": image,
        "image_tag": image_tag,
        "skipped_build": existing_tag == image_tag,
    }


# ---------------------------------------------------------------------------
# Issue-chart helm lifecycle
# ---------------------------------------------------------------------------

def deploy_preview(
    *,
    release: str,
    namespace: str,
    image_tag: str,
    public_host: str,
    pr_number: str = "",
    timeout: str = "5m",
) -> dict:
    """`helm upgrade --install` the per-issue chart at k8s/issue/ into the
    prod glimmung namespace under the per-run release name. Reuses the
    prod release's SA + glimmung-secrets — those are the only shared
    pieces the per-issue Deployment depends on."""
    cmd = [
        "helm", "upgrade", "--install", release, str(repo_root() / ISSUE_CHART_PATH),
        "--namespace", namespace,
        "--set-string", f"image.tag={image_tag}",
        "--set-string", f"hostname={public_host}",
        "--wait",
        "--timeout", timeout,
    ]
    if pr_number:
        cmd.extend(["--set-string", f"prNumber={pr_number}"])
    run_command(cmd)
    return {
        "release": release,
        "namespace": namespace,
        "url": f"https://{public_host}",
    }


def label_release_pr(*, release: str, namespace: str, pr_number: str) -> dict:
    """Add `glimmung.io/pr=<N>` to the issue release's resources after the
    PR opens. Pre-#69 cleanup workflow path; kept for legacy callers.

    Under #69 (glimmung-opens-PR), the agent doesn't know the PR number
    at workflow-run time — use `label_release_branch` instead.
    """
    for kind in ("deployment", "service", "httproute"):
        run_command(
            [
                "kubectl", "-n", namespace, "label",
                kind, release,
                f"glimmung.io/pr={pr_number}",
                "--overwrite",
            ]
        )
    return {"release": release, "namespace": namespace, "pr_number": pr_number}


def label_release_branch(*, release: str, namespace: str, branch_slug: str) -> dict:
    """Add `glimmung.io/branch=<slug>` to the issue release's resources at
    apply time so cleanup can find the release by branch (the agent doesn't
    have the PR number until glimmung opens the PR post-workflow)."""
    for kind in ("deployment", "service", "httproute"):
        run_command(
            [
                "kubectl", "-n", namespace, "label",
                kind, release,
                f"glimmung.io/branch={branch_slug}",
                "--overwrite",
            ]
        )
    return {"release": release, "namespace": namespace, "branch_slug": branch_slug}


def destroy_preview(*, release: str, namespace: str) -> dict:
    subprocess.run(
        ["helm", "uninstall", release, "--namespace", namespace, "--wait"],
        cwd=str(repo_root()),
        capture_output=True,
        text=True,
        check=False,
    )
    return {"release": release, "namespace": namespace, "destroyed": True}


# ---------------------------------------------------------------------------
# HTTP wait
# ---------------------------------------------------------------------------

def wait_http_ready(*, url: str, timeout_seconds: int = 900, interval_seconds: int = 5) -> dict:
    deadline = time.time() + timeout_seconds
    last_error = ""
    while time.time() < deadline:
        try:
            req = urllib.request.Request(url, method="GET")
            with urllib.request.urlopen(req, timeout=10, context=ssl.create_default_context()) as resp:
                status = resp.getcode()
                if 200 <= status < 400:
                    return {"ready": True, "status": status, "url": url}
                last_error = f"unexpected status {status}"
        except urllib.error.URLError as err:
            last_error = str(err)
        time.sleep(interval_seconds)
    raise CommandError(f"timed out waiting for {url}: {last_error}")


def wait_public_preview(*, url: str, timeout_seconds: int = 900) -> dict:
    health_url = urllib.parse.urljoin(url.rstrip("/") + "/", "healthz")
    return wait_http_ready(url=health_url, timeout_seconds=timeout_seconds)


# ---------------------------------------------------------------------------
# Agent Job lifecycle
# ---------------------------------------------------------------------------

# Bash that runs inside the agent container. Plain Python string — no
# f-string interpolation. All `${VAR}` references are evaluated by the
# container's bash from the env block below (REPO_SLUG, GH_TOKEN,
# ISSUE_NUMBER, BRANCH_NAME, etc).
_AGENT_BASH_SCRIPT = r"""set -euo pipefail

# Pre-create evidence dirs the agent writes into. Sibling of /workspace/repo
# (the clone root) so `git add -A` doesn't pick PNGs/notes up. Workflow
# extracts /workspace/evidence from pod stdout via base64-tar markers
# emitted at the end of this script.
mkdir -p /workspace/evidence/screenshots

# Seed claude state — placeholder credentials so claude never tries to
# refresh, project trust + onboarding flags so it boots straight into the run.
mkdir -p $HOME/.claude
cat > $HOME/.claude/.credentials.json <<'EOF'
{
  "claudeAiOauth": {
    "accessToken": "managed-by-tank-operator",
    "refreshToken": "managed-by-tank-operator",
    "expiresAt": 9999999999000,
    "scopes": ["user:inference", "user:profile"],
    "subscriptionType": "max",
    "rateLimitTier": "max"
  }
}
EOF
chmod 600 $HOME/.claude/.credentials.json
cat > $HOME/.claude/settings.json <<'EOF'
{"theme":"dark","permissions":{"defaultMode":"bypassPermissions"},"skipDangerousModePermissionPrompt":true}
EOF
cat > $HOME/.claude.json <<'EOF'
{
  "hasCompletedOnboarding": true,
  "officialMarketplaceAutoInstallAttempted": true,
  "officialMarketplaceAutoInstalled": true,
  "projects": {
    "/workspace/repo": {
      "allowedTools": [],
      "hasTrustDialogAccepted": true,
      "projectOnboardingSeenCount": 1
    }
  }
}
EOF

git config --global user.name "glimmung-agent[bot]"
git config --global user.email "glimmung-agent@romaine.life"

git clone "https://x-access-token:${GH_TOKEN}@github.com/${REPO_SLUG}.git" /workspace/repo
cd /workspace/repo
git checkout -B "${BRANCH_NAME}"

issue_ref="${ISSUE_NUMBER:-${GLIMMUNG_ISSUE_ID}}"
cat > /tmp/issue-context.md <<EOF
# Issue ${issue_ref}: ${ISSUE_TITLE}
URL: ${ISSUE_URL}
Validation env: ${VALIDATION_URL}
EOF
cat /agent-config/prompt.md /tmp/issue-context.md > /tmp/agent-input.md

# stream-json + verbose so the GHA step surfaces tool calls + partial
# messages live instead of going silent for the whole run.
cat /tmp/agent-input.md | claude \
  --print \
  --output-format stream-json \
  --verbose \
  --dangerously-skip-permissions \
  2>&1 | tee /tmp/claude-stream.log

# Refuse to publish runner-local config files. The prompt tells the
# agent not to touch these; this is the second line of defense.
BLOCKED=$(git status --porcelain -- .github/workflows .github/agent .mcp.json 2>/dev/null || true)
if [ -n "$BLOCKED" ]; then
  echo "agent modified runner-local config files (forbidden by prompt):" >&2
  echo "$BLOCKED" >&2
  exit 1
fi

git add -A
if git diff --cached --quiet; then
  echo "agent produced no changes; failing job so the workflow doesn't open an empty PR" >&2
  exit 1
fi
git commit -m "agent: address issue #${ISSUE_NUMBER}

${ISSUE_TITLE}

Closes #${ISSUE_NUMBER}"

# Sync onto current main before pushing — main may have moved during
# the agent run, and pushing a stale branch is rejected by GitHub's
# workflow-permission check even when the agent's commit doesn't touch
# .github/workflows. Rebase replays the single commit; conflicts fail
# loudly rather than ship a stale-base branch.
git fetch origin main
git rebase origin/main

git push origin "HEAD:${BRANCH_NAME}"

# Stream evidence as a base64-tar to stdout between markers the workflow
# extracts via sed. Can't kubectl cp from a Succeeded pod (exec-tar
# requires a live container), so logs are the side-channel — preserved
# regardless of pod state. Empty evidence dir is fine.
if [ -d /workspace/evidence ]; then
  echo "===EVIDENCE-TAR-START==="
  tar -czf - -C /workspace/evidence . | base64
  echo "===EVIDENCE-TAR-END==="
fi
"""


def _agent_job_spec(
    *,
    namespace: str,
    job_name: str,
    issue_number: str,
    issue_id: str,
    issue_title: str,
    issue_url: str,
    validation_url: str,
    branch_name: str,
    proxy_ip: str,
    agent_container_tag: str,
    repo_slug: str,
) -> dict:
    return {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": job_name,
            "namespace": namespace,
            "labels": {
                "app.kubernetes.io/name": "glimmung-agent",
                "glimmung.io/issue": str(issue_number or issue_id),
            },
        },
        "spec": {
            "backoffLimit": 0,
            "ttlSecondsAfterFinished": 1800,
            "template": {
                "metadata": {
                    "labels": {
                        "app.kubernetes.io/name": "glimmung-agent",
                        "glimmung.io/issue": str(issue_number or issue_id),
                    },
                },
                "spec": {
                    "restartPolicy": "Never",
                    # claude --dangerously-skip-permissions refuses to run as root.
                    "securityContext": {
                        "runAsUser": 1000,
                        "runAsGroup": 1000,
                        "fsGroup": 1000,
                        "runAsNonRoot": True,
                    },
                    "hostAliases": [
                        {"ip": proxy_ip, "hostnames": ["api.anthropic.com"]},
                    ],
                    "volumes": [
                        {
                            "name": "claude-ca",
                            "configMap": {
                                "name": "claude-oauth-ca",
                                "items": [{"key": "ca.crt", "path": "ca.crt"}],
                            },
                        },
                        {"name": "workspace", "emptyDir": {}},
                        {"name": "agent-config", "configMap": {"name": "agent-config"}},
                    ],
                    "containers": [
                        {
                            "name": "agent",
                            "image": f"romainecr.azurecr.io/agent-container:{agent_container_tag}",
                            "imagePullPolicy": "IfNotPresent",
                            "command": ["/bin/bash", "-c", _AGENT_BASH_SCRIPT],
                            "env": [
                                {"name": "NODE_EXTRA_CA_CERTS", "value": "/etc/claude-ca/ca.crt"},
                                {"name": "HOME", "value": "/workspace"},
                                {"name": "ISSUE_NUMBER", "value": str(issue_number)},
                                {"name": "GLIMMUNG_ISSUE_ID", "value": str(issue_id)},
                                {"name": "ISSUE_TITLE", "value": issue_title},
                                {"name": "ISSUE_URL", "value": issue_url},
                                {"name": "VALIDATION_URL", "value": validation_url},
                                {"name": "BRANCH_NAME", "value": branch_name},
                                {"name": "REPO_SLUG", "value": repo_slug},
                                {
                                    "name": "GH_TOKEN",
                                    "valueFrom": {
                                        "secretKeyRef": {
                                            "name": "agent-github-token",
                                            "key": "token",
                                        },
                                    },
                                },
                                {"name": "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "value": "1"},
                            ],
                            "volumeMounts": [
                                {"name": "claude-ca", "mountPath": "/etc/claude-ca", "readOnly": True},
                                {"name": "workspace", "mountPath": "/workspace"},
                                {"name": "agent-config", "mountPath": "/agent-config", "readOnly": True},
                            ],
                        },
                    ],
                },
            },
        },
    }


def apply_agent_job(
    *,
    namespace: str,
    job_name: str,
    issue_number: str,
    issue_id: str = "",
    issue_title: str,
    issue_url: str,
    validation_url: str,
    branch_name: str,
    proxy_ip: str,
    agent_container_tag: str,
    repo_slug: str = REPO_SLUG_DEFAULT,
) -> dict:
    spec = _agent_job_spec(
        namespace=namespace,
        job_name=job_name,
        issue_number=issue_number,
        issue_id=issue_id,
        issue_title=issue_title,
        issue_url=issue_url,
        validation_url=validation_url,
        branch_name=branch_name,
        proxy_ip=proxy_ip,
        agent_container_tag=agent_container_tag,
        repo_slug=repo_slug,
    )
    proc = subprocess.run(
        ["kubectl", "apply", "-f", "-"],
        input=json.dumps(spec),
        capture_output=True,
        text=True,
        check=False,
    )
    if proc.returncode != 0:
        raise CommandError(f"kubectl apply failed: {(proc.stderr or proc.stdout).strip()}")
    return {"namespace": namespace, "job": job_name, "applied": proc.stdout.strip()}


def wait_agent_job(
    *,
    namespace: str,
    job_name: str,
    timeout_seconds: int = 1800,
    poll_interval_seconds: int = 3,
) -> dict:
    """Two-stage wait: poll Pod to Running|Succeeded|Failed, stream its
    logs (blocks until pod terminates), then poll Job .status.{succeeded,
    failed} to terminal. Fast-fails on Job failure rather than waiting
    out the full timeout."""
    deadline = time.time() + timeout_seconds

    pod_name = ""
    phase = ""
    while time.time() < deadline:
        pod_name = run_command(
            [
                "kubectl", "-n", namespace, "get", "pods",
                "-l", f"job-name={job_name}",
                "-o", "jsonpath={.items[0].metadata.name}",
            ],
        )
        if pod_name:
            phase = run_command(
                [
                    "kubectl", "-n", namespace, "get", "pod", pod_name,
                    "-o", "jsonpath={.status.phase}",
                ],
            )
            if phase in ("Running", "Succeeded", "Failed"):
                break
        time.sleep(poll_interval_seconds)

    if not pod_name:
        raise CommandError(f"agent pod for Job {job_name!r} never appeared")

    print(f"agent pod {pod_name} (phase={phase}) — streaming logs", flush=True)
    subprocess.run(
        ["kubectl", "-n", namespace, "logs", "-f", pod_name],
        check=False,
    )

    succeeded = ""
    failed = ""
    while time.time() < deadline:
        succeeded = run_command(
            ["kubectl", "-n", namespace, "get", "job", job_name, "-o", "jsonpath={.status.succeeded}"],
        )
        failed = run_command(
            ["kubectl", "-n", namespace, "get", "job", job_name, "-o", "jsonpath={.status.failed}"],
        )
        if succeeded or failed:
            break
        time.sleep(2)

    if (int(succeeded) if succeeded else 0) >= 1:
        return {
            "namespace": namespace,
            "job": job_name,
            "pod": pod_name,
            "succeeded": int(succeeded),
            "failed": int(failed) if failed else 0,
        }
    raise CommandError(
        f"agent Job {job_name!r} failed (succeeded={succeeded or 0}, failed={failed or 0})"
    )
