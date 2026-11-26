terraform {
  required_providers {
    mgcl = {
      source = "geektr-cloud/mgcl"
    }
  }
}

# The provider has no HCL arguments. MGCL_PROVIDER_ENDPOINT and
# MGCL_PROVIDER_TOKEN are injected by `mgcl run -- <command>`.
provider "mgcl" {}
