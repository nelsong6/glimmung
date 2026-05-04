# Entra app reg for glimmung's admin endpoints (POST /v1/projects, /v1/hosts).
# Mirrors tank-operator/infra/oauth_app.tf — public SPA + MSAL pattern. Phase 3
# will add a dashboard SPA that consumes the same client; Phase 2 only needs
# the audience for validating CLI-minted tokens
# (`az account get-access-token --resource <client-id>`).

resource "azuread_application" "oauth" {
  display_name = "glimmung-oauth"
  # Personal MSA accounts (e.g. outlook.com) need this; AzureADMyOrg-only apps
  # rejected by the consumer auth flow with `unauthorized_client`. Sign-in is
  # still gated by the backend's ALLOWED_EMAILS allowlist.
  sign_in_audience = "AzureADandPersonalMicrosoftAccount"

  owners = [data.azuread_client_config.current.object_id]

  # v2 access tokens are required when sign_in_audience includes
  # PersonalMicrosoftAccount.
  api {
    requested_access_token_version = 2
  }

  # SPA platform — MSAL.js auth-code-with-PKCE flow, no client secret. The
  # redirect URI is needed even for CLI-only Phase 2 because the dashboard
  # in Phase 3 will use it.
  single_page_application {
    redirect_uris = [
      "https://${var.hostname}/",
    ]
  }

  # Microsoft Graph: User.Read (delegated) for MSAL profile fetch.
  required_resource_access {
    resource_app_id = "00000003-0000-0000-c000-000000000000"

    resource_access {
      id   = "e1fe6dd8-ba31-4d61-89e7-88639da4683d" # User.Read
      type = "Scope"
    }
  }
}

resource "azuread_service_principal" "oauth" {
  client_id = azuread_application.oauth.client_id
}

resource "azurerm_key_vault_secret" "oauth_client_id" {
  name         = "glimmung-oauth-client-id"
  value        = azuread_application.oauth.client_id
  key_vault_id = data.azurerm_key_vault.main.id
}

resource "azuread_application" "oauth_test" {
  display_name     = "glimmung-oauth-test"
  sign_in_audience = "AzureADandPersonalMicrosoftAccount"

  owners = [data.azuread_client_config.current.object_id]

  api {
    requested_access_token_version = 2
  }

  # Entra SPA redirect URIs do not support wildcards. Frontman/live-design
  # environments use a small stable hostname pool under
  # `glimmung.dev.romaine.life` and /v1/config returns this app's client ID
  # for those hosts only.
  single_page_application {
    redirect_uris = var.test_redirect_uris
  }

  required_resource_access {
    resource_app_id = "00000003-0000-0000-c000-000000000000"

    resource_access {
      id   = "e1fe6dd8-ba31-4d61-89e7-88639da4683d" # User.Read
      type = "Scope"
    }
  }
}

resource "azuread_service_principal" "oauth_test" {
  client_id = azuread_application.oauth_test.client_id
}

resource "azurerm_key_vault_secret" "oauth_test_client_id" {
  name         = "glimmung-oauth-test-client-id"
  value        = azuread_application.oauth_test.client_id
  key_vault_id = data.azurerm_key_vault.main.id
}

# Comma-joined list — the app splits on `,` and lowercases on startup.
# KV secrets are flat strings, so this is the simplest stable encoding.
resource "azurerm_key_vault_secret" "oauth_allowed_emails" {
  name         = "glimmung-oauth-allowed-emails"
  value        = join(",", var.allowed_emails)
  key_vault_id = data.azurerm_key_vault.main.id
}
