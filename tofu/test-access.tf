# ============================================================================
# Cosmos data-plane RBAC for live-mode tests (#37)
# ============================================================================
# `GLIMMUNG_TEST_COSMOS=live` (tests/cosmos_fake.py) routes the suite at the
# real Cosmos containers; auth flows through DefaultAzureCredential, so the
# az-login session principal must hold a data-plane role on the glimmung
# database. Without it the live smoke fails before the first read with
# `readMetadata` denied — the failure that left #37 open after PR #120
# landed the opt-in mechanics.
#
# Mirrors `glimmung_cosmos` in identity.tf: same role (Built-in Data
# Contributor — tests create+replace items), same database scope (the
# `test-...:` partition-key namespace lives inside the glimmung database
# and never touches sibling apps' data on the same account).
#
# `dev_test_principal_ids` is empty by default so untargeted applies are
# no-ops. Populate per-developer as people opt into local live testing —
# look up your object id with:
#
#   az ad signed-in-user show --query id -o tsv          # for org users
#   az ad user show --id <upn> --query id -o tsv         # for guest MSAs
# ============================================================================

variable "dev_test_principal_ids" {
  description = "Entra object ids of developer accounts that need Cosmos data-plane access for live-mode tests (GLIMMUNG_TEST_COSMOS=live). Scope is the glimmung database only."
  type        = list(string)
  default     = []
}

resource "azurerm_cosmosdb_sql_role_assignment" "dev_test_access" {
  for_each = toset(var.dev_test_principal_ids)

  resource_group_name = local.infra.resource_group_name
  account_name        = data.azurerm_cosmosdb_account.infra.name
  role_definition_id  = "${data.azurerm_cosmosdb_account.infra.id}/sqlRoleDefinitions/00000000-0000-0000-0000-000000000002"
  principal_id        = each.value
  scope               = "${data.azurerm_cosmosdb_account.infra.id}/dbs/${azurerm_cosmosdb_sql_database.glimmung.name}"
}
