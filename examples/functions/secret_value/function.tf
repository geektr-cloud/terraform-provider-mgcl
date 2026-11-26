# The result is ordinary, non-sensitive data: OpenTofu may persist it in plan
# files, state, and logs. Use secret_value only for KVs marked public.
resource "terraform_data" "public_region" {
  input = provider::mgcl::secret_value("app-config/region")
}

resource "terraform_data" "public_config" {
  input = provider::mgcl::secret_value("app-config/*")
}
