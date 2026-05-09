variable "hostname" {
  description = "Public hostname (also the SPA redirect URI)."
  type        = string
  default     = "glimmung.romaine.life"
}

variable "allowed_emails" {
  description = "Email allowlist for admin endpoints. Comma-joined and stored in KV; the app splits + lowercases."
  type        = list(string)
  default = [
    "nelson-devops-project@outlook.com",
    "Brenden.owens39@gmail.com",
    "gantonski@gmail.com",
  ]
}

variable "key_vault_name" {
  type    = string
  default = "romaine-kv"
}

variable "test_redirect_uris" {
  description = "Stable dev/test SPA redirect URIs for glimmung-oauth-test. Entra SPA redirect URIs do not support wildcards."
  type        = list(string)
  default = [
    "https://glimmung.dev.romaine.life/",
    "https://glimmung-slot-1.glimmung.dev.romaine.life/",
    "https://glimmung-slot-2.glimmung.dev.romaine.life/",
    "https://glimmung-slot-3.glimmung.dev.romaine.life/",
    "https://glimmung-slot-4.glimmung.dev.romaine.life/",
    "https://glimmung-slot-5.glimmung.dev.romaine.life/",
  ]
}
