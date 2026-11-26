# Values in mgcl_secret.contents are always persisted in plan and state.
# Sensitive only hides them from CLI output.
resource "mgcl_secret" "app_config" {
  name = "app-config"

  contents = {
    region = {
      value          = "us-east-1"
      ephemeral_mark = false
    }
    api_token = {
      value = "example-token"
    }
  }
}
