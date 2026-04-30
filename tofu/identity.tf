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
# Pattern mirrors tank-operator/infra/{api_proxy,credential_refresher}.tf.
#
# One federated credential, exact-match subject. Both prod and per-issue
# agent-CI helm releases run in the `glimmung` namespace under SA
# `infra-shared`, distinguished by Helm release name (resource names
# templated off `.Release.Name`). A single subject covers both because
# they live in the same namespace.
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

resource "azurerm_federated_identity_credential" "glimmung" {
  name                = "aks-glimmung"
  resource_group_name = local.infra.resource_group_name
  parent_id           = azurerm_user_assigned_identity.glimmung.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = data.azurerm_kubernetes_cluster.infra.oidc_issuer_url
  subject             = "system:serviceaccount:glimmung:infra-shared"
}

# Surface the client-id so k8s/values.yaml can pin the SA annotation
# (see k8s/templates/serviceaccount.yaml). Output rather than referencing
# from the chart at apply-time so the chart stays a pure Helm chart.
output "glimmung_identity_client_id" {
  value       = azurerm_user_assigned_identity.glimmung.client_id
  description = "client_id of glimmung-identity. Pin this into k8s/values.yaml for the SA workload-identity annotation."
}
