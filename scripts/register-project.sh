#!/usr/bin/env bash
# Register a project + workflow + slot hosts with glimmung's admin API.
#
# Glimmung's admin endpoints accept an Entra ID JWT bearer token validated
# via JWKS, gated by an email allowlist (glimmung-oauth-allowed-emails KV
# secret). The simplest way to mint one from a workstation is the Azure CLI.
#
# Usage:
#   ./register-project.sh ambience nelsong6/ambience agent-run agent:run \
#       ambience-slot-1 ambience-slot-2 ambience-slot-3
#
# Args:
#   $1            project name (e.g. ambience)
#   $2            github_repo (owner/repo)
#   $3            workflow name (e.g. agent-run)
#   $4            trigger label (e.g. agent:run)
#   $5..          slot host names to register
#
# Capability vocabulary is hardcoded for the agent pattern:
#   {"project": "<project>", "role": "agent"}
# Adjust below if a project needs a different shape.

set -euo pipefail

if [ "$#" -lt 5 ]; then
  echo "usage: $0 <project> <github_repo> <workflow_name> <trigger_label> <host_name>..." >&2
  exit 2
fi

PROJECT="$1"
REPO="$2"
WORKFLOW_NAME="$3"
TRIGGER_LABEL="$4"
shift 4
HOSTS=("$@")

GLIMMUNG_BASE="${GLIMMUNG_BASE:-https://glimmung.romaine.life}"

# Resolve the glimmung-oauth client_id (audience) from KV — this is what
# the API expects in the JWT's `aud` claim.
CLIENT_ID="$(az keyvault secret show \
  --vault-name romaine-kv \
  --name glimmung-oauth-client-id \
  --query value -o tsv)"

if [ -z "$CLIENT_ID" ]; then
  echo "could not resolve glimmung-oauth-client-id from KV; are you logged in to az?" >&2
  exit 1
fi

TOKEN="$(az account get-access-token --resource "$CLIENT_ID" --query accessToken -o tsv)"

post() {
  local path="$1"
  local body="$2"
  echo "POST ${path}"
  curl -fsS -X POST \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d "$body" \
    "${GLIMMUNG_BASE}${path}"
  echo
}

post /v1/projects "$(jq -nc \
  --arg name "$PROJECT" \
  --arg repo "$REPO" \
  '{name: $name, github_repo: $repo}')"

post /v1/workflows "$(jq -nc \
  --arg project "$PROJECT" \
  --arg name "$WORKFLOW_NAME" \
  --arg label "$TRIGGER_LABEL" \
  --arg project_role "$PROJECT" \
  '{
    project: $project,
    name: $name,
    workflow_filename: ($name + ".yml"),
    workflow_ref: "main",
    trigger_label: $label,
    default_requirements: {project: $project_role, role: "agent"}
  }')"

for HOST in "${HOSTS[@]}"; do
  post /v1/hosts "$(jq -nc \
    --arg host "$HOST" \
    --arg project "$PROJECT" \
    '{
      name: $host,
      capabilities: {project: $project, role: "agent"}
    }')"
done

echo "Done. Verify via: curl -s ${GLIMMUNG_BASE}/v1/state | jq"
