package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestSecretValueEphemeralFunctionMetadata(t *testing.T) {
	f := NewSecretValueEphemeralFunction(&mgclProvider{})

	var resp function.MetadataResponse
	f.Metadata(context.Background(), function.MetadataRequest{}, &resp)

	if resp.Name != "i_just_fucking_wanna_read_ephemeral" {
		t.Fatalf("Name = %q, want i_just_fucking_wanna_read_ephemeral", resp.Name)
	}
}

func TestSecretValueEphemeralFunctionDefinition(t *testing.T) {
	f := NewSecretValueEphemeralFunction(&mgclProvider{})

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

	for _, want := range []string{"ephemeral", "persist", "map(string)", "does not give the result OpenTofu ephemeral semantics"} {
		if !strings.Contains(def.MarkdownDescription, want) {
			t.Fatalf("definition does not contain %q: %q", want, def.MarkdownDescription)
		}
	}

	if strings.Contains(def.MarkdownDescription, "every public KV") {
		t.Fatalf("all-KV definition claims to read only public KVs: %q", def.MarkdownDescription)
	}
}

func TestSecretValueEphemeralFunctionReadsEphemeralKV(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config",
		map[string]string{"region": "us-east-1", "token": "EPHEMERAL-TOKEN"},
		map[string]bool{"region": false, "token": true},
	)

	f := newTestSecretValueEphemeralFunction(api, "test-token-xyz")

	resp := runSecretValue(t, f, types.StringValue("app-config/token"))
	if resp.Error != nil {
		t.Fatalf("function run failed: %s", resp.Error.Text)
	}

	if got := resultString(t, resp); got != "EPHEMERAL-TOKEN" {
		t.Fatalf("result = %q, want EPHEMERAL-TOKEN", got)
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

	if call.access != providerapi.SecretAccessEphemeral {
		t.Fatalf("access = %q, want ephemeral", call.access)
	}
}

func TestSecretValueEphemeralFunctionReadsMixedWildcard(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("mixed",
		map[string]string{"public_kv": "PUBLIC-VALUE", "ephemeral_kv": "EPHEMERAL-VALUE"},
		map[string]bool{"public_kv": false, "ephemeral_kv": true},
	)

	f := newTestSecretValueEphemeralFunction(api, "token")

	single := runSecretValue(t, f, types.StringValue("mixed/ephemeral_kv"))
	if single.Error != nil {
		t.Fatalf("single run failed: %s", single.Error.Text)
	}

	if got := resultString(t, single); got != "EPHEMERAL-VALUE" {
		t.Fatalf("single result = %q, want EPHEMERAL-VALUE", got)
	}

	wildcard := runSecretValue(t, f, types.StringValue("mixed/*"))
	if wildcard.Error != nil {
		t.Fatalf("wildcard run failed: %s", wildcard.Error.Text)
	}

	got := resultMap(t, wildcard)
	if len(got) != 2 || got["public_kv"] != "PUBLIC-VALUE" || got["ephemeral_kv"] != "EPHEMERAL-VALUE" {
		t.Fatalf("wildcard result = %v, want every public and ephemeral KV", got)
	}

	if calls := api.calls(); len(calls) != 1 {
		t.Fatalf("API calls = %d, want 1 shared all-KV read", len(calls))
	}
}

func TestSecretValueEphemeralFunctionWildcardSets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
		marks  map[string]bool
		omit   bool
		want   map[string]string
	}{
		{name: "no values", values: map[string]string{}, marks: map[string]bool{}, want: map[string]string{}},
		{name: "only ephemeral", values: map[string]string{"a": "one"}, marks: map[string]bool{"a": true}, want: map[string]string{"a": "one"}},
		{name: "missing values and marks", omit: true, want: map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeProviderAPI(t)
			api.setSecret("app-config", tc.values, tc.marks)
			api.setOmitValues(tc.omit)
			api.setOmitMarks(tc.omit)

			f := newTestSecretValueEphemeralFunction(api, "token")

			resp := runSecretValue(t, f, types.StringValue("app-config/*"))
			if resp.Error != nil {
				t.Fatalf("function run failed: %s", resp.Error.Text)
			}

			value := resultValue(t, resp)
			if value.IsNull() || value.IsUnknown() {
				t.Fatalf("result = %v, want a non-nil map", value)
			}

			got := resultMap(t, resp)
			if len(got) != len(tc.want) {
				t.Fatalf("result = %v, want %v", got, tc.want)
			}

			for key, want := range tc.want {
				if got[key] != want {
					t.Fatalf("result = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSecretValueEphemeralFunctionInvalidReferencesMakeNoRequest(t *testing.T) {
	api := newFakeProviderAPI(t)

	f := newTestSecretValueEphemeralFunction(api, "token")

	cases := []struct {
		name      string
		reference types.String
		want      string
	}{
		{name: "null", reference: types.StringNull(), want: "Secret reference is required"},
		{name: "unknown", reference: types.StringUnknown(), want: "must be known"},
		{name: "empty", reference: types.StringValue(""), want: "Invalid secret reference"},
		{name: "no slash", reference: types.StringValue("bootstrap"), want: "Invalid secret reference"},
		{name: "trailing slash", reference: types.StringValue("a/b/"), want: "Invalid secret reference"},
		{name: "empty segment", reference: types.StringValue("a//b"), want: "Invalid secret reference"},
		{name: "space in name", reference: types.StringValue("Boot strap/a"), want: "Invalid secret reference"},
		{name: "percent in name", reference: types.StringValue("boot%strap/a"), want: "Invalid secret reference"},
		{name: "unicode name", reference: types.StringValue("团队/a"), want: "Invalid secret reference"},
		{name: "space in key", reference: types.StringValue("bootstrap/Bad Key"), want: "Invalid secret reference"},
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

func TestSecretValueEphemeralFunctionMissingKey(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config",
		map[string]string{"public": "one", "ephemeral": "two"},
		map[string]bool{"public": false, "ephemeral": true},
	)

	f := newTestSecretValueEphemeralFunction(api, "token")

	resp := runSecretValue(t, f, types.StringValue("app-config/absent"))
	if resp.Error == nil {
		t.Fatal("expected a key-not-found error")
	}

	if !strings.Contains(resp.Error.Text, `was not found in mgcl secret "app-config"`) {
		t.Fatalf("error = %q, want all-KV key-not-found diagnostic", resp.Error.Text)
	}

	if strings.Contains(resp.Error.Text, "public") {
		t.Fatalf("all-KV diagnostic claims to read only public KVs: %q", resp.Error.Text)
	}
}

func TestSecretValueEphemeralFunctionHTTPErrorDiagnostics(t *testing.T) {
	const (
		token = "dont-leak-this-token"
		value = "dont-leak-this-value"
	)

	t.Run("500 server error", func(t *testing.T) {
		api := newFakeProviderAPI(t)
		api.setSecret("app-config", map[string]string{"a": value}, map[string]bool{"a": true})
		api.setStatus(http.StatusInternalServerError)
		api.setBody(`{"error":"` + token + ` ` + value + `"}`)

		f := newTestSecretValueEphemeralFunction(api, token)

		resp := runSecretValue(t, f, types.StringValue("app-config/a"))
		if resp.Error == nil {
			t.Fatal("expected a server error")
		}

		if !strings.Contains(resp.Error.Text, "Failed to read KVs") {
			t.Fatalf("error = %q, want all-KV read failure summary", resp.Error.Text)
		}

		if strings.Contains(resp.Error.Text, "public") {
			t.Fatalf("all-KV diagnostic claims to read only public KVs: %q", resp.Error.Text)
		}

		assertNoErrorLeak(t, resp.Error.Text, token, value)
	})

	t.Run("404 missing secret", func(t *testing.T) {
		api := newFakeProviderAPI(t)
		api.setStatus(http.StatusNotFound)
		api.setBody(`{"error":"not found ` + value + `"}`)

		f := newTestSecretValueEphemeralFunction(api, token)

		resp := runSecretValue(t, f, types.StringValue("app-config/*"))
		if resp.Error == nil {
			t.Fatal("expected not-found error")
		}

		if !strings.Contains(resp.Error.Text, "KV set could not be read") {
			t.Fatalf("error = %q, want whole KV set diagnostic", resp.Error.Text)
		}

		if strings.Contains(resp.Error.Text, "public") {
			t.Fatalf("all-KV diagnostic claims to read only public KVs: %q", resp.Error.Text)
		}

		assertNoErrorLeak(t, resp.Error.Text, token, value)
	})

	t.Run("name mismatch redacts token", func(t *testing.T) {
		api := newFakeProviderAPI(t)
		api.setSecret("app-config", map[string]string{"a": value}, map[string]bool{"a": true})
		api.setResponseName(token)

		f := newTestSecretValueEphemeralFunction(api, token)

		resp := runSecretValue(t, f, types.StringValue("app-config/a"))
		if resp.Error == nil {
			t.Fatal("expected a mismatched-name error")
		}

		if !strings.Contains(resp.Error.Text, "Unexpected mgcl secret response") {
			t.Fatalf("error = %q, want unexpected response diagnostic", resp.Error.Text)
		}

		assertNoErrorLeak(t, resp.Error.Text, token, value)

		api.setResponseName("")

		retried := runSecretValue(t, f, types.StringValue("app-config/a"))
		if retried.Error != nil {
			t.Fatalf("retry failed: %s", retried.Error.Text)
		}

		if calls := api.calls(); len(calls) != 2 {
			t.Fatalf("API calls = %d, want 2 (mismatched responses must not be cached)", len(calls))
		}
	})
}

func TestSecretValueEphemeralFunctionRejectsMissingMark(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config",
		map[string]string{"a": "A-VALUE", "b": "B-VALUE"},
		map[string]bool{"a": false, "b": true},
	)
	// Key a is valid, key b has no mark: the whole response is rejected.
	api.setRawRead(
		map[string]string{"a": "A-VALUE", "b": "B-VALUE"},
		map[string]bool{"a": false},
	)

	f := newTestSecretValueEphemeralFunction(api, "test-token-xyz")

	resp := runSecretValue(t, f, types.StringValue("app-config/*"))
	if resp.Error == nil {
		t.Fatal("expected a per-KV mark contract error")
	}

	for _, want := range []string{"per-KV mark contract", `key "b"`} {
		if !strings.Contains(resp.Error.Text, want) {
			t.Fatalf("error = %q, want %q", resp.Error.Text, want)
		}
	}

	if strings.Contains(resp.Error.Text, "public") {
		t.Fatalf("all-KV diagnostic claims a public read: %q", resp.Error.Text)
	}

	assertNoErrorLeak(t, resp.Error.Text, "test-token-xyz", "A-VALUE")

	if strings.Contains(resp.Error.Text, "B-VALUE") {
		t.Fatalf("error leaked value %q: %q", "B-VALUE", resp.Error.Text)
	}

	if calls := api.calls(); len(calls) != 1 {
		t.Fatalf("API calls = %d, want 1", len(calls))
	}

	// Fix the response and retry: the rejected response must not be cached.
	api.clearReadOverrides()

	retried := runSecretValue(t, f, types.StringValue("app-config/*"))
	if retried.Error != nil {
		t.Fatalf("retry failed: %s", retried.Error.Text)
	}

	got := resultMap(t, retried)
	if len(got) != 2 || got["a"] != "A-VALUE" || got["b"] != "B-VALUE" {
		t.Fatalf("retry result = %v, want both KVs", got)
	}

	if calls := api.calls(); len(calls) != 2 {
		t.Fatalf("API calls = %d, want 2 (a rejected all-KV read must not be cached)", len(calls))
	}
}

func TestSecretValueEphemeralFunctionAcceptsBothMarks(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("mixed",
		map[string]string{"public_kv": "PUBLIC-VALUE", "ephemeral_kv": "EPHEMERAL-VALUE"},
		map[string]bool{"public_kv": false, "ephemeral_kv": true},
	)

	f := newTestSecretValueEphemeralFunction(api, "token")

	resp := runSecretValue(t, f, types.StringValue("mixed/*"))
	if resp.Error != nil {
		t.Fatalf("function run failed: %s", resp.Error.Text)
	}

	got := resultMap(t, resp)
	if len(got) != 2 {
		t.Fatalf("result = %v, want both marks accepted", got)
	}
}

func TestSecretValueEphemeralFunctionCacheIsolatesPublicReads(t *testing.T) {
	t.Run("public then all then public", func(t *testing.T) {
		api := newFakeProviderAPI(t)
		api.setSecret("mixed",
			map[string]string{"public_kv": "PUBLIC-VALUE", "ephemeral_kv": "EPHEMERAL-VALUE"},
			map[string]bool{"public_kv": false, "ephemeral_kv": true},
		)

		p := &mgclProvider{data: newProviderData(api.url(), "token")}
		publicFunction := NewSecretValueFunction(p)
		allFunction := NewSecretValueEphemeralFunction(p)

		first := runSecretValue(t, publicFunction, types.StringValue("mixed/public_kv"))
		if first.Error != nil {
			t.Fatalf("public run failed: %s", first.Error.Text)
		}

		all := runSecretValue(t, allFunction, types.StringValue("mixed/*"))
		if all.Error != nil {
			t.Fatalf("all run failed: %s", all.Error.Text)
		}

		if got := resultMap(t, all); len(got) != 2 || got["ephemeral_kv"] != "EPHEMERAL-VALUE" {
			t.Fatalf("all result = %v, want public and ephemeral KVs", got)
		}

		second := runSecretValue(t, publicFunction, types.StringValue("mixed/public_kv"))
		if second.Error != nil {
			t.Fatalf("second public run failed: %s", second.Error.Text)
		}

		if got := resultString(t, second); got != "PUBLIC-VALUE" {
			t.Fatalf("second public result = %q, want PUBLIC-VALUE", got)
		}

		calls := api.calls()
		if len(calls) != 2 {
			t.Fatalf("API calls = %d, want 2 (one public read and one all-KV read)", len(calls))
		}

		if calls[0].access != providerapi.SecretAccessPublic || calls[1].access != providerapi.SecretAccessEphemeral {
			t.Fatalf("access sequence = %q, %q; want public, ephemeral", calls[0].access, calls[1].access)
		}
	})

	t.Run("all then public then all", func(t *testing.T) {
		api := newFakeProviderAPI(t)
		api.setSecret("mixed",
			map[string]string{"public_kv": "PUBLIC-VALUE", "ephemeral_kv": "EPHEMERAL-VALUE"},
			map[string]bool{"public_kv": false, "ephemeral_kv": true},
		)

		p := &mgclProvider{data: newProviderData(api.url(), "token")}
		publicFunction := NewSecretValueFunction(p)
		allFunction := NewSecretValueEphemeralFunction(p)

		all := runSecretValue(t, allFunction, types.StringValue("mixed/*"))
		if all.Error != nil {
			t.Fatalf("all run failed: %s", all.Error.Text)
		}

		publicAll := runSecretValue(t, publicFunction, types.StringValue("mixed/*"))
		if publicAll.Error != nil {
			t.Fatalf("public wildcard run failed: %s", publicAll.Error.Text)
		}

		if got := resultMap(t, publicAll); len(got) != 1 || got["public_kv"] != "PUBLIC-VALUE" {
			t.Fatalf("public wildcard result = %v, want only the public KV", got)
		}

		// The all-KV cache must never satisfy a public read with an ephemeral
		// KV.
		publicEphemeral := runSecretValue(t, publicFunction, types.StringValue("mixed/ephemeral_kv"))
		if publicEphemeral.Error == nil {
			t.Fatal("public function read an ephemeral KV from the all-KV cache")
		}

		if !strings.Contains(publicEphemeral.Error.Text, "was not found among the public KVs") {
			t.Fatalf("error = %q, want public-only diagnostic", publicEphemeral.Error.Text)
		}

		again := runSecretValue(t, allFunction, types.StringValue("mixed/*"))
		if again.Error != nil {
			t.Fatalf("second all run failed: %s", again.Error.Text)
		}

		if got := resultMap(t, again); len(got) != 2 {
			t.Fatalf("second all result = %v, want both KVs", got)
		}

		calls := api.calls()
		if len(calls) != 2 {
			t.Fatalf("API calls = %d, want 2 (one all-KV read and one public read)", len(calls))
		}

		if calls[0].access != providerapi.SecretAccessEphemeral || calls[1].access != providerapi.SecretAccessPublic {
			t.Fatalf("access sequence = %q, %q; want ephemeral, public", calls[0].access, calls[1].access)
		}
	})
}

func TestSecretValueEphemeralFunctionCachesWholeSet(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config",
		map[string]string{"a": "one", "b": "two"},
		map[string]bool{"a": false, "b": true},
	)

	f := newTestSecretValueEphemeralFunction(api, "token")

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

func TestSecretValueEphemeralFunctionDoesNotCacheFailures(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"a": "one"}, map[string]bool{"a": true})
	api.setStatus(http.StatusInternalServerError)
	api.setBody(`{"error":"internal"}`)

	f := newTestSecretValueEphemeralFunction(api, "token")

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

func TestSecretValueEphemeralFunctionConcurrentMissesRequestOnce(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config",
		map[string]string{"a": "one", "b": "two"},
		map[string]bool{"a": false, "b": true},
	)

	f := newTestSecretValueEphemeralFunction(api, "token")

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

func TestSecretValueEphemeralFunctionReadsEnvironmentBeforeConfigure(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config", map[string]string{"a": "one"}, map[string]bool{"a": true})

	t.Setenv(providerapi.EnvEndpoint, api.url())
	t.Setenv(providerapi.EnvToken, "env-only-token")

	// Simulate OpenTofu evaluating a provider function before ConfigureProvider.
	p := &mgclProvider{}
	f := NewSecretValueEphemeralFunction(p)

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
		t.Fatalf("API calls = %d, want 1 (Configure must preserve the all-KV cache)", len(calls))
	}
}

func TestSecretValueEphemeralFunctionRequiresConfiguration(t *testing.T) {
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
			resp := runSecretValue(t, NewSecretValueEphemeralFunction(p), types.StringValue("bootstrap/a"))
			if resp.Error == nil {
				t.Fatal("expected a configuration error")
			}

			if !strings.Contains(resp.Error.Text, tc.want) {
				t.Fatalf("error = %q, want %s", resp.Error.Text, tc.want)
			}
		})
	}
}

func TestSecretValueEphemeralCacheReturnsCopies(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setSecret("app-config",
		map[string]string{"a": "original", "b": "ephemeral"},
		map[string]bool{"a": false, "b": true},
	)

	data := &providerData{Endpoint: api.url(), Token: "token"}

	first, err := data.readAllValues(context.Background(), "app-config")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}

	first["a"] = "mutated"

	second, err := data.readAllValues(context.Background(), "app-config")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}

	if second["a"] != "original" || second["b"] != "ephemeral" {
		t.Fatalf("cached values = %v, want original contents", second)
	}

	if calls := api.calls(); len(calls) != 1 {
		t.Fatalf("API calls = %d, want 1", len(calls))
	}
}

func newTestSecretValueEphemeralFunction(api *fakeProviderAPI, token string) function.Function {
	p := &mgclProvider{data: newProviderData(api.url(), token)}
	return NewSecretValueEphemeralFunction(p)
}
