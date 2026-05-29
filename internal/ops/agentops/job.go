package agentops

const AgentBashScript = `set -euo pipefail

# Pre-create evidence dirs the agent writes into. Sibling of /workspace/repo
# (the clone root) so git add -A does not pick PNGs/notes up. The production
# evidence flow is glimmung-native-runner → GLIMMUNG_COMPLETION_FILE refs →
# POST /v1/run-callbacks/.../native/completed; this developer-CLI agent
# script just leaves the files on disk for inspection in the live pod.
mkdir -p /workspace/evidence/screenshots /workspace/evidence/videos

# Seed claude state with placeholder credentials so claude never tries to
# refresh, project trust, and onboarding flags so it boots straight into the run.
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

# stream-json + verbose surfaces tool calls + partial messages live instead of
# going silent for the whole run.
cat /tmp/agent-input.md | claude \
  --print \
  --output-format stream-json \
  --verbose \
  --dangerously-skip-permissions \
  2>&1 | tee /tmp/claude-stream.log

{
  echo "# Agent summary input"
  echo
  echo "## Git status"
  git status --short
  echo
  echo "## Changed files"
  git diff --name-only
  echo
  echo "## Diff stat"
  git diff --stat
  echo
  echo "## Evidence files"
  find /workspace/evidence -type f | sort || true
  echo
  echo "## Interaction log"
  tail -n 400 /tmp/claude-stream.log
} > /tmp/summary-input.md

cat > /tmp/summary-prompt.md <<'EOF'
Summarize this agent run for a human reviewer.

Return concise markdown with:
- What changed
- Verification performed or attempted
- Evidence artifacts produced
- Blockers, caveats, or residual risk

Stick to facts supported by the input. Do not decide whether the change
should merge.
EOF

if cat /tmp/summary-prompt.md /tmp/summary-input.md | claude \
    --print \
    --output-format text \
    --dangerously-skip-permissions \
    > /workspace/evidence/summary.md 2>/tmp/summary-stderr.log; then
  :
else
  {
    echo "## Run summary"
    echo
    echo "_Summary generation failed; see raw agent logs._"
    echo
    sed 's/^/> /' /tmp/summary-stderr.log | head -80
  } > /workspace/evidence/summary.md
fi

# Refuse to publish runner-local config files. The prompt tells the agent not
# to touch these; this is the second line of defense.
BLOCKED=$(git status --porcelain -- .github/workflows .github/agent .mcp.json 2>/dev/null || true)
if [ -n "$BLOCKED" ]; then
  echo "agent modified runner-local config files (forbidden by prompt):" >&2
  echo "$BLOCKED" >&2
  exit 1
fi

git add -A
if git diff --cached --quiet; then
  echo "agent produced no changes; failing job so the workflow does not open an empty PR" >&2
  exit 1
fi
git commit -m "agent: address issue #${ISSUE_NUMBER}

${ISSUE_TITLE}

Closes #${ISSUE_NUMBER}"

# Sync onto current main before pushing. Main may have moved during the agent
# run, and pushing a stale branch is rejected by GitHub's workflow-permission
# check even when the agent's commit does not touch .github/workflows. Rebase
# replays the single commit; conflicts fail loudly rather than ship a stale-base
# branch.
git fetch origin main
git rebase origin/main

git push origin "HEAD:${BRANCH_NAME}"
`

func AgentJobSpec(opts ApplyAgentJobOptions) map[string]any {
	if opts.RepoSlug == "" {
		opts.RepoSlug = RepoSlugDefault
	}
	issueRef := opts.IssueNumber
	if issueRef == "" {
		issueRef = opts.IssueID
	}
	labels := map[string]any{
		"app.kubernetes.io/name": "glimmung-agent",
		"glimmung.io/issue":      issueRef,
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      opts.JobName,
			"namespace": opts.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": 1800,
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": labels,
				},
				"spec": map[string]any{
					"restartPolicy": "Never",
					"securityContext": map[string]any{
						"runAsUser":    1000,
						"runAsGroup":   1000,
						"fsGroup":      1000,
						"runAsNonRoot": true,
					},
					"hostAliases": []any{
						map[string]any{"ip": opts.ProxyIP, "hostnames": []any{"api.anthropic.com"}},
					},
					"volumes": []any{
						map[string]any{
							"name": "claude-ca",
							"configMap": map[string]any{
								"name":  "claude-oauth-ca",
								"items": []any{map[string]any{"key": "ca.crt", "path": "ca.crt"}},
							},
						},
						map[string]any{"name": "workspace", "emptyDir": map[string]any{}},
						map[string]any{"name": "agent-config", "configMap": map[string]any{"name": "agent-config"}},
					},
					"containers": []any{
						map[string]any{
							"name":            "agent",
							"image":           "romainecr.azurecr.io/agent-container:" + opts.AgentContainerTag,
							"imagePullPolicy": "IfNotPresent",
							"command":         []any{"/bin/bash", "-c", AgentBashScript},
							"env": []any{
								map[string]any{"name": "NODE_EXTRA_CA_CERTS", "value": "/etc/claude-ca/ca.crt"},
								map[string]any{"name": "HOME", "value": "/workspace"},
								map[string]any{"name": "ISSUE_NUMBER", "value": opts.IssueNumber},
								map[string]any{"name": "GLIMMUNG_ISSUE_ID", "value": opts.IssueID},
								map[string]any{"name": "ISSUE_TITLE", "value": opts.IssueTitle},
								map[string]any{"name": "ISSUE_URL", "value": opts.IssueURL},
								map[string]any{"name": "VALIDATION_URL", "value": opts.ValidationURL},
								map[string]any{"name": "BRANCH_NAME", "value": opts.BranchName},
								map[string]any{"name": "REPO_SLUG", "value": opts.RepoSlug},
								map[string]any{
									"name": "GH_TOKEN",
									"valueFrom": map[string]any{
										"secretKeyRef": map[string]any{
											"name": "agent-github-token",
											"key":  "token",
										},
									},
								},
								map[string]any{"name": "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "value": "1"},
							},
							"volumeMounts": []any{
								map[string]any{"name": "claude-ca", "mountPath": "/etc/claude-ca", "readOnly": true},
								map[string]any{"name": "workspace", "mountPath": "/workspace"},
								map[string]any{"name": "agent-config", "mountPath": "/agent-config", "readOnly": true},
							},
						},
					},
				},
			},
		},
	}
}
