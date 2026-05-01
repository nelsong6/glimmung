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
