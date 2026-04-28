# References to shared infrastructure provisioned by infra-bootstrap. Resolved
# via data sources at plan time so we don't store duplicated IDs.

locals {
  infra = {
    resource_group_name = "infra"
  }
}

data "azurerm_cosmosdb_account" "infra" {
  name                = "infra-cosmos-serverless"
  resource_group_name = local.infra.resource_group_name
}
