package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	resourceschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/defaults"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func managedSecretSchema(t *testing.T) resourceschema.Schema {
	t.Helper()

	var resp resource.SchemaResponse
	NewSecretResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics.Errors())
	}

	return resp.Schema
}

func managedSecretAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":     types.StringType,
		"contents": types.MapType{ElemType: secretContentObjectType},
	}
}

func contentsValue(t *testing.T, entries map[string]secretContentModel) types.Map {
	t.Helper()

	if entries == nil {
		entries = map[string]secretContentModel{}
	}

	value, diags := types.MapValueFrom(context.Background(), secretContentObjectType, entries)
	if diags.HasError() {
		t.Fatalf("building contents map: %v", diags.Errors())
	}

	return value
}

func entry(value types.String, mark types.Bool) secretContentModel {
	return secretContentModel{Value: value, EphemeralMark: mark}
}

func managedSecretObject(t *testing.T, name types.String, contents types.Map) types.Object {
	t.Helper()

	obj, diags := types.ObjectValue(managedSecretAttrTypes(), map[string]attr.Value{
		"name":     name,
		"contents": contents,
	})
	if diags.HasError() {
		t.Fatalf("building object: %v", diags.Errors())
	}

	return obj
}

func managedSecretPlan(t *testing.T, sch resourceschema.Schema, name types.String, contents types.Map) tfsdk.Plan {
	t.Helper()

	raw, err := managedSecretObject(t, name, contents).ToTerraformValue(context.Background())
	if err != nil {
		t.Fatalf("converting plan to Terraform value: %v", err)
	}

	return tfsdk.Plan{Schema: sch, Raw: raw}
}

func managedSecretState(t *testing.T, sch resourceschema.Schema, name types.String, contents types.Map) tfsdk.State {
	t.Helper()

	raw, err := managedSecretObject(t, name, contents).ToTerraformValue(context.Background())
	if err != nil {
		t.Fatalf("converting state to Terraform value: %v", err)
	}

	return tfsdk.State{Schema: sch, Raw: raw}
}

func newTestSecretResource(data *providerData) *secretResource {
	r := NewSecretResource().(*secretResource)
	r.providerData = data

	return r
}

func createManagedSecret(
	t *testing.T,
	r *secretResource,
	sch resourceschema.Schema,
	name types.String,
	contents types.Map,
) (*resource.CreateResponse, managedSecretModel) {
	t.Helper()

	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	r.Create(context.Background(), resource.CreateRequest{
		Plan: managedSecretPlan(t, sch, name, contents),
	}, resp)

	if resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		return resp, managedSecretModel{}
	}

	return resp, decodeManagedSecretState(t, resp.State)
}

func decodeManagedSecretState(t *testing.T, state tfsdk.State) managedSecretModel {
	t.Helper()

	var model managedSecretModel
	if diags := state.Get(context.Background(), &model); diags.HasError() {
		t.Fatalf("decoding state: %v", diags.Errors())
	}

	return model
}

func decodeManagedContents(t *testing.T, contents types.Map) map[string]secretContentModel {
	t.Helper()

	entries := map[string]secretContentModel{}
	if diags := contents.ElementsAs(context.Background(), &entries, false); diags.HasError() {
		t.Fatalf("decoding contents: %v", diags.Errors())
	}

	return entries
}

func TestSecretResourceMetadata(t *testing.T) {
	var resp resource.MetadataResponse
	NewSecretResource().Metadata(context.Background(), resource.MetadataRequest{}, &resp)

	if resp.TypeName != "mgcl_secret" {
		t.Fatalf("TypeName = %q, want mgcl_secret", resp.TypeName)
	}
}

func TestSecretResourceSchema(t *testing.T) {
	sch := managedSecretSchema(t)

	if len(sch.Attributes) != 2 {
		t.Fatalf("attributes = %v, want name and contents", sch.Attributes)
	}

	name, ok := sch.Attributes["name"].(resourceschema.StringAttribute)
	if !ok {
		t.Fatalf("name type = %T, want StringAttribute", sch.Attributes["name"])
	}

	if !name.Required || name.Optional || name.Computed {
		t.Fatalf("name flags = required:%v optional:%v computed:%v, want required only", name.Required, name.Optional, name.Computed)
	}

	if len(name.PlanModifiers) != 1 {
		t.Fatalf("name plan modifiers = %d, want RequiresReplace", len(name.PlanModifiers))
	}

	contents, ok := sch.Attributes["contents"].(resourceschema.MapNestedAttribute)
	if !ok {
		t.Fatalf("contents type = %T, want MapNestedAttribute", sch.Attributes["contents"])
	}

	if !contents.Required || contents.Optional || contents.Computed {
		t.Fatalf("contents flags = required:%v optional:%v computed:%v, want required only", contents.Required, contents.Optional, contents.Computed)
	}

	value, ok := contents.NestedObject.Attributes["value"].(resourceschema.StringAttribute)
	if !ok {
		t.Fatalf("value type = %T, want StringAttribute", contents.NestedObject.Attributes["value"])
	}

	if !value.Required || value.Optional || value.Computed {
		t.Fatalf("value flags = required:%v optional:%v computed:%v, want required only", value.Required, value.Optional, value.Computed)
	}

	if !value.Sensitive {
		t.Fatal("value attribute must be sensitive")
	}

	mark, ok := contents.NestedObject.Attributes["ephemeral_mark"].(resourceschema.BoolAttribute)
	if !ok {
		t.Fatalf("ephemeral_mark type = %T, want BoolAttribute", contents.NestedObject.Attributes["ephemeral_mark"])
	}

	if !mark.Optional || !mark.Computed || mark.Required {
		t.Fatalf("ephemeral_mark flags = optional:%v computed:%v required:%v, want optional and computed", mark.Optional, mark.Computed, mark.Required)
	}

	if mark.Default == nil {
		t.Fatal("ephemeral_mark has no default; want true")
	}

	var defaultResp defaults.BoolResponse
	mark.Default.DefaultBool(context.Background(), defaults.BoolRequest{}, &defaultResp)

	if defaultResp.PlanValue.IsNull() || !defaultResp.PlanValue.ValueBool() {
		t.Fatalf("ephemeral_mark default = %v, want true", defaultResp.PlanValue)
	}
}

func TestSecretResourceNameRequiresReplace(t *testing.T) {
	sch := managedSecretSchema(t)
	name := sch.Attributes["name"].(resourceschema.StringAttribute)

	var resp planmodifier.StringResponse
	name.PlanModifiers[0].PlanModifyString(context.Background(), planmodifier.StringRequest{
		State:      managedSecretState(t, sch, types.StringValue("team/app"), contentsValue(t, map[string]secretContentModel{})),
		Plan:       managedSecretPlan(t, sch, types.StringValue("team/other"), contentsValue(t, map[string]secretContentModel{})),
		StateValue: types.StringValue("team/app"),
		PlanValue:  types.StringValue("team/other"),
	}, &resp)

	if !resp.RequiresReplace {
		t.Fatal("changing name must require replacement")
	}
}

func TestSecretResourceValidateConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		nameAttr types.String
		contents types.Map
		wantErr  string
	}{
		{
			name:     "valid hierarchical name and mixed marks",
			nameAttr: types.StringValue("aws-605592676484-cn/scarborough"),
			contents: contentsValue(t, map[string]secretContentModel{
				"account_id": entry(types.StringValue("1234"), types.BoolValue(false)),
				"secret_key": entry(types.StringValue("s"), types.BoolNull()),
			}),
		},
		{
			name:     "deep hierarchical name",
			nameAttr: types.StringValue("a/b/c"),
			contents: contentsValue(t, map[string]secretContentModel{"key": entry(types.StringValue("v"), types.BoolValue(true))}),
		},
		{
			name:     "dotted name and mixed-case keys",
			nameAttr: types.StringValue("net.sxxfuture.infra"),
			contents: contentsValue(t, map[string]secretContentModel{
				"Region.Name": entry(types.StringValue("us-east-1"), types.BoolValue(false)),
				"API-Key":     entry(types.StringValue("v"), types.BoolValue(true)),
			}),
		},
		{
			name:     "uppercase dotted name",
			nameAttr: types.StringValue("Net.SxxFuture.Infra"),
			contents: contentsValue(t, map[string]secretContentModel{"Key": entry(types.StringValue("v"), types.BoolValue(true))}),
		},
		{
			name:     "leading punctuation name",
			nameAttr: types.StringValue("_team/-app"),
			contents: contentsValue(t, map[string]secretContentModel{"key": entry(types.StringValue("v"), types.BoolValue(true))}),
		},
		{
			name:     "dot and dotdot segments",
			nameAttr: types.StringValue("a/./../b"),
			contents: contentsValue(t, nil),
		},
		{
			name:     "empty contents",
			nameAttr: types.StringValue("app-config"),
			contents: contentsValue(t, nil),
		},
		{
			name:     "empty value allowed",
			nameAttr: types.StringValue("app-config"),
			contents: contentsValue(t, map[string]secretContentModel{"key": entry(types.StringValue(""), types.BoolValue(true))}),
		},
		{
			name:     "unknown mark skipped",
			nameAttr: types.StringValue("app-config"),
			contents: contentsValue(t, map[string]secretContentModel{"key": entry(types.StringValue("v"), types.BoolUnknown())}),
		},
		{
			name:     "unknown value skipped",
			nameAttr: types.StringValue("app-config"),
			contents: contentsValue(t, map[string]secretContentModel{"key": entry(types.StringUnknown(), types.BoolValue(true))}),
		},
		{
			name:     "unknown contents skipped",
			nameAttr: types.StringValue("app-config"),
			contents: types.MapUnknown(secretContentObjectType),
		},
		{
			name:     "name with space",
			nameAttr: types.StringValue("Team app"),
			contents: contentsValue(t, nil),
			wantErr:  "Invalid secret name",
		},
		{
			name:     "name with percent",
			nameAttr: types.StringValue("team%app"),
			contents: contentsValue(t, nil),
			wantErr:  "Invalid secret name",
		},
		{
			name:     "name with empty segment",
			nameAttr: types.StringValue("team//app"),
			contents: contentsValue(t, nil),
			wantErr:  "Invalid secret name",
		},
		{
			name:     "name with wildcard segment",
			nameAttr: types.StringValue("team/*"),
			contents: contentsValue(t, nil),
			wantErr:  "Invalid secret name",
		},
		{
			name:     "name with unicode",
			nameAttr: types.StringValue("团队"),
			contents: contentsValue(t, nil),
			wantErr:  "Invalid secret name",
		},
		{
			name:     "name too long",
			nameAttr: types.StringValue(strings.Repeat("a", 63) + "/b"),
			contents: contentsValue(t, nil),
			wantErr:  "Invalid secret name",
		},
		{
			name:     "contents key with slash",
			nameAttr: types.StringValue("app-config"),
			contents: contentsValue(t, map[string]secretContentModel{"a/b": entry(types.StringValue("v"), types.BoolValue(true))}),
			wantErr:  "Invalid secret key",
		},
		{
			name:     "contents key empty",
			nameAttr: types.StringValue("app-config"),
			contents: contentsValue(t, map[string]secretContentModel{"": entry(types.StringValue("v"), types.BoolValue(true))}),
			wantErr:  "Invalid secret key",
		},
		{
			name:     "contents key with space",
			nameAttr: types.StringValue("app-config"),
			contents: contentsValue(t, map[string]secretContentModel{"bad key": entry(types.StringValue("v"), types.BoolValue(true))}),
			wantErr:  "Invalid secret key",
		},
		{
			name:     "null value",
			nameAttr: types.StringValue("app-config"),
			contents: contentsValue(t, map[string]secretContentModel{"key": entry(types.StringNull(), types.BoolValue(true))}),
			wantErr:  "Secret value must not be null",
		},
	}

	sch := managedSecretSchema(t)
	r := NewSecretResource().(resource.ResourceWithValidateConfig)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, err := managedSecretObject(t, tc.nameAttr, tc.contents).ToTerraformValue(context.Background())
			if err != nil {
				t.Fatalf("building config: %v", err)
			}

			var resp resource.ValidateConfigResponse
			r.ValidateConfig(context.Background(), resource.ValidateConfigRequest{
				Config: tfsdk.Config{Schema: sch, Raw: raw},
			}, &resp)

			if tc.wantErr == "" {
				if resp.Diagnostics.HasError() {
					t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
				}

				return
			}

			if !resp.Diagnostics.HasError() {
				t.Fatalf("expected %q, got no error", tc.wantErr)
			}

			if text := diagnosticsText(&resp.Diagnostics); !strings.Contains(text, tc.wantErr) {
				t.Fatalf("diagnostics = %q, want %q", text, tc.wantErr)
			}
		})
	}
}

func TestSecretResourceCreateWritesOneAtomicBatch(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	resp, state := createManagedSecret(t, r, sch, types.StringValue("aws-605592676484-cn/scarborough"),
		contentsValue(t, map[string]secretContentModel{
			"account_id": entry(types.StringValue("1234"), types.BoolValue(false)),
			"secret_key": entry(types.StringValue("s3cr3t"), types.BoolValue(true)),
		}))

	if resp.Diagnostics.HasError() {
		t.Fatalf("create failed: %v", resp.Diagnostics.Errors())
	}

	values, marks, ok := api.secretValues("aws-605592676484-cn/scarborough")
	if !ok || len(values) != 2 {
		t.Fatalf("stored values = %v, want two KVs under the hierarchical name", values)
	}

	if values["account_id"] != "1234" || marks["account_id"] != false {
		t.Fatalf("account_id = %q/%v, want 1234/public", values["account_id"], marks["account_id"])
	}

	if values["secret_key"] != "s3cr3t" || marks["secret_key"] != true {
		t.Fatalf("secret_key = %q/%v, want s3cr3t/ephemeral", values["secret_key"], marks["secret_key"])
	}

	calls := api.calls()
	if len(calls) != 1 || calls[0].path != providerapi.SecretKVsWriteBatchPath {
		t.Fatalf("calls = %+v, want one write-batch request", calls)
	}

	var payload providerapi.WriteSecretKVsRequest
	if err := json.Unmarshal(calls[0].body, &payload); err != nil {
		t.Fatalf("decoding request: %v", err)
	}

	if payload.Name != "aws-605592676484-cn/scarborough" || !payload.Ensure {
		t.Fatalf("request = %+v, want hierarchical name and ensure", payload)
	}

	if len(payload.KVs) != 2 || len(payload.DeleteKeys) != 0 {
		t.Fatalf("request = %+v, want two upserts and no deletes", payload)
	}

	entries := decodeManagedContents(t, state.Contents)
	if got := entries["account_id"].EphemeralMark; got.IsNull() || got.ValueBool() {
		t.Fatalf("state account_id mark = %v, want false", got)
	}

	if got := entries["secret_key"].EphemeralMark; got.IsNull() || !got.ValueBool() {
		t.Fatalf("state secret_key mark = %v, want true", got)
	}
}

func TestSecretResourceCreatePathEscapeStableNames(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	resp, _ := createManagedSecret(t, r, sch, types.StringValue("net.sxxfuture.infra"),
		contentsValue(t, map[string]secretContentModel{
			"Region.Name": entry(types.StringValue("us-east-1"), types.BoolValue(false)),
			"API-Key":     entry(types.StringValue("s3cr3t"), types.BoolValue(true)),
		}))

	if resp.Diagnostics.HasError() {
		t.Fatalf("create failed: %v", resp.Diagnostics.Errors())
	}

	values, marks, ok := api.secretValues("net.sxxfuture.infra")
	if !ok || len(values) != 2 {
		t.Fatalf("stored values = %v, want two mixed-case dotted KVs", values)
	}

	if values["Region.Name"] != "us-east-1" || marks["Region.Name"] != false {
		t.Fatalf("Region.Name = %q/%v, want us-east-1/public", values["Region.Name"], marks["Region.Name"])
	}

	if values["API-Key"] != "s3cr3t" || marks["API-Key"] != true {
		t.Fatalf("API-Key = %q/%v, want s3cr3t/ephemeral", values["API-Key"], marks["API-Key"])
	}
}

func TestSecretResourceCreateEmptyMapEnsuresSecret(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	resp, state := createManagedSecret(t, r, sch, types.StringValue("empty/secret"), contentsValue(t, nil))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create failed: %v", resp.Diagnostics.Errors())
	}

	if _, _, ok := api.secretValues("empty/secret"); !ok {
		t.Fatal("empty contents did not ensure the Secret")
	}

	if state.Contents.IsNull() {
		t.Fatal("state contents must be an empty map, not null")
	}

	if len(decodeManagedContents(t, state.Contents)) != 0 {
		t.Fatal("state contents should be empty")
	}

	calls := api.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want one write-batch request", len(calls))
	}

	var payload providerapi.WriteSecretKVsRequest
	if err := json.Unmarshal(calls[0].body, &payload); err != nil {
		t.Fatalf("decoding request: %v", err)
	}

	if !payload.Ensure || len(payload.KVs) != 0 {
		t.Fatalf("request = %+v, want ensure with zero KVs", payload)
	}
}

func TestSecretResourceCreateRejectsUnknownAndNullWithoutRequest(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	cases := []struct {
		name     string
		nameAttr types.String
		contents types.Map
		want     string
	}{
		{
			name:     "null contents",
			nameAttr: types.StringValue("app-config"),
			contents: types.MapNull(secretContentObjectType),
			want:     "Secret contents are required",
		},
		{
			name:     "unknown contents",
			nameAttr: types.StringValue("app-config"),
			contents: types.MapUnknown(secretContentObjectType),
			want:     "Secret contents must be known",
		},
		{
			name:     "null value",
			nameAttr: types.StringValue("app-config"),
			contents: contentsValue(t, map[string]secretContentModel{"key": entry(types.StringNull(), types.BoolValue(true))}),
			want:     "Secret value must not be null",
		},
		{
			name:     "unknown value",
			nameAttr: types.StringValue("app-config"),
			contents: contentsValue(t, map[string]secretContentModel{"key": entry(types.StringUnknown(), types.BoolValue(true))}),
			want:     "Secret value must be known",
		},
		{
			name:     "unknown name",
			nameAttr: types.StringUnknown(),
			contents: contentsValue(t, nil),
			want:     "Secret name must be known",
		},
		{
			name:     "invalid name",
			nameAttr: types.StringValue("App config"),
			contents: contentsValue(t, nil),
			want:     "Invalid secret name",
		},
		{
			name:     "key with slash",
			nameAttr: types.StringValue("app-config"),
			contents: contentsValue(t, map[string]secretContentModel{"a/b": entry(types.StringValue("v"), types.BoolValue(true))}),
			want:     "Invalid secret key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := createManagedSecret(t, r, sch, tc.nameAttr, tc.contents)
			if !resp.Diagnostics.HasError() {
				t.Fatalf("expected %q, got no error", tc.want)
			}

			if text := diagnosticsText(&resp.Diagnostics); !strings.Contains(text, tc.want) {
				t.Fatalf("diagnostics = %q, want %q", text, tc.want)
			}
		})
	}

	if calls := api.calls(); len(calls) != 0 {
		t.Fatalf("API calls = %d, want 0", len(calls))
	}
}

func TestSecretResourceCreateAllowsEmptyValue(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	resp, state := createManagedSecret(t, r, sch, types.StringValue("app-config"),
		contentsValue(t, map[string]secretContentModel{"empty": entry(types.StringValue(""), types.BoolValue(true))}))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create failed: %v", resp.Diagnostics.Errors())
	}

	if got := decodeManagedContents(t, state.Contents)["empty"].Value; got.ValueString() != "" || got.IsNull() {
		t.Fatalf("state value = %v, want an empty string", got)
	}

	values, _, ok := api.secretValues("app-config")
	if !ok || values["empty"] != "" {
		t.Fatalf("stored value = %q, want an empty string", values["empty"])
	}
}

func TestSecretResourceUpdateAppliesUpsertsAndDeletes(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{
		"kept":    "old",
		"removed": "gone",
		"extra":   "outside-resource",
	}, map[string]bool{"kept": true, "removed": true, "extra": true})

	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	state := managedSecretState(t, sch, types.StringValue("app-config"),
		contentsValue(t, map[string]secretContentModel{
			"kept":    entry(types.StringValue("old"), types.BoolValue(true)),
			"removed": entry(types.StringValue("gone"), types.BoolValue(true)),
		}))

	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: sch}}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan: managedSecretPlan(t, sch, types.StringValue("app-config"),
			contentsValue(t, map[string]secretContentModel{
				"kept":  entry(types.StringValue("new"), types.BoolValue(false)),
				"added": entry(types.StringValue("fresh"), types.BoolValue(true)),
			})),
		State: state,
	}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("update failed: %v", resp.Diagnostics.Errors())
	}

	values, marks, ok := api.secretValues("app-config")
	if !ok {
		t.Fatal("Secret disappeared after update")
	}

	if values["kept"] != "new" || marks["kept"] != false {
		t.Fatalf("kept = %q/%v, want new/public", values["kept"], marks["kept"])
	}

	if values["added"] != "fresh" || marks["added"] != true {
		t.Fatalf("added = %q/%v, want fresh/ephemeral", values["added"], marks["added"])
	}

	if _, exists := values["removed"]; exists {
		t.Fatalf("removed key still exists: %v", values)
	}

	if values["extra"] != "outside-resource" {
		t.Fatalf("extra key was touched: %v", values)
	}

	calls := api.calls()
	if len(calls) != 1 || calls[0].path != providerapi.SecretKVsWriteBatchPath {
		t.Fatalf("calls = %+v, want one write-batch request", calls)
	}

	var payload providerapi.WriteSecretKVsRequest
	if err := json.Unmarshal(calls[0].body, &payload); err != nil {
		t.Fatalf("decoding request: %v", err)
	}

	if len(payload.DeleteKeys) != 1 || payload.DeleteKeys[0] != "removed" {
		t.Fatalf("delete_keys = %v, want [removed]", payload.DeleteKeys)
	}

	if len(payload.KVs) != 2 {
		t.Fatalf("kvs = %+v, want two upserts", payload.KVs)
	}

	updated := decodeManagedContents(t, decodeManagedSecretState(t, resp.State).Contents)
	if len(updated) != 2 || updated["kept"].Value.ValueString() != "new" || updated["added"].Value.ValueString() != "fresh" {
		t.Fatalf("state contents = %+v, want kept and added", updated)
	}
}

func TestSecretResourceWriteResponseMustMatch(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeProviderAPI)
	}{
		{name: "mark mismatch", setup: func(api *fakeProviderAPI) { api.setBatchMark("key", false) }},
		{name: "key mismatch", setup: func(api *fakeProviderAPI) { api.setBatchKey("key", "other") }},
		{name: "missing key", setup: func(api *fakeProviderAPI) { api.setBatchDropKey("key") }},
		{name: "name mismatch", setup: func(api *fakeProviderAPI) { api.setBatchResponseName("other") }},
	}

	sch := managedSecretSchema(t)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeProviderAPI(t)
			tc.setup(api)

			r := newTestSecretResource(newProviderData(api.url(), "token"))

			resp, _ := createManagedSecret(t, r, sch, types.StringValue("app-config"),
				contentsValue(t, map[string]secretContentModel{"key": entry(types.StringValue("v"), types.BoolValue(true))}))
			if !resp.Diagnostics.HasError() {
				t.Fatal("expected a response validation error")
			}

			if text := diagnosticsText(&resp.Diagnostics); !strings.Contains(text, "Unexpected mgcl secret response") {
				t.Fatalf("diagnostics = %q, want unexpected response", text)
			}
		})
	}
}

func TestSecretResourceReadSynchronizesDrift(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{
		"alpha": "drifted",
		"extra": "outside-resource",
	}, map[string]bool{"alpha": false, "extra": true})

	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	state := managedSecretState(t, sch, types.StringValue("app-config"),
		contentsValue(t, map[string]secretContentModel{
			"alpha": entry(types.StringValue("old"), types.BoolValue(true)),
			"beta":  entry(types.StringValue("gone"), types.BoolValue(true)),
		}))

	resp := &resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("read failed: %v", resp.Diagnostics.Errors())
	}

	if resp.State.Raw.IsNull() {
		t.Fatal("read removed the resource; only missing managed KVs should be dropped")
	}

	entries := decodeManagedContents(t, decodeManagedSecretState(t, resp.State).Contents)

	if len(entries) != 1 {
		t.Fatalf("state contents = %+v, want only alpha", entries)
	}

	if got := entries["alpha"]; got.Value.ValueString() != "drifted" || got.EphemeralMark.ValueBool() {
		t.Fatalf("alpha = %+v, want drifted/public", got)
	}

	if _, exists := entries["extra"]; exists {
		t.Fatal("read adopted an extra remote KV")
	}
}

func TestSecretResourceReadDropsAllMissingManagedKeysWithoutRemovingResource(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"extra": "outside"}, map[string]bool{"extra": true})

	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	state := managedSecretState(t, sch, types.StringValue("app-config"),
		contentsValue(t, map[string]secretContentModel{"alpha": entry(types.StringValue("gone"), types.BoolValue(true))}))

	resp := &resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("read failed: %v", resp.Diagnostics.Errors())
	}

	if resp.State.Raw.IsNull() {
		t.Fatal("read removed the resource even though the Secret still exists")
	}

	if got := decodeManagedContents(t, decodeManagedSecretState(t, resp.State).Contents); len(got) != 0 {
		t.Fatalf("state contents = %+v, want an empty map", got)
	}
}

func TestSecretResourceReadRemovesMissingSecret(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	state := managedSecretState(t, sch, types.StringValue("missing"),
		contentsValue(t, map[string]secretContentModel{"alpha": entry(types.StringValue("v"), types.BoolValue(true))}))

	resp := &resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("read failed: %v", resp.Diagnostics.Errors())
	}

	if !resp.State.Raw.IsNull() {
		t.Fatal("a missing Secret must remove the resource from state")
	}
}

func TestSecretResourceReadRejectsMissingMark(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"alpha": "SECRET-VALUE"}, map[string]bool{"alpha": true})
	api.setOmitMarks(true)

	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	state := managedSecretState(t, sch, types.StringValue("app-config"),
		contentsValue(t, map[string]secretContentModel{"alpha": entry(types.StringValue("SECRET-VALUE"), types.BoolValue(true))}))

	resp := &resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error when the ephemeral mark is missing")
	}

	text := diagnosticsText(&resp.Diagnostics)
	if !strings.Contains(text, "Missing ephemeral mark") {
		t.Fatalf("diagnostics = %q, want missing-mark error", text)
	}

	if strings.Contains(text, "SECRET-VALUE") {
		t.Fatalf("diagnostics leaked the value: %q", text)
	}
}

func TestSecretResourceDeleteIsBatchIdempotentAndKeepsSecret(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{
		"alpha": "one",
		"beta":  "two",
		"extra": "outside",
	}, map[string]bool{"alpha": true, "beta": false, "extra": true})

	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	state := managedSecretState(t, sch, types.StringValue("app-config"),
		contentsValue(t, map[string]secretContentModel{
			"alpha": entry(types.StringValue("one"), types.BoolValue(true)),
			"beta":  entry(types.StringValue("two"), types.BoolValue(false)),
		}))

	for i := 0; i < 2; i++ {
		resp := &resource.DeleteResponse{}
		r.Delete(context.Background(), resource.DeleteRequest{State: state}, resp)

		if resp.Diagnostics.HasError() {
			t.Fatalf("delete %d failed: %v", i, resp.Diagnostics.Errors())
		}
	}

	values, _, ok := api.secretValues("app-config")
	if !ok {
		t.Fatal("delete removed the Secret itself")
	}

	if _, exists := values["alpha"]; exists {
		t.Fatal("delete left a managed key behind")
	}

	if _, exists := values["beta"]; exists {
		t.Fatal("delete left a managed key behind")
	}

	if values["extra"] != "outside" {
		t.Fatalf("delete touched an unmanaged key: %v", values)
	}

	calls := api.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want two delete requests", len(calls))
	}

	for _, call := range calls {
		if call.path != providerapi.SecretKVsWriteBatchPath {
			t.Fatalf("call path = %q, want write-batch", call.path)
		}

		var payload providerapi.WriteSecretKVsRequest
		if err := json.Unmarshal(call.body, &payload); err != nil {
			t.Fatalf("decoding request: %v", err)
		}

		if payload.Ensure {
			t.Fatal("delete request must not ensure (create) the Secret")
		}

		if len(payload.KVs) != 0 {
			t.Fatalf("delete request upserts KVs: %+v", payload.KVs)
		}

		if len(payload.DeleteKeys) != 2 {
			t.Fatalf("delete_keys = %v, want two keys", payload.DeleteKeys)
		}
	}
}

func TestSecretResourceDeleteMissingSecretDoesNotCreate(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	state := managedSecretState(t, sch, types.StringValue("missing"),
		contentsValue(t, map[string]secretContentModel{"alpha": entry(types.StringValue("v"), types.BoolValue(true))}))

	resp := &resource.DeleteResponse{}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("delete failed: %v", resp.Diagnostics.Errors())
	}

	if _, _, ok := api.secretValues("missing"); ok {
		t.Fatal("delete created the missing Secret")
	}
}

func TestSecretResourceDeleteEmptyContentsMakesNoRequest(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	state := managedSecretState(t, sch, types.StringValue("app-config"), contentsValue(t, nil))

	resp := &resource.DeleteResponse{}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("delete failed: %v", resp.Diagnostics.Errors())
	}

	if calls := api.calls(); len(calls) != 0 {
		t.Fatalf("API calls = %d, want 0", len(calls))
	}
}

func TestSecretResourceErrorsDoNotLeak(t *testing.T) {
	const (
		token = "dont-leak-this-token"
		value = "dont-leak-this-value"
	)

	api := newFakeProviderAPI(t)
	api.setStatus(http.StatusInternalServerError)
	api.setBody(`{"error":"` + token + ` ` + value + `"}`)

	r := newTestSecretResource(newProviderData(api.url(), token))
	sch := managedSecretSchema(t)

	created, _ := createManagedSecret(t, r, sch, types.StringValue("app-config"),
		contentsValue(t, map[string]secretContentModel{"key": entry(types.StringValue(value), types.BoolValue(true))}))
	if !created.Diagnostics.HasError() {
		t.Fatal("expected a write error")
	}

	assertNoErrorLeak(t, diagnosticsText(&created.Diagnostics), token, value)

	api.setStatus(0)
	api.setBody("")
	api.setSecret("app-config", map[string]string{"key": value}, map[string]bool{"key": true})
	api.setStatus(http.StatusInternalServerError)

	state := managedSecretState(t, sch, types.StringValue("app-config"),
		contentsValue(t, map[string]secretContentModel{"key": entry(types.StringValue(value), types.BoolValue(true))}))

	read := &resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, read)
	if !read.Diagnostics.HasError() {
		t.Fatal("expected a read error")
	}

	assertNoErrorLeak(t, diagnosticsText(&read.Diagnostics), token, value)

	deleted := &resource.DeleteResponse{}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, deleted)
	if !deleted.Diagnostics.HasError() {
		t.Fatal("expected a delete error")
	}

	assertNoErrorLeak(t, diagnosticsText(&deleted.Diagnostics), token, value)
}

func TestSecretResourceRequiresConfiguration(t *testing.T) {
	api := newFakeProviderAPI(t)
	sch := managedSecretSchema(t)

	for _, data := range []*providerData{nil, {Token: "token"}, {Endpoint: api.url()}} {
		r := newTestSecretResource(data)
		resp, _ := createManagedSecret(t, r, sch, types.StringValue("app-config"),
			contentsValue(t, map[string]secretContentModel{"key": entry(types.StringValue("v"), types.BoolValue(true))}))
		if !resp.Diagnostics.HasError() {
			t.Fatalf("data %+v: expected a configuration error", data)
		}
	}

	if calls := api.calls(); len(calls) != 0 {
		t.Fatalf("API calls = %d, want 0 without configuration", len(calls))
	}
}

func TestSecretResourceConfigure(t *testing.T) {
	r := NewSecretResource().(*secretResource)

	data := &providerData{Endpoint: "http://127.0.0.1:1217", Token: "tok"}

	var resp resource.ConfigureResponse
	r.Configure(context.Background(), resource.ConfigureRequest{ProviderData: data}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	if r.providerData != data {
		t.Fatalf("providerData = %+v, want stored provider data", r.providerData)
	}

	var bad resource.ConfigureResponse
	r.Configure(context.Background(), resource.ConfigureRequest{ProviderData: struct{}{}}, &bad)

	if !bad.Diagnostics.HasError() {
		t.Fatal("expected an error for unexpected provider data")
	}
}

func contentsWithRaw(t *testing.T, elements map[string]attr.Value) types.Map {
	t.Helper()

	value, diags := types.MapValue(secretContentObjectType, elements)
	if diags.HasError() {
		t.Fatalf("building raw contents map: %v", diags.Errors())
	}

	return value
}

func TestSecretResourceValidateConfigSkipsNullAndUnknownElements(t *testing.T) {
	sch := managedSecretSchema(t)
	r := NewSecretResource().(resource.ResourceWithValidateConfig)

	cases := map[string]types.Map{
		"null element":    contentsWithRaw(t, map[string]attr.Value{"key": types.ObjectNull(secretContentAttrTypes)}),
		"unknown element": contentsWithRaw(t, map[string]attr.Value{"key": types.ObjectUnknown(secretContentAttrTypes)}),
	}

	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := managedSecretObject(t, types.StringValue("app-config"), contents).ToTerraformValue(context.Background())
			if err != nil {
				t.Fatalf("building config: %v", err)
			}

			var resp resource.ValidateConfigResponse
			r.ValidateConfig(context.Background(), resource.ValidateConfigRequest{
				Config: tfsdk.Config{Schema: sch, Raw: raw},
			}, &resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("ValidateConfig must skip %s and let core report it: %v", name, resp.Diagnostics.Errors())
			}
		})
	}
}

func TestSecretResourceCreateRejectsNullAndUnknownElementsWithoutConversionError(t *testing.T) {
	sch := managedSecretSchema(t)

	cases := map[string]types.Map{
		"null element":    contentsWithRaw(t, map[string]attr.Value{"key": types.ObjectNull(secretContentAttrTypes)}),
		"unknown element": contentsWithRaw(t, map[string]attr.Value{"key": types.ObjectUnknown(secretContentAttrTypes)}),
	}

	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeProviderAPI(t)
			r := newTestSecretResource(newProviderData(api.url(), "token"))

			resp, _ := createManagedSecret(t, r, sch, types.StringValue("app-config"), contents)
			if !resp.Diagnostics.HasError() {
				t.Fatal("expected a safe contents error")
			}

			text := diagnosticsText(&resp.Diagnostics)
			if !strings.Contains(text, "Secret contents must be known") {
				t.Fatalf("diagnostics = %q, want Secret contents must be known", text)
			}

			if strings.Contains(text, "Value Conversion Error") || strings.Contains(text, "provider bug") {
				t.Fatalf("diagnostics leaked a framework conversion error: %q", text)
			}

			if calls := api.calls(); len(calls) != 0 {
				t.Fatalf("API calls = %d, want 0", len(calls))
			}
		})
	}
}

func limitEntries(t *testing.T, count, valueSize int) types.Map {
	t.Helper()

	entries := make(map[string]secretContentModel, count)
	for i := 0; i < count; i++ {
		entries[fmt.Sprintf("key_%02d", i)] = entry(types.StringValue(strings.Repeat("a", valueSize)), types.BoolValue(true))
	}

	return contentsValue(t, entries)
}

func TestSecretResourceValidateConfigRejectsLimits(t *testing.T) {
	sch := managedSecretSchema(t)
	r := NewSecretResource().(resource.ResourceWithValidateConfig)

	cases := []struct {
		name     string
		contents types.Map
		want     string
	}{
		{name: "too many entries", contents: limitEntries(t, 65, 1), want: "Too many Secret contents entries"},
		{name: "value too large", contents: limitEntries(t, 1, (64<<10)+1), want: "Secret value exceeds size limit"},
		{name: "total too large", contents: limitEntries(t, 5, 60<<10), want: "Secret contents exceed size limit"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := managedSecretObject(t, types.StringValue("app-config"), tc.contents).ToTerraformValue(context.Background())
			if err != nil {
				t.Fatalf("building config: %v", err)
			}

			var resp resource.ValidateConfigResponse
			r.ValidateConfig(context.Background(), resource.ValidateConfigRequest{
				Config: tfsdk.Config{Schema: sch, Raw: raw},
			}, &resp)

			if !resp.Diagnostics.HasError() {
				t.Fatalf("expected %q, got no error", tc.want)
			}

			if text := diagnosticsText(&resp.Diagnostics); !strings.Contains(text, tc.want) {
				t.Fatalf("diagnostics = %q, want %q", text, tc.want)
			}
		})
	}
}

func TestSecretResourceValidateConfigAcceptsLimitsAtBoundary(t *testing.T) {
	sch := managedSecretSchema(t)
	r := NewSecretResource().(resource.ResourceWithValidateConfig)

	// 64 entries x 4 KiB = exactly 256 KiB total, each value under 64 KiB.
	contents := limitEntries(t, 64, 4<<10)

	raw, err := managedSecretObject(t, types.StringValue("app-config"), contents).ToTerraformValue(context.Background())
	if err != nil {
		t.Fatalf("building config: %v", err)
	}

	var resp resource.ValidateConfigResponse
	r.ValidateConfig(context.Background(), resource.ValidateConfigRequest{
		Config: tfsdk.Config{Schema: sch, Raw: raw},
	}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("boundary limits should be accepted: %v", resp.Diagnostics.Errors())
	}
}

func TestSecretResourceCreateRejectsLimitsWithoutRequest(t *testing.T) {
	sch := managedSecretSchema(t)

	cases := []struct {
		name     string
		contents types.Map
		want     string
	}{
		{name: "too many entries", contents: limitEntries(t, 65, 1), want: "Too many Secret contents entries"},
		{name: "value too large", contents: limitEntries(t, 1, (64<<10)+1), want: "Secret value exceeds size limit"},
		{name: "total too large", contents: limitEntries(t, 5, 60<<10), want: "Secret contents exceed size limit"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeProviderAPI(t)
			r := newTestSecretResource(newProviderData(api.url(), "token"))

			resp, _ := createManagedSecret(t, r, sch, types.StringValue("app-config"), tc.contents)
			if !resp.Diagnostics.HasError() {
				t.Fatalf("expected %q, got no error", tc.want)
			}

			if text := diagnosticsText(&resp.Diagnostics); !strings.Contains(text, tc.want) {
				t.Fatalf("diagnostics = %q, want %q", text, tc.want)
			}

			if calls := api.calls(); len(calls) != 0 {
				t.Fatalf("API calls = %d, want 0", len(calls))
			}
		})
	}
}

func TestSecretResourceDeleteRejectsNotFound(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setStatus(http.StatusNotFound)
	api.setBody(`{"error":"not found"}`)

	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	state := managedSecretState(t, sch, types.StringValue("app-config"),
		contentsValue(t, map[string]secretContentModel{"key": entry(types.StringValue("v"), types.BoolValue(true))}))

	resp := &resource.DeleteResponse{}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("404 from the batch endpoint must be an error, not an idempotent success")
	}

	text := diagnosticsText(&resp.Diagnostics)
	if !strings.Contains(text, "Failed to delete mgcl secret KVs") {
		t.Fatalf("diagnostics = %q, want delete failure", text)
	}

	if !strings.Contains(text, "secret-kvs:write-batch") {
		t.Fatalf("diagnostics = %q, want the batch/hierarchical-name hint", text)
	}
}

func TestSecretResourceWriteNotFoundSuggestsBatchSupport(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setStatus(http.StatusNotFound)
	api.setBody(`{"error":"not found"}`)

	r := newTestSecretResource(newProviderData(api.url(), "token"))
	sch := managedSecretSchema(t)

	resp, _ := createManagedSecret(t, r, sch, types.StringValue("app-config"),
		contentsValue(t, map[string]secretContentModel{"key": entry(types.StringValue("v"), types.BoolValue(true))}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected a write error")
	}

	text := diagnosticsText(&resp.Diagnostics)
	if !strings.Contains(text, "Failed to write mgcl secret") || !strings.Contains(text, "secret-kvs:write-batch") {
		t.Fatalf("diagnostics = %q, want write failure with batch hint", text)
	}
}
