# ============================================================================
# Workload identity for glimmung
# ============================================================================
# Glimmung previously reused `infra-shared-identity` (provisioned in
# infra-bootstrap) via a per-app federated credential keyed on
# `system:serviceaccount:glimmung:infra-shared`. That shared identity holds
# Cosmos Data Contributor at *account* scope, Key Vault Secrets User on the
# whole vault, and Storage Blob Data Contributor at subscription scope —
# every opted-in app could read every other app's data plane. This file
# replaces that arrangement with an identity scoped to glimmung's blast
# radius only: data-plane on the `glimmung` Cosmos database, nothing else.
#
# Two federated credentials cover the two ways a glimmung pod runs today:
#   1. Prod pod in the `glimmung` namespace under SA `infra-shared`.
#   2. Per-issue agent-CI pods in `glimmung-issue-<N>-<run>-<sha>`
#      namespaces (created on the fly), same SA name. Subject claims for
#      these can't be enumerated, so the second cred uses a claims-matching
#      expression to accept any namespace fitting the pattern.
# ============================================================================

data "azurerm_resource_group" "infra" {
  name = local.infra.resource_group_name
}

data "azurerm_kubernetes_cluster" "infra" {
  name                = "infra-aks"
  resource_group_name = local.infra.resource_group_name
}

resource "azurerm_user_assigned_identity" "glimmung" {
  name                = "glimmung-identity"
  resource_group_name = data.azurerm_resource_group.infra.name
  location            = data.azurerm_resource_group.infra.location
}

# Cosmos DB Built-in Data Contributor (00000000-0000-0000-0000-000000000002)
# scoped to the glimmung database only — not the account. Other apps' data on
# the same `infra-cosmos-serverless` account stays unreachable from this
# identity even if a glimmung pod is compromised.
#
# `scope` is hand-built rather than `azurerm_cosmosdb_sql_database.glimmung.id`
# because Cosmos data-plane RBAC uses its own path scheme (`/dbs/<name>`)
# distinct from the ARM resource ID (`/sqlDatabases/<name>`); passing the
# ARM ID gets rejected by the Cosmos service with "Expected path segment
# [dbs] at position [0] but found [sqlDatabases]."
resource "azurerm_cosmosdb_sql_role_assignment" "glimmung_cosmos" {
  resource_group_name = local.infra.resource_group_name
  account_name        = data.azurerm_cosmosdb_account.infra.name
  role_definition_id  = "${data.azurerm_cosmosdb_account.infra.id}/sqlRoleDefinitions/00000000-0000-0000-0000-000000000002"
  principal_id        = azurerm_user_assigned_identity.glimmung.principal_id
  scope               = "${data.azurerm_cosmosdb_account.infra.id}/dbs/${azurerm_cosmosdb_sql_database.glimmung.name}"
}

# Prod pod: exact-match subject. The SA name `infra-shared` is a relic of
# the shared-identity era; renaming the SA is a separate cleanup.
resource "azurerm_federated_identity_credential" "glimmung_prod" {
  name                = "aks-glimmung-prod"
  resource_group_name = local.infra.resource_group_name
  parent_id           = azurerm_user_assigned_identity.glimmung.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = data.azurerm_kubernetes_cluster.infra.oidc_issuer_url
  subject             = "system:serviceaccount:glimmung:infra-shared"
}

# Per-issue agent-CI pods: claims-matching expression. AKS issues OIDC
# tokens with `sub=system:serviceaccount:<ns>:<sa>`; the expression matches
# any `glimmung-issue-*` namespace whose pod is running under SA
# `infra-shared`. The combined startsWith/endsWith form replaces a single
# regex `matches()` call: Microsoft's flexible-FIC grammar (the one that
# parses `claimsMatchingExpression.value`) doesn't accept method-call
# syntax like `claims['sub'].matches(...)` — it rejects the `.` between
# the indexer and the method with "symbol '.': Input doesn't match any
# rule in grammar!". `startsWith`/`endsWith`/`&&` ARE in the grammar.
#
# Provisioned via azapi rather than azurerm_federated_identity_credential
# because the azurerm provider (pinned to 4.70 by infra-bootstrap's shared
# providers) doesn't expose `claimsMatchingExpression` yet — the resource
# only accepts a literal `subject` string. API version 2025-01-31-preview
# is the first one whose schema (as bundled with azapi 2.9.0) exposes
# `claimsMatchingExpression`.
resource "azapi_resource" "glimmung_per_issue_fic" {
  type      = "Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2025-01-31-preview"
  parent_id = azurerm_user_assigned_identity.glimmung.id
  name      = "aks-glimmung-per-issue"
  body = {
    properties = {
      issuer    = data.azurerm_kubernetes_cluster.infra.oidc_issuer_url
      audiences = ["api://AzureADTokenExchange"]
      claimsMatchingExpression = {
        value           = "claims['sub'].startsWith('system:serviceaccount:glimmung-issue-') && claims['sub'].endsWith(':infra-shared')"
        languageVersion = 1
      }
    }
  }
}

# Surface the client-id so k8s/values.yaml can pin the SA annotation
# (see k8s/templates/serviceaccount.yaml). Output rather than referencing
# from the chart at apply-time so the chart stays a pure Helm chart.
output "glimmung_identity_client_id" {
  value       = azurerm_user_assigned_identity.glimmung.client_id
  description = "client_id of glimmung-identity. Pin this into k8s/values.yaml for the SA workload-identity annotation."
}
