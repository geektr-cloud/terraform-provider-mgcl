# The ephemeral resource reads public and ephemeral KVs during plan and apply.
# OpenTofu keeps these values out of plan and state.
ephemeral "mgcl_secret" "bootstrap" {
  name = "bootstrap"
}
