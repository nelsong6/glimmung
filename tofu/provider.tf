# Remote state in Azure Storage (backend config passed via -backend-config in CI).
# OIDC auth — no static credentials.

terraform {
  backend "azurerm" {}
}

provider "azurerm" {
  features {}
  use_oidc = true
}
