# The value is always persisted in plan and state. Sensitive only hides it
# from CLI output.
resource "mgcl_secret_kv" "region" {
  key            = "app-config/region"
  value          = "us-east-1"
  ephemeral_mark = false
}
