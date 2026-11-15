package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestSecretValueFunctionMetadata(t *testing.T) {
	f := NewSecretValueFunction(&mgclProvider{})

	var resp function.MetadataResponse
	f.Metadata(context.Background(), function.MetadataRequest{}, &resp)

	if resp.Name != "secret_value" {
		t.Fatalf("Name = %q, want secret_value", resp.Name)
	}
}

func TestSecretValueFunctionDefinition(t *testing.T) {
	f := NewSecretValueFunction(&mgclProvider{})

	var resp function.DefinitionResponse
	f.Definition(context.Background(), function.DefinitionRequest{}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	def := resp.Definition

	if len(def.Parameters) != 1 {
		t.Fatalf("parameters = %d, want 1", len(def.Parameters))
	}

	param, ok := def.Parameters[0].(function.StringParameter)
	if !ok {
		t.Fatalf("parameter type = %T, want StringParameter", def.Parameters[0])
	}

	if param.Name != "reference" {
		t.Fatalf("parameter name = %q, want reference", param.Name)
	}

	if param.AllowNullValue || param.AllowUnknownValues {
		t.Fatal("reference must be required (no null or unknown values)")
	}

	if _, ok := def.Return.(function.DynamicReturn); !ok {
		t.Fatalf("return type = %T, want DynamicReturn", def.Return)
	}

	for _, want := range []string{"public", "persist", "map(string)"} {
		if !strings.Contains(def.MarkdownDescription, want) {
			t.Fatalf("definition does not contain %q: %q", want, def.MarkdownDescription)
		}
	}
}

func TestSecretValueFunctionConfigureSharesProviderData(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("bootstrap", map[string]string{"key": "value"}, map[string]bool{"key": false})

	t.Setenv(providerapi.EnvEndpoint, api.url())
	t.Setenv(providerapi.EnvToken, "configured-token")

	p := New("dev")().(*mgclProvider)

	var configureResp provider.ConfigureResponse
	p.Configure(context.Background(), provider.ConfigureRequest{}, &configureResp)

	if configureResp.Diagnostics.HasError() {
		t.Fatalf("unexpected Configure diagnostics: %v", configureResp.Diagnostics.Errors())
	}

	data := p.providerData()
	if data == nil {
		t.Fatal("Configure did not store provider data")
	}

	if data.Endpoint != api.url() || data.Token != "configured-token" {
		t.Fatalf("provider data = %+v, want endpoint and token from environment", data)
	}

	if configureResp.EphemeralResourceData != data {
		t.Fatal("ephemeral resource data and function provider data differ")
	}

	if configureResp.ResourceData != data {
		t.Fatal("managed resource data and function provider data differ")
	}

	resp := runSecretValue(t, NewSecretValueFunction(p), types.StringValue("bootstrap/key"))
	if resp.Error != nil {
		t.Fatalf("function run failed: %s", resp.Error.Text)
	}

	if got := resultString(t, resp); got != "value" {
		t.Fatalf("result = %q, want value", got)
	}
}

func TestSecretValueFunctionReadsPublicValue(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config",
		map[string]string{"region": "us-east-1", "token": "EPHEMERAL-TOKEN"},
		map[string]bool{"region": false, "token": true},
	)

	f := newTestSecretValueFunction(api, "test-token-xyz")

	resp := runSecretValue(t, f, types.StringValue("app-config/region"))
	if resp.Error != nil {
		t.Fatalf("function run failed: %s", resp.Error.Text)
	}

	if got := resultString(t, resp); got != "us-east-1" {
		t.Fatalf("result = %q, want us-east-1", got)
	}

	calls := api.calls()
	if len(calls) != 1 {
		t.Fatalf("API calls = %d, want 1", len(calls))
	}

	call := calls[0]
	if call.method != http.MethodPost || call.path != providerapi.SecretsReadPath {
		t.Fatalf("request = %s %s, want POST %s", call.method, call.path, providerapi.SecretsReadPath)
	}

	if call.authorization != "Bearer test-token-xyz" {
		t.Fatalf("Authorization = %q, want bearer token", call.authorization)
	}

	if call.access != providerapi.SecretAccessPublic {
		t.Fatalf("access = %q, want public", call.access)
	}
}

func TestSecretValueFunctionReadsWildcardMap(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config",
		map[string]string{"region": "us-east-1", "env": "prod", "token": "EPHEMERAL-TOKEN"},
		map[string]bool{"region": false, "env": false, "token": true},
	)

	f := newTestSecretValueFunction(api, "token")

	resp := runSecretValue(t, f, types.StringValue("app-config/*"))
	if resp.Error != nil {
		t.Fatalf("function run failed: %s", resp.Error.Text)
	}

	got := resultMap(t, resp)
	if len(got) != 2 || got["region"] != "us-east-1" || got["env"] != "prod" {
		t.Fatalf("result = %v, want only public KVs", got)
	}

	if _, ok := got["token"]; ok {
		t.Fatalf("wildcard result leaked an ephemeral KV: %v", got)
	}

	if calls := api.calls(); len(calls) != 1 {
		t.Fatalf("API calls = %d, want 1", len(calls))
	}
}

func TestSecretValueFunctionWildcardEmptyMap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
		marks  map[string]bool
		omit   bool
	}{
		{name: "no values", values: map[string]string{}, marks: map[string]bool{}},
		{name: "only ephemeral", values: map[string]string{"a": "one"}, marks: map[string]bool{"a": true}},
		{name: "missing values and marks", omit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeProviderAPI(t)
			api.setSecret("app-config", tc.values, tc.marks)
			api.setOmitValues(tc.omit)
			api.setOmitMarks(tc.omit)

			f := newTestSecretValueFunction(api, "token")

			resp := runSecretValue(t, f, types.StringValue("app-config/*"))
			if resp.Error != nil {
				t.Fatalf("function run failed: %s", resp.Error.Text)
			}

			value := resultValue(t, resp)
			if value.IsNull() || value.IsUnknown() {
				t.Fatalf("result = %v, want a non-nil empty map", value)
			}

			if got := resultMap(t, resp); len(got) != 0 {
				t.Fatalf("result = %v, want an empty map", got)
			}
		})
	}
}

func TestSecretValueFunctionCachesWholePublicSet(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config",
		map[string]string{"a": "one", "b": "two"},
		map[string]bool{"a": false, "b": false},
	)

	f := newTestSecretValueFunction(api, "token")

	for _, reference := range []string{"app-config/a", "app-config/b", "app-config/a", "app-config/*"} {
		resp := runSecretValue(t, f, types.StringValue(reference))
		if resp.Error != nil {
			t.Fatalf("run for %q failed: %s", reference, resp.Error.Text)
		}
	}

	if calls := api.calls(); len(calls) != 1 {
		t.Fatalf("API calls = %d, want exactly 1 for one Secret", len(calls))
	}
}

func TestSecretValueFunctionConcurrentMissesRequestOnce(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config",
		map[string]string{"a": "one", "b": "two"},
		map[string]bool{"a": false, "b": false},
	)

	f := newTestSecretValueFunction(api, "token")

	const goroutines = 24

	var wg sync.WaitGroup

	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			reference := "app-config/a"
			want := "one"

			if i%2 == 1 {
				reference, want = "app-config/b", "two"
			}

			resp := runSecretValueNoTest(f, types.StringValue(reference))
			if resp.Error != nil {
				errs <- fmt.Errorf("run %d: %s", i, resp.Error.Text)
				return
			}

			value, ok := resultAttr(resp).(types.String)
			if !ok || value.ValueString() != want {
				errs <- fmt.Errorf("run %d result = %v, want %q", i, resp.Result.Value(), want)
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	if calls := api.calls(); len(calls) != 1 {
		t.Fatalf("API calls = %d, want 1 for concurrent misses on one Secret", len(calls))
	}
}

func TestSecretValueFunctionDoesNotCacheFailures(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"a": "one"}, map[string]bool{"a": false})
	api.setStatus(http.StatusInternalServerError)
	api.setBody(`{"error":"internal"}`)

	f := newTestSecretValueFunction(api, "token")

	failed := runSecretValue(t, f, types.StringValue("app-config/a"))
	if failed.Error == nil {
		t.Fatal("expected an error for the failed request")
	}

	if calls := api.calls(); len(calls) != 1 {
		t.Fatalf("API calls = %d, want 1 after failure", len(calls))
	}

	api.setStatus(0)
	api.setBody("")

	retried := runSecretValue(t, f, types.StringValue("app-config/a"))
	if retried.Error != nil {
		t.Fatalf("retry failed: %s", retried.Error.Text)
	}

	if got := resultString(t, retried); got != "one" {
		t.Fatalf("retry result = %q, want one", got)
	}

	if calls := api.calls(); len(calls) != 2 {
		t.Fatalf("API calls = %d, want 2 (a failure must not be cached)", len(calls))
	}
}

func TestSecretValueFunctionReadsDifferentSecretsSeparately(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("first", map[string]string{"a": "one"}, map[string]bool{"a": false})
	api.setSecret("second", map[string]string{"a": "two"}, map[string]bool{"a": false})

	f := newTestSecretValueFunction(api, "token")

	for _, reference := range []string{"first/a", "second/a", "first/a"} {
		resp := runSecretValue(t, f, types.StringValue(reference))
		if resp.Error != nil {
			t.Fatalf("run for %q failed: %s", reference, resp.Error.Text)
		}
	}

	calls := api.calls()
	if len(calls) != 2 {
		t.Fatalf("API calls = %d, want 2 (one per distinct Secret)", len(calls))
	}

	if calls[0].body == nil || !strings.Contains(string(calls[0].body), `"first"`) {
		t.Fatalf("first request = %s, want secret first", calls[0].body)
	}

	if calls[1].body == nil || !strings.Contains(string(calls[1].body), `"second"`) {
		t.Fatalf("second request = %s, want secret second", calls[1].body)
	}
}

func TestSecretValueFunctionRejectsMismatchedResponseNameWithoutCaching(t *testing.T) {
	const token = "test-token-xyz"

	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"a": "one"}, map[string]bool{"a": false})
	api.setResponseName(token)

	f := newTestSecretValueFunction(api, token)

	resp := runSecretValue(t, f, types.StringValue("app-config/a"))
	if resp.Error == nil {
		t.Fatal("expected an error for a mismatched response name")
	}

	if !strings.Contains(resp.Error.Text, "Unexpected mgcl secret response") {
		t.Fatalf("error = %q, want unexpected response diagnostic", resp.Error.Text)
	}

	if strings.Contains(resp.Error.Text, "one") {
		t.Fatalf("error leaked a secret value: %q", resp.Error.Text)
	}

	assertNoErrorLeak(t, resp.Error.Text, token, "one")

	api.setResponseName("")

	retried := runSecretValue(t, f, types.StringValue("app-config/a"))
	if retried.Error != nil {
		t.Fatalf("retry failed: %s", retried.Error.Text)
	}

	if calls := api.calls(); len(calls) != 2 {
		t.Fatalf("API calls = %d, want 2 (mismatched responses must not be cached)", len(calls))
	}
}

func TestSecretValueFunctionMissingKey(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config",
		map[string]string{"public": "one", "ephemeral": "two"},
		map[string]bool{"public": false, "ephemeral": true},
	)

	f := newTestSecretValueFunction(api, "token")

	for _, reference := range []string{"app-config/absent", "app-config/ephemeral"} {
		resp := runSecretValue(t, f, types.StringValue(reference))
		if resp.Error == nil {
			t.Fatalf("%s: expected a key-not-found error", reference)
		}

		if !strings.Contains(resp.Error.Text, "was not found among the public KVs") {
			t.Fatalf("%s: error = %q, want public-set key-not-found diagnostic", reference, resp.Error.Text)
		}
	}
}

func TestSecretValueFunctionInvalidReferencesMakeNoRequest(t *testing.T) {
	api := newFakeProviderAPI(t)

	f := newTestSecretValueFunction(api, "token")

	cases := []struct {
		name      string
		reference types.String
		want      string
	}{
		{name: "null", reference: types.StringNull(), want: "Secret reference is required"},
		{name: "unknown", reference: types.StringUnknown(), want: "must be known"},
		{name: "empty", reference: types.StringValue(""), want: "Invalid secret reference"},
		{name: "no slash", reference: types.StringValue("bootstrap"), want: "Invalid secret reference"},
		{name: "two slashes", reference: types.StringValue("a/b/c"), want: "Invalid secret reference"},
		{name: "uppercase name", reference: types.StringValue("Bootstrap/a"), want: "Invalid secret reference"},
		{name: "uppercase key", reference: types.StringValue("bootstrap/Key"), want: "Invalid secret reference"},
		{name: "empty key", reference: types.StringValue("bootstrap/"), want: "Invalid secret reference"},
		{name: "empty name", reference: types.StringValue("/a"), want: "Invalid secret reference"},
		{name: "wildcard name", reference: types.StringValue("*/a"), want: "Invalid secret reference"},
		{name: "partial wildcard", reference: types.StringValue("bootstrap/a*"), want: "Invalid secret reference"},
		{name: "nested wildcard", reference: types.StringValue("bootstrap/*/a"), want: "Invalid secret reference"},
		{name: "too long name", reference: types.StringValue(strings.Repeat("a", 65) + "/key"), want: "Invalid secret reference"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := runSecretValue(t, f, tc.reference)
			if resp.Error == nil {
				t.Fatal("expected an argument error")
			}

			if !strings.Contains(resp.Error.Text, tc.want) {
				t.Fatalf("error = %q, want %q", resp.Error.Text, tc.want)
			}
		})
	}

	if calls := api.calls(); len(calls) != 0 {
		t.Fatalf("API calls = %d, want 0 for invalid references", len(calls))
	}
}

func TestSecretValueFunctionHTTPErrorDiagnostics(t *testing.T) {
	const (
		token = "dont-leak-this-token"
		value = "dont-leak-this-value"
	)

	t.Run("403 server refusal", func(t *testing.T) {
		api := newFakeProviderAPI(t)
		api.setSecret("app-config", map[string]string{"a": value}, map[string]bool{"a": false})
		api.setStatus(http.StatusForbidden)
		api.setBody(`{"error":"secret access denied"}`)

		f := newTestSecretValueFunction(api, token)

		resp := runSecretValue(t, f, types.StringValue("app-config/a"))
		if resp.Error == nil {
			t.Fatal("expected a server error")
		}

		for _, want := range []string{"Failed to read public KVs", "status 403"} {
			if !strings.Contains(resp.Error.Text, want) {
				t.Fatalf("error = %q, want %q", resp.Error.Text, want)
			}
		}

		assertNoErrorLeak(t, resp.Error.Text, token, value)
	})

	t.Run("404 missing secret", func(t *testing.T) {
		api := newFakeProviderAPI(t)
		api.setStatus(http.StatusNotFound)
		api.setBody(`{"error":"not found ` + value + `"}`)

		f := newTestSecretValueFunction(api, token)

		resp := runSecretValue(t, f, types.StringValue("app-config/*"))
		if resp.Error == nil {
			t.Fatal("expected not-found error")
		}

		if !strings.Contains(resp.Error.Text, "public KV set could not be read") {
			t.Fatalf("error = %q, want whole public KV set diagnostic", resp.Error.Text)
		}

		assertNoErrorLeak(t, resp.Error.Text, token, value)
	})

	t.Run("500 server error", func(t *testing.T) {
		api := newFakeProviderAPI(t)
		api.setStatus(http.StatusInternalServerError)
		api.setBody(`{"error":"` + token + ` ` + value + `"}`)

		f := newTestSecretValueFunction(api, token)

		resp := runSecretValue(t, f, types.StringValue("app-config/a"))
		if resp.Error == nil {
			t.Fatal("expected a server error")
		}

		if !strings.Contains(resp.Error.Text, "Failed to read public KVs") {
			t.Fatalf("error = %q, want read failure summary", resp.Error.Text)
		}

		assertNoErrorLeak(t, resp.Error.Text, token, value)
	})
}

// assertPublicMarkContractError verifies a public read that violates the
// per-KV mark contract fails safely and is not cached.
func assertPublicMarkContractError(t *testing.T, api *fakeProviderAPI, reference, want string, values ...string) {
	t.Helper()

	const token = "test-token-xyz"

	f := newTestSecretValueFunction(api, token)

	resp := runSecretValue(t, f, types.StringValue(reference))
	if resp.Error == nil {
		t.Fatal("expected a per-KV mark contract error")
	}

	text := resp.Error.Text
	for _, marker := range []string{"per-KV public mark contract", want} {
		if !strings.Contains(text, marker) {
			t.Fatalf("error = %q, want %q", text, marker)
		}
	}

	assertNoErrorLeak(t, text, token, "")

	for _, value := range values {
		if strings.Contains(text, value) {
			t.Fatalf("error leaked value %q: %q", value, text)
		}
	}

	if calls := api.calls(); len(calls) != 1 {
		t.Fatalf("API calls = %d, want 1", len(calls))
	}

	// Fix the response and retry: the rejected response must not be cached.
	api.clearReadOverrides()

	retried := runSecretValue(t, f, types.StringValue(reference))
	if retried.Error != nil {
		t.Fatalf("retry failed: %s", retried.Error.Text)
	}

	if calls := api.calls(); len(calls) != 2 {
		t.Fatalf("API calls = %d, want 2 (a rejected public read must not be cached)", len(calls))
	}
}

func TestSecretValueFunctionRejectsPublicReadWithoutMarks(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"a": "A-VALUE"}, map[string]bool{"a": false})
	api.setOmitMarks(true)

	assertPublicMarkContractError(t, api, "app-config/a", "without an ephemeral mark", "A-VALUE")
}

func TestSecretValueFunctionRejectsEphemeralMarkInPublicRead(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"a": "A-VALUE"}, map[string]bool{"a": false})
	api.setRawRead(map[string]string{"a": "A-VALUE"}, map[string]bool{"a": true})

	assertPublicMarkContractError(t, api, "app-config/a", "ephemeral-marked key", "A-VALUE")
}

func TestSecretValueFunctionRejectsMixedInvalidPublicRead(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config",
		map[string]string{"a": "A-VALUE", "b": "B-VALUE"},
		map[string]bool{"a": false, "b": false},
	)
	// Key a is valid, key b has no mark: the whole response is rejected.
	api.setRawRead(
		map[string]string{"a": "A-VALUE", "b": "B-VALUE"},
		map[string]bool{"a": false},
	)

	assertPublicMarkContractError(t, api, "app-config/*", `key "b"`, "A-VALUE", "B-VALUE")
}

func TestSecretValueFunctionReadsEnvironmentBeforeConfigure(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"a": "one"}, map[string]bool{"a": false})

	t.Setenv(providerapi.EnvEndpoint, api.url())
	t.Setenv(providerapi.EnvToken, "env-only-token")

	// Simulate OpenTofu evaluating a provider function before ConfigureProvider.
	p := &mgclProvider{}
	f := NewSecretValueFunction(p)

	resp := runSecretValue(t, f, types.StringValue("app-config/a"))
	if resp.Error != nil {
		t.Fatalf("function run failed: %s", resp.Error.Text)
	}

	if got := resultString(t, resp); got != "one" {
		t.Fatalf("result = %q, want one", got)
	}

	// A later Configure must not discard the cache populated before it.
	var configureResp provider.ConfigureResponse
	p.Configure(context.Background(), provider.ConfigureRequest{}, &configureResp)

	if configureResp.Diagnostics.HasError() {
		t.Fatalf("unexpected Configure diagnostics: %v", configureResp.Diagnostics.Errors())
	}

	again := runSecretValue(t, f, types.StringValue("app-config/a"))
	if again.Error != nil {
		t.Fatalf("function run after Configure failed: %s", again.Error.Text)
	}

	if calls := api.calls(); len(calls) != 1 {
		t.Fatalf("API calls = %d, want 1 (Configure must preserve the function cache)", len(calls))
	}
}

func TestSecretValueFunctionRequiresConfiguration(t *testing.T) {
	t.Setenv(providerapi.EnvEndpoint, "")
	t.Setenv(providerapi.EnvToken, "")

	cases := []struct {
		name string
		data *providerData
		want string
	}{
		{name: "no provider data", data: nil, want: providerapi.EnvEndpoint},
		{name: "empty provider data", data: &providerData{}, want: providerapi.EnvEndpoint},
		{name: "no endpoint", data: &providerData{Token: "token"}, want: providerapi.EnvEndpoint},
		{name: "no token", data: &providerData{Endpoint: "http://127.0.0.1:1217"}, want: providerapi.EnvToken},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &mgclProvider{data: tc.data}
			resp := runSecretValue(t, NewSecretValueFunction(p), types.StringValue("bootstrap/a"))
			if resp.Error == nil {
				t.Fatal("expected a configuration error")
			}

			if !strings.Contains(resp.Error.Text, tc.want) {
				t.Fatalf("error = %q, want %s", resp.Error.Text, tc.want)
			}
		})
	}
}

func TestSecretValueFunctionCacheReturnsCopies(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"a": "original"}, map[string]bool{"a": false})

	data := &providerData{Endpoint: api.url(), Token: "token"}

	first, err := data.readPublicValues(context.Background(), "app-config")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}

	first["a"] = "mutated"

	second, err := data.readPublicValues(context.Background(), "app-config")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}

	if second["a"] != "original" {
		t.Fatalf("cached value = %q, want original (caller mutated the cache)", second["a"])
	}

	if calls := api.calls(); len(calls) != 1 {
		t.Fatalf("API calls = %d, want 1", len(calls))
	}
}

func TestSecretEphemeralResourceReturnsPublicAndEphemeralKVs(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("mixed",
		map[string]string{"public_kv": "PUBLIC-VALUE", "ephemeral_kv": "EPHEMERAL-VALUE"},
		map[string]bool{"public_kv": false, "ephemeral_kv": true},
	)

	r := newTestResource(&providerData{Endpoint: api.url(), Token: "test-token-xyz"})
	sch := secretSchema(t)

	resp := openSecret(t, r, secretConfig(t, sch, types.StringValue("mixed")))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	got := mapValue(t, openModel(t, resp).Values)
	if len(got) != 2 || got["public_kv"] != "PUBLIC-VALUE" || got["ephemeral_kv"] != "EPHEMERAL-VALUE" {
		t.Fatalf("values = %v, want all public and ephemeral KVs", got)
	}

	calls := api.calls()
	if len(calls) != 1 {
		t.Fatalf("API calls = %d, want 1", len(calls))
	}

	if calls[0].access != providerapi.SecretAccessEphemeral {
		t.Fatalf("access = %q, want ephemeral", calls[0].access)
	}
}

func TestSecretValueFunctionCacheAndEphemeralOpenAreIndependent(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("mixed",
		map[string]string{"public_kv": "PUBLIC-VALUE", "ephemeral_kv": "EPHEMERAL-VALUE"},
		map[string]bool{"public_kv": false, "ephemeral_kv": true},
	)

	data := &providerData{Endpoint: api.url(), Token: "token"}
	p := &mgclProvider{data: data}
	f := NewSecretValueFunction(p)

	first := runSecretValue(t, f, types.StringValue("mixed/public_kv"))
	if first.Error != nil {
		t.Fatalf("function run failed: %s", first.Error.Text)
	}

	// The ephemeral resource never uses the function cache and must still make
	// its own request.
	r := newTestResource(data)
	sch := secretSchema(t)

	openResp := openSecret(t, r, secretConfig(t, sch, types.StringValue("mixed")))
	if openResp.Diagnostics.HasError() {
		t.Fatalf("unexpected ephemeral diagnostics: %v", openResp.Diagnostics.Errors())
	}

	second := runSecretValue(t, f, types.StringValue("mixed/public_kv"))
	if second.Error != nil {
		t.Fatalf("second function run failed: %s", second.Error.Text)
	}

	calls := api.calls()
	if len(calls) != 2 {
		t.Fatalf("API calls = %d, want 2 (one function read plus one ephemeral open)", len(calls))
	}

	if calls[0].access != providerapi.SecretAccessPublic || calls[1].access != providerapi.SecretAccessEphemeral {
		t.Fatalf("access sequence = %q, %q; want public, ephemeral", calls[0].access, calls[1].access)
	}
}

func newTestSecretValueFunction(api *fakeProviderAPI, token string) function.Function {
	p := &mgclProvider{data: newProviderData(api.url(), token)}
	return NewSecretValueFunction(p)
}

func runSecretValue(t *testing.T, f function.Function, reference types.String) *function.RunResponse {
	t.Helper()

	return runSecretValueNoTest(f, reference)
}

func runSecretValueNoTest(f function.Function, reference types.String) *function.RunResponse {
	resp := &function.RunResponse{
		Result: function.NewResultData(types.DynamicUnknown()),
	}

	f.Run(context.Background(), function.RunRequest{
		Arguments: function.NewArgumentsData([]attr.Value{reference}),
	}, resp)

	return resp
}

// resultAttr unwraps a Dynamic result so callers can assert the concrete type.
func resultAttr(resp *function.RunResponse) attr.Value {
	value := resp.Result.Value()
	if dynamic, ok := value.(types.Dynamic); ok {
		return dynamic.UnderlyingValue()
	}

	return value
}

func resultValue(t *testing.T, resp *function.RunResponse) attr.Value {
	t.Helper()

	value := resultAttr(resp)
	if value == nil {
		t.Fatal("result is nil")
	}

	return value
}

func resultString(t *testing.T, resp *function.RunResponse) string {
	t.Helper()

	value, ok := resultAttr(resp).(types.String)
	if !ok {
		t.Fatalf("result type = %T, want types.String", resultAttr(resp))
	}

	return value.ValueString()
}

func resultMap(t *testing.T, resp *function.RunResponse) map[string]string {
	t.Helper()

	value, ok := resultAttr(resp).(types.Map)
	if !ok {
		t.Fatalf("result type = %T, want types.Map", resultAttr(resp))
	}

	out := map[string]string{}
	if diags := value.ElementsAs(context.Background(), &out, false); diags.HasError() {
		t.Fatalf("decoding map result: %v", diags.Errors())
	}

	return out
}

func assertNoErrorLeak(t *testing.T, text, token, value string) {
	t.Helper()

	for _, marker := range []string{token, value} {
		if marker != "" && strings.Contains(text, marker) {
			t.Fatalf("diagnostic leaked %q: %q", marker, text)
		}
	}
}
