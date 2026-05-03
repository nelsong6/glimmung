# ============================================================================
# Workload identity for glimmung
# ============================================================================
# Glimmung previously reused `infra-shared-identity` (provisioned in
# infra-bootstrap) via a per-app federated credential keyed on
# `system:serviceaccount:glimmung:infra-shared`. That shared identity holds
# Cosmos Data Contributor at *account* scope, Key Vault Secrets User on the
# whole vault, and Storage Blob Data Contributor at subscription scope.
#
# Glimmung is moving its identities into the `glimmung` resource group:
# - current `glimmung-identity` stays in the infra resource group until the
#   chart is repointed to the dedicated identity output below
# - dedicated `glimmung-identity` in the Glimmung resource group for the
#   API/dashboard pod
# - `glimmung-native-runner-identity` for native Kubernetes runner Jobs
#
# The staged shape avoids recreating the identity currently used by the live
# ServiceAccount during the same apply that creates the replacement. Once
# k8s/values.yaml is pinned to `glimmung_dedicated_identity_client_id`, the
# old infra-RG identity can be removed in a cleanup PR.
#
# Federated credentials use exact-match subjects. The API/dashboard pod
# remains on `system:serviceaccount:glimmung:infra-shared` for now to avoid
# a chart/service-account rename in this infra slice. Native runner Jobs use
# `system:serviceaccount:glimmung-runs:glimmung-native-runner`.
# ============================================================================

data "azurerm_resource_group" "infra" {
  name = local.infra.resource_group_name
}

resource "azurerm_resource_group" "glimmung" {
  name     = "glimmung"
  location = data.azurerm_resource_group.infra.location
}

data "azurerm_kubernetes_cluster" "infra" {
  name                = "infra-aks"
  resource_group_name = local.infra.resource_group_name
}

data "azurerm_container_registry" "romaine" {
  name                = "romainecr"
  resource_group_name = local.infra.resource_group_name
}

resource "azurerm_user_assigned_identity" "glimmung" {
  name                = "glimmung-identity"
  resource_group_name = data.azurerm_resource_group.infra.name
  location            = data.azurerm_resource_group.infra.location
}

resource "azurerm_user_assigned_identity" "glimmung_dedicated" {
  name                = "glimmung-identity"
  resource_group_name = azurerm_resource_group.glimmung.name
  location            = azurerm_resource_group.glimmung.location
}

resource "azurerm_user_assigned_identity" "native_runner" {
  name                = "glimmung-native-runner-identity"
  resource_group_name = azurerm_resource_group.glimmung.name
  location            = azurerm_resource_group.glimmung.location
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

resource "azurerm_cosmosdb_sql_role_assignment" "glimmung_dedicated_cosmos" {
  resource_group_name = local.infra.resource_group_name
  account_name        = data.azurerm_cosmosdb_account.infra.name
  role_definition_id  = "${data.azurerm_cosmosdb_account.infra.id}/sqlRoleDefinitions/00000000-0000-0000-0000-000000000002"
  principal_id        = azurerm_user_assigned_identity.glimmung_dedicated.principal_id
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

resource "azurerm_federated_identity_credential" "glimmung_dedicated" {
  name                = "aks-glimmung"
  resource_group_name = azurerm_resource_group.glimmung.name
  parent_id           = azurerm_user_assigned_identity.glimmung_dedicated.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = data.azurerm_kubernetes_cluster.infra.oidc_issuer_url
  subject             = "system:serviceaccount:glimmung:infra-shared"
}

resource "azurerm_federated_identity_credential" "native_runner" {
  name                = "aks-glimmung-native-runner"
  resource_group_name = azurerm_resource_group.glimmung.name
  parent_id           = azurerm_user_assigned_identity.native_runner.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = data.azurerm_kubernetes_cluster.infra.oidc_issuer_url
  subject             = "system:serviceaccount:glimmung-runs:glimmung-native-runner"
}

resource "azurerm_role_assignment" "native_runner_acr_push" {
  scope                = data.azurerm_container_registry.romaine.id
  role_definition_name = "AcrPush"
  principal_id         = azurerm_user_assigned_identity.native_runner.principal_id
}

# Surface the client-id so k8s/values.yaml can pin the SA annotation
# (see k8s/templates/serviceaccount.yaml). Output rather than referencing
# from the chart at apply-time so the chart stays a pure Helm chart.
output "glimmung_identity_client_id" {
  value       = azurerm_user_assigned_identity.glimmung.client_id
  description = "client_id of the current infra-RG glimmung-identity. k8s/values.yaml stays pinned here until the staged cutover."
}

output "glimmung_dedicated_identity_client_id" {
  value       = azurerm_user_assigned_identity.glimmung_dedicated.client_id
  description = "client_id of the Glimmung-owned glimmung-identity. Pin this into k8s/values.yaml during the staged cutover."
}

output "glimmung_native_runner_identity_client_id" {
  value       = azurerm_user_assigned_identity.native_runner.client_id
  description = "client_id of glimmung-native-runner-identity. Use for the glimmung-runs runner ServiceAccount annotation."
}
