# "ephemeral" in this function name does not grant OpenTofu ephemeral
# semantics. The result is ordinary data that OpenTofu may persist in plan
# files, state, and logs, including values read from ephemeral KVs.
resource "terraform_data" "all_token" {
  input = provider::mgcl::i_just_fucking_wanna_read_ephemeral("app-config/token")
}

resource "terraform_data" "all_config" {
  input = provider::mgcl::i_just_fucking_wanna_read_ephemeral("app-config/*")
}
