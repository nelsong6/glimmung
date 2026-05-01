# Cosmos DB NoSQL Database. Containers: projects, hosts, workflows, leases, runs.
# Created here at the control plane; the runtime pod (workload identity) only
# needs data-plane perms which are already granted on infra-shared-identity at
# the account scope (infra-bootstrap/tofu/cosmos-serverless.tf line 45).

resource "azurerm_cosmosdb_sql_database" "glimmung" {
  name                = "glimmung"
  resource_group_name = local.infra.resource_group_name
  account_name        = data.azurerm_cosmosdb_account.infra.name
  lifecycle {
    ignore_changes = [throughput]
  }
}

resource "azurerm_cosmosdb_sql_container" "projects" {
  name                = "projects"
  resource_group_name = local.infra.resource_group_name
  account_name        = data.azurerm_cosmosdb_account.infra.name
  database_name       = azurerm_cosmosdb_sql_database.glimmung.name
  partition_key_paths = ["/name"]

  indexing_policy {
    indexing_mode = "consistent"
    included_path {
      path = "/*"
    }
  }
}

resource "azurerm_cosmosdb_sql_container" "hosts" {
  name                = "hosts"
  resource_group_name = local.infra.resource_group_name
  account_name        = data.azurerm_cosmosdb_account.infra.name
  database_name       = azurerm_cosmosdb_sql_database.glimmung.name
  partition_key_paths = ["/name"]

  indexing_policy {
    indexing_mode = "consistent"
    included_path {
      path = "/*"
    }
  }
}

resource "azurerm_cosmosdb_sql_container" "workflows" {
  name                = "workflows"
  resource_group_name = local.infra.resource_group_name
  account_name        = data.azurerm_cosmosdb_account.infra.name
  database_name       = azurerm_cosmosdb_sql_database.glimmung.name
  partition_key_paths = ["/project"]

  indexing_policy {
    indexing_mode = "consistent"
    included_path {
      path = "/*"
    }
  }
}

resource "azurerm_cosmosdb_sql_container" "leases" {
  name                = "leases"
  resource_group_name = local.infra.resource_group_name
  account_name        = data.azurerm_cosmosdb_account.infra.name
  database_name       = azurerm_cosmosdb_sql_database.glimmung.name
  partition_key_paths = ["/project"]

  indexing_policy {
    indexing_mode = "consistent"
    included_path {
      path = "/*"
    }
  }
}

# Verify-loop run state (#18). One doc per (project, issue_number);
# attempts accumulate inside the doc so the decision engine can read
# the full attempt history in a single Cosmos point-read. Same
# partition strategy as leases (`/project`) — an in-flight run is
# always project-scoped, per-project queries stay single-partition.
resource "azurerm_cosmosdb_sql_container" "runs" {
  name                = "runs"
  resource_group_name = local.infra.resource_group_name
  account_name        = data.azurerm_cosmosdb_account.infra.name
  database_name       = azurerm_cosmosdb_sql_database.glimmung.name
  partition_key_paths = ["/project"]

  indexing_policy {
    indexing_mode = "consistent"
    included_path {
      path = "/*"
    }
  }
}
