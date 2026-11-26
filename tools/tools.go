//go:build generate

// Package tools pins the documentation generator. It is excluded from normal
// builds so tfplugindocs and its dependencies never enter the provider go.mod.
package tools

import (
	// Documentation generation from provider schemas and the examples/ tree.
	_ "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs"
)

//go:generate tofu fmt -recursive ../examples/
//go:generate go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate --provider-dir .. --provider-name mgcl
//go:generate go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs validate --provider-dir .. --provider-name mgcl
