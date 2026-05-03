# Private Glimmung artifact storage for native runner logs, evidence, issue
# attachments, and Report artifacts. Public users should access these objects
# through Glimmung, not raw Blob URLs.

resource "azurerm_storage_account" "artifacts" {
  name                            = "romaineglimmungartifacts"
  resource_group_name             = azurerm_resource_group.glimmung.name
  location                        = azurerm_resource_group.glimmung.location
  account_tier                    = "Standard"
  account_replication_type        = "LRS"
  account_kind                    = "StorageV2"
  allow_nested_items_to_be_public = false
  shared_access_key_enabled       = false
  min_tls_version                 = "TLS1_2"

  blob_properties {
    delete_retention_policy {
      days = 30
    }
    container_delete_retention_policy {
      days = 30
    }
  }
}

resource "azurerm_storage_container" "artifacts" {
  name                  = "artifacts"
  storage_account_id    = azurerm_storage_account.artifacts.id
  container_access_type = "private"
}

resource "azurerm_role_assignment" "glimmung_artifacts_blob_contributor" {
  scope                = azurerm_storage_container.artifacts.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = azurerm_user_assigned_identity.glimmung.principal_id
}

resource "azurerm_role_assignment" "native_runner_artifacts_blob_contributor" {
  scope                = azurerm_storage_container.artifacts.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = azurerm_user_assigned_identity.native_runner.principal_id
}

output "glimmung_artifacts_storage_account" {
  value       = azurerm_storage_account.artifacts.name
  description = "Private storage account for Glimmung native-runner logs, evidence, issues, and Reports."
}

output "glimmung_artifacts_container" {
  value       = azurerm_storage_container.artifacts.name
  description = "Private blob container for Glimmung artifacts. Use logical prefixes such as runs/, issues/, and reports/."
}
