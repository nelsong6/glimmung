# These OAuth resources remain live but are no longer actively configured by
# this stack. Keep their state addresses present so OpenTofu 1.9 does not plan
# destructive cleanup during unrelated infra applies.
resource "azuread_application" "oauth" {
  display_name = "glimmung-oauth"

  lifecycle {
    prevent_destroy = true
    ignore_changes  = all
  }
}

resource "azuread_application" "oauth_test" {
  display_name = "glimmung-oauth-test"

  lifecycle {
    prevent_destroy = true
    ignore_changes  = all
  }
}

resource "azuread_service_principal" "oauth" {
  client_id = "7047dc95-abb2-43ee-90e4-1997a329c031"

  lifecycle {
    prevent_destroy = true
    ignore_changes  = all
  }
}

resource "azuread_service_principal" "oauth_test" {
  client_id = "2a481f2c-2095-4b46-96e8-3a59fcada4f8"

  lifecycle {
    prevent_destroy = true
    ignore_changes  = all
  }
}
