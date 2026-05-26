# ============================================================================
# Workload identity for glimmung
# ============================================================================
# Glimmung previously reused `infra-shared-identity` (provisioned in
# infra-bootstrap) via a per-app federated credential keyed on
# `system:serviceaccount:glimmung:infra-shared`. That shared identity holds
# Cosmos Data Contributor at *account* scope, Key Vault Secrets User on the
# whole vault, and Storage Blob Data Contributor at subscription scope.
#
# Glimmung-owned identities live in the `glimmung` resource group:
# - `glimmung-identity` for the API/dashboard pod
# - `glimmung-native-runner-identity` for native Kubernetes runner Jobs
#
# Federated credentials use exact-match subjects. The API/dashboard pod
# remains on `system:serviceaccount:glimmung:infra-shared` to avoid a
# chart/service-account rename in this infra slice. Native runner Jobs use
# `system:serviceaccount:glimmung-runs:glimmung-native-runner`.
# ============================================================================

data "azurerm_resource_group" "infra" {
  name = local.infra.resource_group_name
}

resource "azurerm_resource_group" "glimmung" {
  name     = "glimmung"
  location = data.azurerm_resource_group.infra.location
}

data "azurerm_container_registry" "romaine" {
  name                = "romainecr"
  resource_group_name = local.infra.resource_group_name
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

resource "azurerm_federated_identity_credential" "glimmung_dedicated" {
  name                = "aks-glimmung"
  resource_group_name = azurerm_resource_group.glimmung.name
  parent_id           = azurerm_user_assigned_identity.glimmung_dedicated.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = local.aks_oidc_issuer_url
  subject             = "system:serviceaccount:glimmung:infra-shared"
}

resource "azurerm_federated_identity_credential" "native_runner" {
  name                = "aks-glimmung-native-runner"
  resource_group_name = azurerm_resource_group.glimmung.name
  parent_id           = azurerm_user_assigned_identity.native_runner.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = local.aks_oidc_issuer_url
  subject             = "system:serviceaccount:glimmung-runs:glimmung-native-runner"
}

resource "azurerm_role_assignment" "native_runner_acr_push" {
  scope                = data.azurerm_container_registry.romaine.id
  role_definition_name = "AcrPush"
  principal_id         = azurerm_user_assigned_identity.native_runner.principal_id
}

# Native app runners use `az acr build` for validation images because the
# Kubernetes job does not have a Docker daemon. AcrPush covers direct image
# push/pull, but ACR Tasks are management-plane operations on the registry.
resource "azurerm_role_assignment" "native_runner_acr_build_contributor" {
  scope                = data.azurerm_container_registry.romaine.id
  role_definition_name = "Contributor"
  principal_id         = azurerm_user_assigned_identity.native_runner.principal_id
}

resource "azurerm_role_assignment" "glimmung_dedicated_subscription_contributor" {
  scope                = "/subscriptions/${data.azurerm_client_config.current.subscription_id}"
  role_definition_name = "Contributor"
  principal_id         = azurerm_user_assigned_identity.glimmung_dedicated.principal_id
  principal_type       = "ServicePrincipal"
}

resource "azurerm_role_assignment" "glimmung_dedicated_subscription_rbac_admin" {
  scope                = "/subscriptions/${data.azurerm_client_config.current.subscription_id}"
  role_definition_name = "Role Based Access Control Administrator"
  principal_id         = azurerm_user_assigned_identity.glimmung_dedicated.principal_id
  principal_type       = "ServicePrincipal"
}

output "glimmung_dedicated_identity_client_id" {
  value       = azurerm_user_assigned_identity.glimmung_dedicated.client_id
  description = "client_id of the Glimmung-owned glimmung-identity. Pin this into k8s/values.yaml."
}

output "glimmung_native_runner_identity_client_id" {
  value       = azurerm_user_assigned_identity.native_runner.client_id
  description = "client_id of glimmung-native-runner-identity. Use for the glimmung-runs runner ServiceAccount annotation."
}
