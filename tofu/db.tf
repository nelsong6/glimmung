# Cosmos DB NoSQL Database. Containers: projects, hosts, workflows, leases, runs, locks, signals, issues, prs.
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

# Glimmung-native PRs (#41). Mirrors the Issue substrate (#28): glimmung
# is the source of truth for PR conversation (title/body/state plus reviews
# and comments), GitHub is one syndication target. Lets the read path
# (`/v1/prs/detail`) render entirely from Cosmos with no live-GH stitch,
# and gives the iteration-graph viewer (#43) PR-side conversation nodes
# without per-request GH calls. Same partition strategy as `issues` /
# `runs` (`/project`) — listing open PRs for a project stays single-
# partition.
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
