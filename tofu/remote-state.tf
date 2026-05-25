# References to shared infrastructure provisioned by infra-bootstrap. Resolved
# via data sources at plan time so we don't store duplicated IDs.

locals {
  infra = {
    resource_group_name   = "infra"
    shared_key_vault_name = "romaine-kv"
  }
}

data "azurerm_cosmosdb_account" "infra" {
  name                = "infra-cosmos-serverless"
  resource_group_name = local.infra.resource_group_name
}

data "azuread_client_config" "current" {}

data "azurerm_client_config" "current" {}

data "terraform_remote_state" "infra_bootstrap" {
  backend = "azurerm"

  config = {
    resource_group_name  = "infra"
    storage_account_name = "nelsontofu"
    container_name       = "tfstate"
    key                  = "infra-bootstrap.tfstate"
    use_oidc             = true
  }
}

locals {
  aks_oidc_issuer_url = data.terraform_remote_state.infra_bootstrap.outputs.aks_oidc_issuer_url
}

data "azurerm_key_vault" "shared" {
  name                = local.infra.shared_key_vault_name
  resource_group_name = local.infra.resource_group_name
}

data "azurerm_user_assigned_identity" "external_secrets" {
  name                = "infra-shared-identity"
  resource_group_name = local.infra.resource_group_name
}
