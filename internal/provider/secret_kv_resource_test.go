package provider

import (
	"context"
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

func resourceSchema(t *testing.T) resourceschema.Schema {
	t.Helper()

	var resp resource.SchemaResponse
	NewSecretKVResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics.Errors())
	}

	return resp.Schema
}

func secretKVAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"key":            types.StringType,
		"value":          types.StringType,
		"ephemeral_mark": types.BoolType,
	}
}

func secretKVObject(t *testing.T, key, value types.String, mark types.Bool) types.Object {
	t.Helper()

	obj, diags := types.ObjectValue(secretKVAttrTypes(), map[string]attr.Value{
		"key":            key,
		"value":          value,
		"ephemeral_mark": mark,
	})
	if diags.HasError() {
		t.Fatalf("building object: %v", diags.Errors())
	}

	return obj
}

func secretKVPlan(t *testing.T, sch resourceschema.Schema, key, value types.String, mark types.Bool) tfsdk.Plan {
	t.Helper()

	raw, err := secretKVObject(t, key, value, mark).ToTerraformValue(context.Background())
	if err != nil {
		t.Fatalf("converting plan to Terraform value: %v", err)
	}

	return tfsdk.Plan{Schema: sch, Raw: raw}
}

func secretKVState(t *testing.T, sch resourceschema.Schema, key, value types.String, mark types.Bool) tfsdk.State {
	t.Helper()

	raw, err := secretKVObject(t, key, value, mark).ToTerraformValue(context.Background())
	if err != nil {
		t.Fatalf("converting state to Terraform value: %v", err)
	}

	return tfsdk.State{Schema: sch, Raw: raw}
}

func newTestSecretKVResource(data *providerData) *secretKVResource {
	r := NewSecretKVResource().(*secretKVResource)
	r.providerData = data
	return r
}

func createSecretKV(t *testing.T, r *secretKVResource, sch resourceschema.Schema, key, value types.String, mark types.Bool) (*resource.CreateResponse, secretKVModel) {
	t.Helper()

	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	r.Create(context.Background(), resource.CreateRequest{
		Plan: secretKVPlan(t, sch, key, value, mark),
	}, resp)

	if resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		return resp, secretKVModel{}
	}

	return resp, decodeSecretKVState(t, resp.State)
}

func decodeSecretKVState(t *testing.T, state tfsdk.State) secretKVModel {
	t.Helper()

	var model secretKVModel
	if diags := state.Get(context.Background(), &model); diags.HasError() {
		t.Fatalf("decoding state: %v", diags.Errors())
	}

	return model
}

func TestSecretKVResourceMetadata(t *testing.T) {
	var resp resource.MetadataResponse
	NewSecretKVResource().Metadata(context.Background(), resource.MetadataRequest{}, &resp)

	if resp.TypeName != "mgcl_secret_kv" {
		t.Fatalf("TypeName = %q, want mgcl_secret_kv", resp.TypeName)
	}
}

func TestSecretKVResourceSchema(t *testing.T) {
	sch := resourceSchema(t)

	if len(sch.Attributes) != 3 {
		t.Fatalf("attributes = %v, want key, value and ephemeral_mark", sch.Attributes)
	}

	key, ok := sch.Attributes["key"].(resourceschema.StringAttribute)
	if !ok {
		t.Fatalf("key type = %T, want StringAttribute", sch.Attributes["key"])
	}

	if !key.Required || key.Optional || key.Computed {
		t.Fatalf("key flags = required:%v optional:%v computed:%v, want required only", key.Required, key.Optional, key.Computed)
	}

	if len(key.PlanModifiers) != 1 {
		t.Fatalf("key plan modifiers = %d, want RequiresReplace", len(key.PlanModifiers))
	}

	var replaceResp planmodifier.StringResponse
	key.PlanModifiers[0].PlanModifyString(context.Background(), planmodifier.StringRequest{
		State:      secretKVState(t, sch, types.StringValue("app-config/old"), types.StringValue("v"), types.BoolValue(true)),
		Plan:       secretKVPlan(t, sch, types.StringValue("app-config/new"), types.StringValue("v"), types.BoolValue(true)),
		StateValue: types.StringValue("app-config/old"),
		PlanValue:  types.StringValue("app-config/new"),
	}, &replaceResp)

	if !replaceResp.RequiresReplace {
		t.Fatal("changing key must require replacement")
	}

	value, ok := sch.Attributes["value"].(resourceschema.StringAttribute)
	if !ok {
		t.Fatalf("value type = %T, want StringAttribute", sch.Attributes["value"])
	}

	if !value.Required || value.Optional || value.Computed {
		t.Fatalf("value flags = required:%v optional:%v computed:%v, want required only", value.Required, value.Optional, value.Computed)
	}

	if !value.Sensitive {
		t.Fatal("value must be sensitive")
	}

	mark, ok := sch.Attributes["ephemeral_mark"].(resourceschema.BoolAttribute)
	if !ok {
		t.Fatalf("ephemeral_mark type = %T, want BoolAttribute", sch.Attributes["ephemeral_mark"])
	}

	if !mark.Optional || !mark.Computed || mark.Required {
		t.Fatalf("ephemeral_mark flags = optional:%v computed:%v required:%v, want optional+computed", mark.Optional, mark.Computed, mark.Required)
	}

	if mark.Default == nil {
		t.Fatal("ephemeral_mark has no default")
	}

	var defaultResp defaults.BoolResponse
	mark.Default.DefaultBool(context.Background(), defaults.BoolRequest{}, &defaultResp)

	if !defaultResp.PlanValue.Equal(types.BoolValue(true)) {
		t.Fatalf("ephemeral_mark default = %v, want true", defaultResp.PlanValue)
	}
}

func TestSecretKVResourceValidateConfig(t *testing.T) {
	sch := resourceSchema(t)
	r := NewSecretKVResource().(resource.ResourceWithValidateConfig)

	cases := []struct {
		name    string
		key     types.String
		wantErr bool
	}{
		{name: "valid", key: types.StringValue("app-config/region")},
		{name: "no slash", key: types.StringValue("not-a-reference"), wantErr: true},
		{name: "wildcard", key: types.StringValue("app-config/*"), wantErr: true},
		{name: "uppercase", key: types.StringValue("App/region"), wantErr: true},
		{name: "null skipped", key: types.StringNull()},
		{name: "unknown skipped", key: types.StringUnknown()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := secretKVObject(t, tc.key, types.StringValue("v"), types.BoolValue(true)).ToTerraformValue(context.Background())
			if err != nil {
				t.Fatalf("building config: %v", err)
			}

			var resp resource.ValidateConfigResponse
			r.ValidateConfig(context.Background(), resource.ValidateConfigRequest{
				Config: tfsdk.Config{Schema: sch, Raw: raw},
			}, &resp)

			if got := resp.Diagnostics.HasError(); got != tc.wantErr {
				t.Fatalf("HasError = %v, want %v (diagnostics: %v)", got, tc.wantErr, resp.Diagnostics.Errors())
			}
		})
	}
}

func TestSecretKVResourceConfigureStoresProviderData(t *testing.T) {
	r := NewSecretKVResource().(*secretKVResource)
	data := newProviderData("http://127.0.0.1:1217", "tok")

	var resp resource.ConfigureResponse
	r.Configure(context.Background(), resource.ConfigureRequest{ProviderData: data}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	if r.providerData != data {
		t.Fatal("Configure did not store the shared provider data")
	}
}

func TestSecretKVResourceConfigureRejectsUnexpectedData(t *testing.T) {
	r := NewSecretKVResource().(*secretKVResource)

	var resp resource.ConfigureResponse
	r.Configure(context.Background(), resource.ConfigureRequest{ProviderData: struct{}{}}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected error for unexpected provider data type")
	}
}

func TestSecretKVResourceCreateWritesKV(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretKVResource(newProviderData(api.url(), "test-token-xyz"))
	sch := resourceSchema(t)

	resp, state := createSecretKV(t, r, sch, types.StringValue("app-config/region"), types.StringValue("us-east-1"), types.BoolValue(false))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	if state.Key.ValueString() != "app-config/region" || state.Value.ValueString() != "us-east-1" || state.EphemeralMark.ValueBool() {
		t.Fatalf("state = %+v, want the planned key, value and mark", state)
	}

	values, marks, ok := api.secretValues("app-config")
	if !ok {
		t.Fatal("Secret was not created")
	}

	if values["region"] != "us-east-1" || marks["region"] != false {
		t.Fatalf("stored value/mark = %q/%v, want us-east-1/false", values["region"], marks["region"])
	}

	calls := api.calls()
	if len(calls) != 1 || calls[0].path != providerapi.SecretKVsWritePath {
		t.Fatalf("calls = %+v, want one write", calls)
	}
}

func TestSecretKVResourceCreateDefaultsNullMarkToEphemeral(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretKVResource(newProviderData(api.url(), "token"))
	sch := resourceSchema(t)

	resp, state := createSecretKV(t, r, sch, types.StringValue("app-config/region"), types.StringValue("us-east-1"), types.BoolNull())
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	if state.EphemeralMark.IsNull() || !state.EphemeralMark.ValueBool() {
		t.Fatalf("state mark = %v, want normalized true", state.EphemeralMark)
	}

	_, marks, ok := api.secretValues("app-config")
	if !ok || !marks["region"] {
		t.Fatalf("mark = %v, want ephemeral true", marks)
	}
}

func TestSecretKVResourceCreateTwoKVInOneSecret(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretKVResource(newProviderData(api.url(), "token"))
	sch := resourceSchema(t)

	for _, tc := range []struct {
		key, value string
		mark       bool
	}{
		{key: "app-config/alpha", value: "one", mark: false},
		{key: "app-config/beta", value: "two", mark: true},
	} {
		resp, _ := createSecretKV(t, r, sch, types.StringValue(tc.key), types.StringValue(tc.value), types.BoolValue(tc.mark))
		if resp.Diagnostics.HasError() {
			t.Fatalf("creating %s: %v", tc.key, resp.Diagnostics.Errors())
		}
	}

	values, marks, ok := api.secretValues("app-config")
	if !ok || len(values) != 2 {
		t.Fatalf("stored values = %v, want two KVs in one Secret", values)
	}

	if values["alpha"] != "one" || values["beta"] != "two" || marks["alpha"] || !marks["beta"] {
		t.Fatalf("values/marks = %v/%v, want alpha public and beta ephemeral", values, marks)
	}
}

func TestSecretKVResourceCreateRejectsInvalidKeyWithoutRequest(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretKVResource(newProviderData(api.url(), "token"))
	sch := resourceSchema(t)

	for _, key := range []string{"app-config", "app-config/*", "a/b/c", "Bootstrap/a", "app-config/Key", ""} {
		resp, _ := createSecretKV(t, r, sch, types.StringValue(key), types.StringValue("v"), types.BoolValue(true))
		if !resp.Diagnostics.HasError() {
			t.Fatalf("key %q: expected an error", key)
		}

		if !strings.Contains(diagnosticsText(&resp.Diagnostics), "Invalid secret KV reference") {
			t.Fatalf("key %q: diagnostics = %q", key, diagnosticsText(&resp.Diagnostics))
		}
	}

	if calls := api.calls(); len(calls) != 0 {
		t.Fatalf("API calls = %d, want 0 for invalid keys", len(calls))
	}
}

func TestSecretKVResourceCreateRejectsMismatchedWriteResponse(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setWriteMark(false)

	r := newTestSecretKVResource(newProviderData(api.url(), "token"))
	sch := resourceSchema(t)

	resp, _ := createSecretKV(t, r, sch, types.StringValue("app-config/region"), types.StringValue("us-east-1"), types.BoolValue(true))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for mismatched write metadata")
	}

	if !strings.Contains(diagnosticsText(&resp.Diagnostics), "Unexpected mgcl secret response") {
		t.Fatalf("diagnostics = %q, want unexpected response", diagnosticsText(&resp.Diagnostics))
	}
}

func TestSecretKVResourceCreateRejectsMismatchedWriteKey(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setWriteKey("other_key")

	r := newTestSecretKVResource(newProviderData(api.url(), "token"))
	sch := resourceSchema(t)

	resp, _ := createSecretKV(t, r, sch, types.StringValue("app-config/region"), types.StringValue("us-east-1"), types.BoolValue(true))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for a mismatched write key")
	}

	if !strings.Contains(diagnosticsText(&resp.Diagnostics), "Unexpected mgcl secret response") {
		t.Fatalf("diagnostics = %q, want unexpected response", diagnosticsText(&resp.Diagnostics))
	}
}

func TestSecretKVResourceCreateErrorDoesNotLeak(t *testing.T) {
	const (
		token = "dont-leak-this-token"
		value = "dont-leak-this-value"
	)

	api := newFakeProviderAPI(t)
	api.setStatus(http.StatusInternalServerError)
	api.setBody(`{"error":"` + token + ` ` + value + `"}`)

	r := newTestSecretKVResource(newProviderData(api.url(), token))
	sch := resourceSchema(t)

	resp, _ := createSecretKV(t, r, sch, types.StringValue("app-config/region"), types.StringValue(value), types.BoolValue(true))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected a write error")
	}

	text := diagnosticsText(&resp.Diagnostics)
	assertNoErrorLeak(t, text, token, value)
}

func TestSecretKVResourceUpdateWritesInPlace(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretKVResource(newProviderData(api.url(), "token"))
	sch := resourceSchema(t)

	created, _ := createSecretKV(t, r, sch, types.StringValue("app-config/region"), types.StringValue("old"), types.BoolValue(true))
	if created.Diagnostics.HasError() {
		t.Fatalf("create failed: %v", created.Diagnostics.Errors())
	}

	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: sch}}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  secretKVPlan(t, sch, types.StringValue("app-config/region"), types.StringValue("new"), types.BoolValue(false)),
		State: secretKVState(t, sch, types.StringValue("app-config/region"), types.StringValue("old"), types.BoolValue(true)),
	}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("update failed: %v", resp.Diagnostics.Errors())
	}

	state := decodeSecretKVState(t, resp.State)
	if state.Value.ValueString() != "new" || state.EphemeralMark.ValueBool() {
		t.Fatalf("state = %+v, want updated value and mark", state)
	}

	values, marks, ok := api.secretValues("app-config")
	if !ok || values["region"] != "new" || marks["region"] != false {
		t.Fatalf("stored value/mark = %q/%v, want new/false", values["region"], marks["region"])
	}
}

func TestSecretKVResourceReadSynchronizesDrift(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretKVResource(newProviderData(api.url(), "token"))
	sch := resourceSchema(t)

	created, _ := createSecretKV(t, r, sch, types.StringValue("app-config/region"), types.StringValue("old"), types.BoolValue(true))
	if created.Diagnostics.HasError() {
		t.Fatalf("create failed: %v", created.Diagnostics.Errors())
	}

	api.setSecret("app-config",
		map[string]string{"region": "drifted"},
		map[string]bool{"region": false},
	)

	resp := &resource.ReadResponse{
		State: secretKVState(t, sch, types.StringValue("app-config/region"), types.StringValue("old"), types.BoolValue(true)),
	}
	r.Read(context.Background(), resource.ReadRequest{State: resp.State}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("read failed: %v", resp.Diagnostics.Errors())
	}

	state := decodeSecretKVState(t, resp.State)
	if state.Value.ValueString() != "drifted" || state.EphemeralMark.ValueBool() {
		t.Fatalf("state = %+v, want drift synchronized to drifted/false", state)
	}
}

func TestSecretKVResourceReadRemovesMissingResource(t *testing.T) {
	api := newFakeProviderAPI(t)
	r := newTestSecretKVResource(newProviderData(api.url(), "token"))
	sch := resourceSchema(t)

	created, _ := createSecretKV(t, r, sch, types.StringValue("app-config/region"), types.StringValue("v"), types.BoolValue(true))
	if created.Diagnostics.HasError() {
		t.Fatalf("create failed: %v", created.Diagnostics.Errors())
	}

	cases := []struct {
		name string
		set  func()
	}{
		{name: "missing key", set: func() {
			api.setSecret("app-config", map[string]string{}, map[string]bool{})
		}},
		{name: "missing secret", set: func() {
			api.mu.Lock()
			delete(api.secrets, "app-config")
			api.mu.Unlock()
		}},
		{name: "404", set: func() {
			api.setStatus(http.StatusNotFound)
			api.setBody(`{"error":"secret not found"}`)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api.setStatus(0)
			api.setBody("")
			api.setSecret("app-config", map[string]string{"region": "v"}, map[string]bool{"region": true})
			tc.set()

			state := secretKVState(t, sch, types.StringValue("app-config/region"), types.StringValue("v"), types.BoolValue(true))
			resp := &resource.ReadResponse{State: state}
			r.Read(context.Background(), resource.ReadRequest{State: state}, resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("read failed: %v", resp.Diagnostics.Errors())
			}

			if !resp.State.Raw.IsNull() {
				t.Fatal("missing resource was not removed from state")
			}
		})
	}
}

func TestSecretKVResourceReadRejectsMissingMark(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"region": "SECRET-VALUE"}, map[string]bool{"region": false})
	api.setOmitMarks(true)

	r := newTestSecretKVResource(newProviderData(api.url(), "token"))
	sch := resourceSchema(t)

	state := secretKVState(t, sch, types.StringValue("app-config/region"), types.StringValue("SECRET-VALUE"), types.BoolValue(false))
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

func TestSecretKVResourceReadRejectsMismatchedResponseName(t *testing.T) {
	const token = "test-token-xyz"

	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"region": "v"}, map[string]bool{"region": false})
	api.setResponseName(token)

	r := newTestSecretKVResource(newProviderData(api.url(), token))
	sch := resourceSchema(t)

	state := secretKVState(t, sch, types.StringValue("app-config/region"), types.StringValue("v"), types.BoolValue(false))
	resp := &resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for a mismatched response name")
	}

	text := diagnosticsText(&resp.Diagnostics)
	if !strings.Contains(text, "Unexpected mgcl secret response") {
		t.Fatalf("diagnostics = %q, want unexpected response", text)
	}

	assertNoErrorLeak(t, text, token, "")
}

func TestSecretKVResourceDeleteCallsAPIIdempotently(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"region": "v"}, map[string]bool{"region": true})

	r := newTestSecretKVResource(newProviderData(api.url(), "token"))
	sch := resourceSchema(t)

	for i := 0; i < 2; i++ {
		resp := &resource.DeleteResponse{}
		r.Delete(context.Background(), resource.DeleteRequest{
			State: secretKVState(t, sch, types.StringValue("app-config/region"), types.StringValue("v"), types.BoolValue(true)),
		}, resp)

		if resp.Diagnostics.HasError() {
			t.Fatalf("delete %d failed: %v", i, resp.Diagnostics.Errors())
		}
	}

	values, _, ok := api.secretValues("app-config")
	if !ok {
		t.Fatal("delete removed the Secret itself; it should leave an empty Secret")
	}

	if len(values) != 0 {
		t.Fatalf("values = %v, want the KV removed", values)
	}

	if calls := api.calls(); len(calls) != 2 || calls[0].path != providerapi.SecretKVsDeletePath {
		t.Fatalf("calls = %+v, want two delete calls", calls)
	}
}

func TestSecretKVResourceDeleteErrorDoesNotLeak(t *testing.T) {
	const (
		token = "dont-leak-this-token"
		value = "dont-leak-this-value"
	)

	api := newFakeProviderAPI(t)
	api.setStatus(http.StatusInternalServerError)
	api.setBody(`{"error":"` + token + ` ` + value + `"}`)

	r := newTestSecretKVResource(newProviderData(api.url(), token))
	sch := resourceSchema(t)

	resp := &resource.DeleteResponse{}
	r.Delete(context.Background(), resource.DeleteRequest{
		State: secretKVState(t, sch, types.StringValue("app-config/region"), types.StringValue(value), types.BoolValue(true)),
	}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected a delete error")
	}

	assertNoErrorLeak(t, diagnosticsText(&resp.Diagnostics), token, value)
}

func TestSecretKVResourceRequiresConfiguration(t *testing.T) {
	api := newFakeProviderAPI(t)
	sch := resourceSchema(t)

	for _, data := range []*providerData{nil, {Token: "token"}, {Endpoint: api.url()}} {
		r := newTestSecretKVResource(data)
		resp, _ := createSecretKV(t, r, sch, types.StringValue("app-config/region"), types.StringValue("v"), types.BoolValue(true))
		if !resp.Diagnostics.HasError() {
			t.Fatalf("data %+v: expected a configuration error", data)
		}
	}

	if calls := api.calls(); len(calls) != 0 {
		t.Fatalf("API calls = %d, want 0 without configuration", len(calls))
	}
}
