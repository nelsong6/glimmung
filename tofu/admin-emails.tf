# Glimmung-local admin allowlist. Originally lived in infra-bootstrap's
# `romaine-life-admin-emails` KV secret — a shared list every romaine.life
# app consumed via ExternalSecret. That central list was retired once
# auth.romaine.life became the platform identity service and tank-operator
# moved to gating on the JWT role claim instead.
#
# Glimmung still uses its own MSAL flow + email allowlist (its own
# `tofu/oauth.tf` Entra app reg, separate from auth.romaine.life), so it
# needs a local copy of the list to stay functional. Replace this resource
# with auth.romaine.life delegation when migrating glimmung's auth.

locals {
  admin_emails = [
    "nelson@romaine.life",
    "nelson-devops-project@outlook.com",
    "brenden.owens39@gmail.com",
    "gantonski@gmail.com",
    "menacewwo@gmail.com",
  ]
}

resource "azurerm_key_vault_secret" "admin_emails" {
  name         = "glimmung-admin-emails"
  key_vault_id = data.azurerm_key_vault.main.id
  value        = join(",", local.admin_emails)
  content_type = "text/csv"
}
