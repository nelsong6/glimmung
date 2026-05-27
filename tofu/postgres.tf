# ============================================================================
# Azure Database for PostgreSQL — Flexible Server
# ============================================================================
# Glimmung's durable store. Mirrors the tank-operator pattern established in
# nelsong6/tank-operator#466: same SKU shape, same AAD-via-UAMI auth model,
# same break-glass-password-in-KV escape hatch.
#
# Sized for glimmung's workload: B1ms (1 vCore burstable, 2 GiB RAM), single
# AZ, no HA tier. With pgxpool MaxConns=6 across ~3 replicas, worst-case
# concurrent connections sit ~18 — well under B1ms's default max_connections
# of ~50.
#
# pg_cron is preloaded so the app can schedule the `run_events` 7-day TTL job.
# ============================================================================

# Break-glass admin password. The glimmung pod never reads this; it
# authenticates via workload identity (AAD). This is for human ops only —
# connect with
#   psql "host=<fqdn> user=pgadmin dbname=glimmung sslmode=require"
# and PGPASSWORD set to this value, pulled from the ng6-glimmung KV.
resource "random_password" "pg_admin" {
  length      = 32
  special     = true
  min_lower   = 1
  min_upper   = 1
  min_numeric = 1
  min_special = 1
  # The Postgres admin login forbids these characters in passwords.
  override_special = "!#$%&*+-_=?"
}

resource "azurerm_postgresql_flexible_server" "glimmung" {
  name                = "glimmung-pg"
  resource_group_name = azurerm_resource_group.glimmung.name

  # Pinned to westus3 because the subscription's westus2 capacity for
  # Flexible Server is currently restricted (`LocationIsOfferRestricted`)
  # — same constraint tank-operator hit in #466. westus3 is in the same
  # physical area as westus2; latency from AKS in westus2 to this DB is
  # comparable to intra-region and egress cost at glimmung's write volume
  # is sub-dollar. Move to data.azurerm_resource_group.infra.location if
  # the quota request lands (https://aka.ms/postgres-request-quota-increase).
  location = "westus3"

  version    = "16"
  sku_name   = "B_Standard_B1ms"
  storage_mb = 32768
  zone       = "1"

  # Public endpoint, gated by AAD auth at the data plane and the
  # Azure-internal firewall rule below. VNet integration is a later
  # tightening if private-only access becomes a requirement; for now this
  # matches tank-operator's Postgres shape.
  public_network_access_enabled = true

  authentication {
    active_directory_auth_enabled = true
    password_auth_enabled         = true
    tenant_id                     = data.azurerm_client_config.current.tenant_id
  }

  administrator_login    = "pgadmin"
  administrator_password = random_password.pg_admin.result

  backup_retention_days        = 7
  geo_redundant_backup_enabled = false

  lifecycle {
    ignore_changes = [
      # AZ can be reassigned during planned maintenance; don't fight it.
      zone,
    ]
  }
}

# AAD admin — glimmung-identity. Granting the existing UAMI server-admin
# rather than a narrower Postgres role keeps the wiring simple: the same
# identity that already federates from the glimmung pod becomes the DB
# admin, and any schema migration the app runs at startup happens under
# that identity. If we later want non-admin app roles, they get created
# via SQL by this admin.
resource "azurerm_postgresql_flexible_server_active_directory_administrator" "glimmung" {
  server_name         = azurerm_postgresql_flexible_server.glimmung.name
  resource_group_name = azurerm_resource_group.glimmung.name
  tenant_id           = data.azurerm_client_config.current.tenant_id
  object_id           = azurerm_user_assigned_identity.glimmung_dedicated.principal_id
  principal_name      = azurerm_user_assigned_identity.glimmung_dedicated.name
  principal_type      = "ServicePrincipal"
}

resource "azurerm_postgresql_flexible_server_database" "glimmung" {
  name      = "glimmung"
  server_id = azurerm_postgresql_flexible_server.glimmung.id
  collation = "en_US.utf8"
  charset   = "utf8"
}

# Firewall: allow Azure-internal traffic. The 0.0.0.0/0.0.0.0 magic rule
# whitelists traffic from any Azure resource in any subscription,
# gated by AAD auth at the data plane. AKS outbound flows through the
# standard LB and reaches this server as Azure-internal.
resource "azurerm_postgresql_flexible_server_firewall_rule" "allow_azure_internal" {
  name             = "allow-azure-internal"
  server_id        = azurerm_postgresql_flexible_server.glimmung.id
  start_ip_address = "0.0.0.0"
  end_ip_address   = "0.0.0.0"
}

# ----------------------------------------------------------------------------
# pg_cron preload
# ----------------------------------------------------------------------------
# pg_cron needs to be in `shared_preload_libraries` to start its background
# worker, and it must appear in the `azure.extensions` allowlist before
# `CREATE EXTENSION pg_cron;` is permitted. Setting `cron.database_name`
# makes `cron.schedule()` calls from the glimmung database default to
# running there, rather than the postgres maintenance DB where the
# cron extension installs its schema.
#
# These settings allow RunMigrations to create pg_cron in the glimmung
# database and schedule the run_events TTL job.

resource "azurerm_postgresql_flexible_server_configuration" "azure_extensions" {
  name      = "azure.extensions"
  server_id = azurerm_postgresql_flexible_server.glimmung.id
  value     = "PG_CRON"
}

resource "azurerm_postgresql_flexible_server_configuration" "shared_preload_libraries" {
  name      = "shared_preload_libraries"
  server_id = azurerm_postgresql_flexible_server.glimmung.id
  value     = "pg_cron"
}

resource "azurerm_postgresql_flexible_server_configuration" "cron_database_name" {
  name      = "cron.database_name"
  server_id = azurerm_postgresql_flexible_server.glimmung.id
  value     = "glimmung"

  # `shared_preload_libraries` must be applied first (and the resulting
  # server restart must complete) before `cron.database_name` is a valid
  # parameter on this server. Without the explicit dependency, parallel
  # apply order can race.
  depends_on = [
    azurerm_postgresql_flexible_server_configuration.shared_preload_libraries,
  ]
}

# ----------------------------------------------------------------------------
# Key Vault publication
# ----------------------------------------------------------------------------
# Publish to ng6-glimmung (the app-owned KV provisioned in keyvault.tf).
# Matches infra-bootstrap doctrine that app-owned secrets belong in
# Key Vaults provisioned by the app repo. ExternalSecrets in k8s/ then
# materializes these into K8s Secrets for the pod to consume.

resource "azurerm_key_vault_secret" "postgres_host" {
  name         = "glimmung-pg-host"
  value        = azurerm_postgresql_flexible_server.glimmung.fqdn
  key_vault_id = azurerm_key_vault.main.id
}

resource "azurerm_key_vault_secret" "postgres_database" {
  name         = "glimmung-pg-database"
  value        = azurerm_postgresql_flexible_server_database.glimmung.name
  key_vault_id = azurerm_key_vault.main.id
}

resource "azurerm_key_vault_secret" "postgres_admin_password" {
  name         = "glimmung-pg-admin-password"
  value        = random_password.pg_admin.result
  key_vault_id = azurerm_key_vault.main.id
}

# ----------------------------------------------------------------------------
# Outputs
# ----------------------------------------------------------------------------

output "postgres_fqdn" {
  value       = azurerm_postgresql_flexible_server.glimmung.fqdn
  description = "FQDN of the glimmung Postgres Flexible Server."
}

output "postgres_database_name" {
  value       = azurerm_postgresql_flexible_server_database.glimmung.name
  description = "Name of the application database inside the Flexible Server."
}
