# Cosmos DB NoSQL Database. Containers: projects, hosts, workflows, leases,
# runs, run_events, locks, signals, issues, playbooks, reports,
# report_versions, and legacy prs.
# Created here at the control plane; the runtime pod uses glimmung-identity
# with Cosmos data-plane scope limited to this database.

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

# Ordered native-runner event/log stream. One doc per
# `(run_id, attempt_index, job_id, seq)`; partitioned by project so hot log
# reads stay with the run graph.
resource "azurerm_cosmosdb_sql_container" "run_events" {
  name                = "run_events"
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

# Lock primitive (W1 substrate). Generic mutual-exclusion claims keyed
# by (scope, key) — used by per-PR triage serialization (#19), per-issue
# dispatch serialization (#20), signal-drain locks, and any future
# critical-section need. Doc id is deterministic (`f"{scope}::{quote(key)}"`)
# so concurrent claims race through Cosmos's id-uniqueness constraint,
# not through application logic. Partition by `/scope` — low cardinality
# but bounded (~5-10 values), keeps "all PR locks" / "all issue locks"
# diagnostic queries single-partition.
resource "azurerm_cosmosdb_sql_container" "locks" {
  name                = "locks"
  resource_group_name = local.infra.resource_group_name
  account_name        = data.azurerm_cosmosdb_account.infra.name
  database_name       = azurerm_cosmosdb_sql_database.glimmung.name
  partition_key_paths = ["/scope"]

  indexing_policy {
    indexing_mode = "consistent"
    included_path {
      path = "/*"
    }
  }
}

# Signal bus (#19). Webhooks (GH PR review, GH PR/issue comment), the
# glimmung UI (reject button), and future automations enqueue Signals
# here; a background drain loop processes them through the per-target
# lock primitive + decision engine. Partition by `/target_repo` so per-
# repo drain queries stay single-partition; cross-repo diagnostic
# queries are rare and tolerable as cross-partition scans.
resource "azurerm_cosmosdb_sql_container" "signals" {
  name                = "signals"
  resource_group_name = local.infra.resource_group_name
  account_name        = data.azurerm_cosmosdb_account.infra.name
  database_name       = azurerm_cosmosdb_sql_database.glimmung.name
  partition_key_paths = ["/target_repo"]

  indexing_policy {
    indexing_mode = "consistent"
    included_path {
      path = "/*"
    }
  }
}

# Glimmung-native issues (#28). Replaces live-GH polling with a
# first-class issue model: glimmung is the orchestrator / source of
# truth, GitHub is one of N possible syndication targets. A glimmung
# Issue may carry `metadata.github_issue_url` to link out, but it
# exists and is dispatchable whether or not a GH counterpart exists.
# Same partition strategy as workflows / runs (`/project`) — listing
# open issues for a project stays single-partition.
resource "azurerm_cosmosdb_sql_container" "issues" {
  name                = "issues"
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

# Playbooks (#189). Operator-authored batches of issue specs used to
# coordinate staged work. The first slice is storage-only; execution state
# stays in this document when runnable semantics land.
resource "azurerm_cosmosdb_sql_container" "playbooks" {
  name                = "playbooks"
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

# Legacy PR container. Kept only until the one-shot migration into
# `reports`/`report_versions` has run in production; code no longer reads
# this container.
resource "azurerm_cosmosdb_sql_container" "prs" {
  name                = "prs"
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

resource "azurerm_cosmosdb_sql_container" "reports" {
  name                = "reports"
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

resource "azurerm_cosmosdb_sql_container" "report_versions" {
  name                = "report_versions"
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
