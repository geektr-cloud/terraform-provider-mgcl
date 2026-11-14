package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	ephemeralschema "github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// secretSchema returns the resource schema under test.
func secretSchema(t *testing.T) ephemeralschema.Schema {
	t.Helper()

	var resp ephemeral.SchemaResponse
	NewSecretEphemeralResource().Schema(context.Background(), ephemeral.SchemaRequest{}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics.Errors())
	}

	return resp.Schema
}

// secretConfig builds a tfsdk.Config for mgcl_secret with the supplied name.
// The computed values attribute is always unknown, as it is during plan.
func secretConfig(t *testing.T, sch ephemeralschema.Schema, name types.String) tfsdk.Config {
	t.Helper()

	attrTypes := map[string]attr.Type{
		"name":   types.StringType,
		"values": types.MapType{ElemType: types.StringType},
	}
	attrValues := map[string]attr.Value{
		"name":   name,
		"values": types.MapUnknown(types.StringType),
	}

	object, diags := types.ObjectValue(attrTypes, attrValues)
	if diags.HasError() {
		t.Fatalf("building config object: %v", diags.Errors())
	}

	raw, err := object.ToTerraformValue(context.Background())
	if err != nil {
		t.Fatalf("converting config to Terraform value: %v", err)
	}

	return tfsdk.Config{Schema: sch, Raw: raw}
}

// openSecret opens the resource with the given config and returns the response.
func openSecret(t *testing.T, r ephemeral.EphemeralResource, config tfsdk.Config) *ephemeral.OpenResponse {
	t.Helper()

	resp := &ephemeral.OpenResponse{
		Result: tfsdk.EphemeralResultData{Schema: config.Schema},
	}

	r.Open(context.Background(), ephemeral.OpenRequest{Config: config}, resp)

	return resp
}

// openModel decodes a successful OpenResponse result.
func openModel(t *testing.T, resp *ephemeral.OpenResponse) secretModel {
	t.Helper()

	var data secretModel
	if diags := resp.Result.Get(context.Background(), &data); diags.HasError() {
		t.Fatalf("decoding result: %v", diags.Errors())
	}

	return data
}

// diagnosticsText concatenates all diagnostic summaries and details.
func diagnosticsText(diags *diag.Diagnostics) string {
	var out string

	for _, err := range diags.Errors() {
		out += err.Summary() + "\n" + err.Detail() + "\n"
	}

	for _, warn := range diags.Warnings() {
		out += warn.Summary() + "\n" + warn.Detail() + "\n"
	}

	return out
}

// newTestResource returns a resource configured with the given provider data.
func newTestResource(data *providerData) *secretEphemeralResource {
	r := NewSecretEphemeralResource().(*secretEphemeralResource)
	r.providerData = data

	return r
}
