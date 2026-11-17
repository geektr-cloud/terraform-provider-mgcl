package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	ephemeralschema "github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestSecretEphemeralResourceMetadata(t *testing.T) {
	var resp ephemeral.MetadataResponse
	NewSecretEphemeralResource().Metadata(context.Background(), ephemeral.MetadataRequest{}, &resp)

	if resp.TypeName != "mgcl_secret" {
		t.Fatalf("TypeName = %q, want %q", resp.TypeName, "mgcl_secret")
	}
}

func TestSecretEphemeralResourceSchema(t *testing.T) {
	sch := secretSchema(t)

	if len(sch.Attributes) != 2 {
		t.Fatalf("attributes = %v, want name and values", sch.Attributes)
	}

	name, ok := sch.Attributes["name"].(ephemeralschema.StringAttribute)
	if !ok {
		t.Fatalf("name attribute type = %T, want StringAttribute", sch.Attributes["name"])
	}

	if !name.Required || name.Optional || name.Computed {
		t.Fatalf("name attribute flags = required:%v optional:%v computed:%v, want required only", name.Required, name.Optional, name.Computed)
	}

	values, ok := sch.Attributes["values"].(ephemeralschema.MapAttribute)
	if !ok {
		t.Fatalf("values attribute type = %T, want MapAttribute", sch.Attributes["values"])
	}

	if !values.Computed || values.Optional || values.Required {
		t.Fatalf("values attribute flags = computed:%v optional:%v required:%v, want computed only", values.Computed, values.Optional, values.Required)
	}

	if !values.Sensitive {
		t.Fatal("values attribute must be sensitive")
	}

	if values.ElementType != types.StringType {
		t.Fatalf("values element type = %v, want string", values.ElementType)
	}
}

func TestSecretEphemeralResourceValidateConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		value   types.String
		wantErr bool
	}{
		{name: "simple", value: types.StringValue("bootstrap")},
		{name: "single char", value: types.StringValue("a")},
		{name: "digits and separators", value: types.StringValue("a1_b-2")},
		{name: "max length", value: types.StringValue(strings.Repeat("a", 64))},
		{name: "uppercase", value: types.StringValue("Bootstrap")},
		{name: "leading dash", value: types.StringValue("-bootstrap")},
		{name: "leading underscore", value: types.StringValue("_bootstrap")},
		{name: "leading dot", value: types.StringValue(".hidden")},
		{name: "dotted name", value: types.StringValue("net.sxxfuture.infra")},
		{name: "mixed case dotted name", value: types.StringValue("Net.SxxFuture.Infra")},
		{name: "path-safe punctuation", value: types.StringValue("a$b&c+d=e:f@g~h")},
		{name: "space", value: types.StringValue("boot strap"), wantErr: true},
		{name: "percent", value: types.StringValue("boot%strap"), wantErr: true},
		{name: "question mark", value: types.StringValue("boot?strap"), wantErr: true},
		{name: "hash", value: types.StringValue("boot#strap"), wantErr: true},
		{name: "unicode", value: types.StringValue("秘密"), wantErr: true},
		{name: "wildcard segment", value: types.StringValue("a/*"), wantErr: true},
		{name: "hierarchical", value: types.StringValue("aws-605592676484-cn/scarborough")},
		{name: "deep hierarchical", value: types.StringValue("a/b/c")},
		{name: "dot segment", value: types.StringValue("a/./b")},
		{name: "dotdot segment", value: types.StringValue("a/../b")},
		{name: "trailing slash", value: types.StringValue("a/"), wantErr: true},
		{name: "empty segment", value: types.StringValue("a//b"), wantErr: true},
		{name: "leading slash", value: types.StringValue("/a"), wantErr: true},
		{name: "total length over 64", value: types.StringValue(strings.Repeat("a", 63) + "/b"), wantErr: true},
		{name: "empty", value: types.StringValue(""), wantErr: true},
		{name: "too long", value: types.StringValue(strings.Repeat("a", 65)), wantErr: true},
		{name: "null skipped", value: types.StringNull()},
		{name: "unknown skipped", value: types.StringUnknown()},
	}

	sch := secretSchema(t)
	r := NewSecretEphemeralResource().(ephemeral.EphemeralResourceWithValidateConfig)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var resp ephemeral.ValidateConfigResponse
			r.ValidateConfig(context.Background(), ephemeral.ValidateConfigRequest{
				Config: secretConfig(t, sch, tc.value),
			}, &resp)

			if got := resp.Diagnostics.HasError(); got != tc.wantErr {
				t.Fatalf("HasError = %v, want %v (diagnostics: %v)", got, tc.wantErr, resp.Diagnostics.Errors())
			}
		})
	}
}

func TestValidateSecretNameAndKeyPathEscapeBoundary(t *testing.T) {
	t.Parallel()

	validNames := []string{
		".",
		"..",
		"-a",
		"_a",
		"A",
		"a1_b-2",
		"net.sxxfuture.infra",
		"a/./../b",
		"$&+=:@~",
		strings.Repeat("a", 64),
	}
	for _, name := range validNames {
		if err := validateSecretName(name); err != nil {
			t.Errorf("validateSecretName(%q) = %v, want nil", name, err)
		}
	}

	invalidNames := []string{
		"",
		"a b",
		"a%b",
		"a?b",
		"a#b",
		"秘密",
		"a;b",
		"a,b",
		"a/*",
		"a//b",
		"a/",
		"/a",
		strings.Repeat("a", 65),
	}
	for _, name := range invalidNames {
		if err := validateSecretName(name); err == nil {
			t.Errorf("validateSecretName(%q) = nil, want an error", name)
		}
	}

	validKeys := []string{
		".",
		"..",
		"-a",
		"_a",
		"A",
		"Region.Name",
		"$&+=:@~",
		strings.Repeat("a", 64),
	}
	for _, key := range validKeys {
		if err := validateSecretKey(key); err != nil {
			t.Errorf("validateSecretKey(%q) = %v, want nil", key, err)
		}
	}

	if got := url.PathEscape("*"); got != "%2A" {
		t.Fatalf("url.PathEscape(\"*\") = %q, want %%2A", got)
	}

	invalidKeys := []string{
		"",
		"*",
		"a/b",
		"a b",
		"a%b",
		"a?b",
		"a#b",
		"秘密",
		"a;b",
		"a,b",
		strings.Repeat("a", 65),
	}
	for _, key := range invalidKeys {
		if err := validateSecretKey(key); err == nil {
			t.Errorf("validateSecretKey(%q) = nil, want an error", key)
		}
	}
}

func TestSecretEphemeralResourceOpenReadsSecret(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setValues(map[string]string{
		"access_key_id":     "AKIAEXAMPLE",
		"secret_access_key": "s3cr3t-value",
	})

	r := newTestResource(&providerData{Endpoint: api.url(), Token: "test-token-xyz"})
	sch := secretSchema(t)

	resp := openSecret(t, r, secretConfig(t, sch, types.StringValue("bootstrap")))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	data := openModel(t, resp)
	if data.Name.ValueString() != "bootstrap" {
		t.Fatalf("name = %q, want bootstrap", data.Name.ValueString())
	}

	got := map[string]string{}
	if diags := data.Values.ElementsAs(context.Background(), &got, false); diags.HasError() {
		t.Fatalf("decoding values: %v", diags.Errors())
	}

	if len(got) != 2 || got["access_key_id"] != "AKIAEXAMPLE" || got["secret_access_key"] != "s3cr3t-value" {
		t.Fatalf("values = %v, want the API response values", got)
	}

	calls := api.calls()
	if len(calls) != 1 {
		t.Fatalf("API calls = %d, want exactly 1 per Open", len(calls))
	}

	call := calls[0]
	if call.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", call.method)
	}

	if call.path != providerapi.SecretsReadPath {
		t.Fatalf("path = %q, want %q", call.path, providerapi.SecretsReadPath)
	}

	if call.authorization != "Bearer test-token-xyz" {
		t.Fatalf("Authorization = %q, want bearer token", call.authorization)
	}

	if !strings.HasPrefix(call.contentType, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", call.contentType)
	}

	var body providerapi.ReadSecretRequest
	if err := json.Unmarshal(call.body, &body); err != nil {
		t.Fatalf("request body %q is not JSON: %v", call.body, err)
	}

	if body.Name != "bootstrap" {
		t.Fatalf("request body name = %q, want bootstrap", body.Name)
	}
}

func TestSecretEphemeralResourceOpenReadsHierarchicalSecret(t *testing.T) {
	const name = "aws-605592676484-cn/scarborough"

	api := newFakeProviderAPI(t)
	api.setSecret(name,
		map[string]string{"account_id": "1234", "secret_key": "s"},
		map[string]bool{"account_id": false, "secret_key": true},
	)

	r := newTestResource(&providerData{Endpoint: api.url(), Token: "test-token-xyz"})
	sch := secretSchema(t)

	resp := openSecret(t, r, secretConfig(t, sch, types.StringValue(name)))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	data := openModel(t, resp)

	got := map[string]string{}
	if diags := data.Values.ElementsAs(context.Background(), &got, false); diags.HasError() {
		t.Fatalf("decoding values: %v", diags.Errors())
	}

	if len(got) != 2 || got["account_id"] != "1234" || got["secret_key"] != "s" {
		t.Fatalf("values = %v, want every KV of the hierarchical Secret", got)
	}
}

func TestSecretEphemeralResourceOpenReturnsLatestValues(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setValues(map[string]string{"key": "first"})

	r := newTestResource(&providerData{Endpoint: api.url(), Token: "token"})
	sch := secretSchema(t)

	first := openModel(t, openSecret(t, r, secretConfig(t, sch, types.StringValue("bootstrap"))))
	if v := mapValue(t, first.Values)["key"]; v != "first" {
		t.Fatalf("first open key = %q, want first", v)
	}

	api.setValues(map[string]string{"key": "second"})

	resp := openSecret(t, r, secretConfig(t, sch, types.StringValue("bootstrap")))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	second := openModel(t, resp)
	if v := mapValue(t, second.Values)["key"]; v != "second" {
		t.Fatalf("second open key = %q, want second (values must not be cached across opens)", v)
	}

	if calls := api.calls(); len(calls) != 2 {
		t.Fatalf("API calls = %d, want 2", len(calls))
	}
}

func TestSecretEphemeralResourceOpenEmptyValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		omit bool
	}{
		{name: "empty object", omit: false},
		{name: "missing field", omit: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := newFakeProviderAPI(t)
			api.setOmitValues(tc.omit)

			r := newTestResource(&providerData{Endpoint: api.url(), Token: "token"})
			sch := secretSchema(t)

			resp := openSecret(t, r, secretConfig(t, sch, types.StringValue("bootstrap")))
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
			}

			data := openModel(t, resp)
			if data.Values.IsNull() {
				t.Fatal("values is null, want an empty non-nil map")
			}

			if data.Values.IsUnknown() {
				t.Fatal("values is unknown, want an empty map")
			}

			if got := mapValue(t, data.Values); len(got) != 0 {
				t.Fatalf("values = %v, want empty map", got)
			}
		})
	}
}

func TestSecretEphemeralResourceOpenHTTPError(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setStatus(http.StatusNotFound)
	api.setBody(`{"error":"secret not found"}`)

	r := newTestResource(&providerData{Endpoint: api.url(), Token: "test-token-xyz"})
	sch := secretSchema(t)

	resp := openSecret(t, r, secretConfig(t, sch, types.StringValue("bootstrap")))

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic for a 404 response")
	}

	text := diagnosticsText(&resp.Diagnostics)
	if !strings.Contains(text, "Failed to read mgcl secret") {
		t.Fatalf("diagnostics %q do not contain the failure summary", text)
	}

	if strings.Contains(text, "test-token-xyz") {
		t.Fatalf("diagnostics leak the provider token: %q", text)
	}

	if strings.Contains(text, "s3cr3t") {
		t.Fatalf("diagnostics leak a secret value: %q", text)
	}
}

func TestSecretEphemeralResourceOpenRedactsTokenFromAPIError(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setStatus(http.StatusUnauthorized)
	// A misbehaving server echoes the credential. The provider diagnostic must
	// still not contain it.
	api.setBody(`{"error":"invalid token test-token-xyz"}`)

	r := newTestResource(&providerData{Endpoint: api.url(), Token: "test-token-xyz"})
	sch := secretSchema(t)

	resp := openSecret(t, r, secretConfig(t, sch, types.StringValue("bootstrap")))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic for a 401 response")
	}

	text := diagnosticsText(&resp.Diagnostics)
	if strings.Contains(text, "test-token-xyz") {
		t.Fatalf("diagnostics leak the provider token: %q", text)
	}
}

func TestSecretEphemeralResourceOpenRequiresConfiguration(t *testing.T) {
	t.Parallel()

	api := newFakeProviderAPI(t)

	cases := []struct {
		name string
		data *providerData
		want string
	}{
		{name: "no provider data", data: nil, want: providerapi.EnvEndpoint},
		{name: "no endpoint", data: &providerData{Token: "token"}, want: providerapi.EnvEndpoint},
		{name: "no token", data: &providerData{Endpoint: api.url()}, want: providerapi.EnvToken},
	}

	sch := secretSchema(t)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := openSecret(t, newTestResource(tc.data), secretConfig(t, sch, types.StringValue("bootstrap")))
			if !resp.Diagnostics.HasError() {
				t.Fatal("expected an error diagnostic for missing configuration")
			}

			text := diagnosticsText(&resp.Diagnostics)
			if !strings.Contains(text, tc.want) {
				t.Fatalf("diagnostics %q do not mention %s", text, tc.want)
			}
		})
	}

	if calls := api.calls(); len(calls) != 0 {
		t.Fatalf("API calls = %d, want 0 when configuration is missing", len(calls))
	}
}

func TestSecretEphemeralResourceOpenRequiresKnownName(t *testing.T) {
	api := newFakeProviderAPI(t)

	r := newTestResource(&providerData{Endpoint: api.url(), Token: "token"})
	sch := secretSchema(t)

	resp := openSecret(t, r, secretConfig(t, sch, types.StringUnknown()))

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic for an unknown name")
	}

	if text := diagnosticsText(&resp.Diagnostics); !strings.Contains(text, "must be known") {
		t.Fatalf("diagnostics %q do not explain that the name must be known", text)
	}

	if calls := api.calls(); len(calls) != 0 {
		t.Fatalf("API calls = %d, want 0 for an unknown name", len(calls))
	}
}

func TestSecretEphemeralResourceOpenRequiresName(t *testing.T) {
	api := newFakeProviderAPI(t)

	r := newTestResource(&providerData{Endpoint: api.url(), Token: "token"})
	sch := secretSchema(t)

	resp := openSecret(t, r, secretConfig(t, sch, types.StringNull()))

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic for a null name")
	}

	text := diagnosticsText(&resp.Diagnostics)
	if !strings.Contains(text, "Secret name is required") {
		t.Fatalf("diagnostics %q do not explain that name is required", text)
	}

	if strings.Contains(text, "must be known") {
		t.Fatalf("null name reported the unknown-name error instead: %q", text)
	}

	if resp.Result.Raw.Type() != nil {
		t.Fatal("a result was set for a null name")
	}

	if calls := api.calls(); len(calls) != 0 {
		t.Fatalf("API calls = %d, want 0 for a null name", len(calls))
	}
}

func TestSecretEphemeralResourceOpenRejectsMismatchedResponseName(t *testing.T) {
	api := newFakeProviderAPI(t)
	api.setResponseName("other-secret")
	api.setValues(map[string]string{"access_key_id": "mismatch-s3cr3t"})

	r := newTestResource(&providerData{Endpoint: api.url(), Token: "test-token-xyz"})
	sch := secretSchema(t)

	resp := openSecret(t, r, secretConfig(t, sch, types.StringValue("bootstrap")))

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic for a mismatched response name")
	}

	text := diagnosticsText(&resp.Diagnostics)
	if !strings.Contains(text, "Unexpected mgcl secret response") {
		t.Fatalf("diagnostics %q do not contain the response error summary", text)
	}

	if strings.Contains(text, "mismatch-s3cr3t") {
		t.Fatalf("diagnostics leak a returned value: %q", text)
	}

	if strings.Contains(text, "test-token-xyz") {
		t.Fatalf("diagnostics leak the provider token: %q", text)
	}

	if resp.Result.Raw.Type() != nil {
		t.Fatal("values from a mismatched response were adopted")
	}
}

func TestSecretEphemeralResourceOpenRejectsInvalidName(t *testing.T) {
	api := newFakeProviderAPI(t)

	r := newTestResource(&providerData{Endpoint: api.url(), Token: "token"})
	sch := secretSchema(t)

	resp := openSecret(t, r, secretConfig(t, sch, types.StringValue("bad name")))

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic for an invalid name")
	}

	if text := diagnosticsText(&resp.Diagnostics); !strings.Contains(text, "Invalid secret name") {
		t.Fatalf("diagnostics %q do not contain the validation summary", text)
	}

	if calls := api.calls(); len(calls) != 0 {
		t.Fatalf("API calls = %d, want 0 for an invalid name", len(calls))
	}
}

func TestSecretEphemeralResourceCloseIsNoop(t *testing.T) {
	resp := &ephemeral.CloseResponse{}
	NewSecretEphemeralResource().(ephemeral.EphemeralResourceWithClose).Close(context.Background(), ephemeral.CloseRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	// Close must not need provider data or talk to the endpoint.
	unconfigured := NewSecretEphemeralResource().(ephemeral.EphemeralResourceWithClose)
	resp = &ephemeral.CloseResponse{}
	unconfigured.Close(context.Background(), ephemeral.CloseRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}
}

func mapValue(t *testing.T, value types.Map) map[string]string {
	t.Helper()

	out := map[string]string{}
	if diags := value.ElementsAs(context.Background(), &out, false); diags.HasError() {
		t.Fatalf("decoding map value: %v", diags.Errors())
	}

	return out
}

// fakeProviderAPI is an httptest-backed implementation of the provider API
// contract used by mgcl. Secrets are keyed by name; each KV carries an
// ephemeral mark (true = ephemeral, false = public).
type fakeProviderAPI struct {
	t *testing.T

	server *httptest.Server

	mu           sync.Mutex
	callsList    []recordedCall
	secrets      map[string]*fakeSecret
	omitValues   bool
	omitMarks    bool
	responseName string
	writeMark    *bool
	writeKey     *string
	rawRead      *fakeReadResponse
	batchName    *string
	batchMarks   map[string]bool
	batchKeys    map[string]string
	batchDrops   map[string]bool
	status       int
	body         string
}

type fakeSecret struct {
	values map[string]string
	marks  map[string]bool
}

// fakeReadResponse overrides the response body of secrets:read, bypassing the
// per-KV mark filtering. Tests use it to emulate a server that violates the
// public read contract.
type fakeReadResponse struct {
	values map[string]string
	marks  map[string]bool
}

type recordedCall struct {
	method        string
	path          string
	authorization string
	contentType   string
	access        string
	body          []byte
}

func newFakeProviderAPI(t *testing.T) *fakeProviderAPI {
	t.Helper()

	api := &fakeProviderAPI{
		t:       t,
		secrets: map[string]*fakeSecret{},
	}
	api.secrets["bootstrap"] = &fakeSecret{
		values: map[string]string{},
		marks:  map[string]bool{},
	}

	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}

		api.mu.Lock()
		var readPayload providerapi.ReadSecretRequest
		_ = json.Unmarshal(body, &readPayload)
		api.callsList = append(api.callsList, recordedCall{
			method:        req.Method,
			path:          req.URL.Path,
			authorization: req.Header.Get("Authorization"),
			contentType:   req.Header.Get("Content-Type"),
			access:        readPayload.Access,
			body:          body,
		})
		api.mu.Unlock()

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")

		if status := api.statusLocked(); status != 0 {
			api.mu.Lock()
			raw := api.body
			api.mu.Unlock()
			w.WriteHeader(status)
			_, _ = io.WriteString(w, raw)

			return
		}

		switch req.URL.Path {
		case providerapi.SecretsReadPath:
			api.handleRead(w, body)
		case providerapi.SecretKVsWritePath:
			api.handleWrite(w, body)
		case providerapi.SecretKVsDeletePath:
			api.handleDelete(w, body)
		case providerapi.SecretKVsWriteBatchPath:
			api.handleWriteBatch(w, body)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
		}
	}))
	t.Cleanup(api.server.Close)

	return api
}

func (a *fakeProviderAPI) handleRead(w http.ResponseWriter, body []byte) {
	var payload providerapi.ReadSecretRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		a.t.Errorf("decoding read request: %v", err)
	}

	access := payload.Access
	if access == "" {
		access = providerapi.SecretAccessEphemeral
	}

	values := map[string]string{}
	marks := map[string]bool{}

	a.mu.Lock()
	secret := a.secrets[payload.Name]
	omitValues := a.omitValues
	omitMarks := a.omitMarks
	responseName := a.responseName
	raw := a.rawRead
	if raw != nil {
		values = cloneStringMap(raw.values)
		marks = cloneBoolMap(raw.marks)
	} else if secret != nil {
		for key, value := range secret.values {
			mark, ok := secret.marks[key]
			if !ok {
				mark = true
			}

			if access == providerapi.SecretAccessPublic && mark {
				continue
			}

			values[key] = value
			marks[key] = mark
		}
	}
	a.mu.Unlock()

	if secret == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"secret not found"}`)

		return
	}

	if responseName == "" {
		responseName = payload.Name
	}

	response := map[string]any{"name": responseName}
	if !omitValues {
		response["values"] = values
	}
	if !omitMarks {
		response["ephemeral_marks"] = marks
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		a.t.Errorf("encoding read response: %v", err)
	}
}

func (a *fakeProviderAPI) handleWrite(w http.ResponseWriter, body []byte) {
	var payload providerapi.WriteSecretKVRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		a.t.Errorf("decoding write request: %v", err)
	}

	name, key, ok := splitFakeReference(payload.Key)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid secret reference"}`)

		return
	}

	a.mu.Lock()
	secret := a.secrets[name]
	if secret == nil {
		secret = &fakeSecret{values: map[string]string{}, marks: map[string]bool{}}
		a.secrets[name] = secret
	}
	mark := true
	if payload.IsEphemeral != nil {
		mark = *payload.IsEphemeral
	}
	if override := a.writeMark; override != nil {
		mark = *override
	}
	responseKey := key
	if override := a.writeKey; override != nil {
		responseKey = *override
	}
	secret.values[key] = payload.Value
	secret.marks[key] = mark
	a.mu.Unlock()

	_ = json.NewEncoder(w).Encode(providerapi.WriteSecretKVResponse{
		Name:        name,
		Key:         responseKey,
		IsEphemeral: mark,
	})
}

func (a *fakeProviderAPI) handleDelete(w http.ResponseWriter, body []byte) {
	var payload providerapi.DeleteSecretKVRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		a.t.Errorf("decoding delete request: %v", err)
	}

	name, key, ok := splitFakeReference(payload.Key)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid secret reference"}`)

		return
	}

	a.mu.Lock()
	secret := a.secrets[name]
	if secret != nil {
		delete(secret.values, key)
		delete(secret.marks, key)
	}
	a.mu.Unlock()

	_ = json.NewEncoder(w).Encode(providerapi.DeleteSecretKVResponse{Name: name, Key: key})
}

// handleWriteBatch implements secret-kvs:write-batch: one atomic request that
// applies upserts and idempotent deletes to one Secret. It creates the Secret
// only when Ensure is set or at least one KV is upserted, so a delete-only
// request against a missing Secret succeeds without creating it.
func (a *fakeProviderAPI) handleWriteBatch(w http.ResponseWriter, body []byte) {
	var payload providerapi.WriteSecretKVsRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		a.t.Errorf("decoding batch write request: %v", err)
	}

	if err := validateSecretName(payload.Name); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid secret name"}`)

		return
	}

	a.mu.Lock()

	secret := a.secrets[payload.Name]
	if secret == nil {
		if !payload.Ensure && len(payload.KVs) == 0 {
			a.mu.Unlock()

			_ = json.NewEncoder(w).Encode(providerapi.WriteSecretKVsResponse{
				Name: payload.Name,
				KVs:  []providerapi.SecretKVWriteMetadata{},
			})

			return
		}

		secret = &fakeSecret{values: map[string]string{}, marks: map[string]bool{}}
		a.secrets[payload.Name] = secret
	}

	for _, key := range payload.DeleteKeys {
		delete(secret.values, key)
		delete(secret.marks, key)
	}

	metadata := make([]providerapi.SecretKVWriteMetadata, 0, len(payload.KVs))
	responseName := payload.Name
	if a.batchName != nil {
		responseName = *a.batchName
	}

	for _, kv := range payload.KVs {
		mark := true
		if kv.IsEphemeral != nil {
			mark = *kv.IsEphemeral
		}

		secret.values[kv.Key] = kv.Value
		secret.marks[kv.Key] = mark

		responseKey := kv.Key
		if override, ok := a.batchKeys[kv.Key]; ok {
			responseKey = override
		}

		responseMark := mark
		if override, ok := a.batchMarks[kv.Key]; ok {
			responseMark = override
		}

		if a.batchDrops[kv.Key] {
			continue
		}

		metadata = append(metadata, providerapi.SecretKVWriteMetadata{
			Key:         responseKey,
			IsEphemeral: responseMark,
		})
	}

	a.mu.Unlock()

	_ = json.NewEncoder(w).Encode(providerapi.WriteSecretKVsResponse{
		Name: responseName,
		KVs:  metadata,
	})
}

func splitFakeReference(reference string) (string, string, bool) {
	index := strings.LastIndex(reference, "/")
	if index < 0 {
		return "", "", false
	}

	name, key := reference[:index], reference[index+1:]
	if validateSecretName(name) != nil || validateSecretKey(key) != nil {
		return "", "", false
	}

	return name, key, true
}

func (a *fakeProviderAPI) statusLocked() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.status
}

// setValues replaces the values of the default "bootstrap" Secret and marks
// every KV ephemeral.
func (a *fakeProviderAPI) setValues(values map[string]string) {
	marks := make(map[string]bool, len(values))
	for key := range values {
		marks[key] = true
	}

	a.setSecret("bootstrap", values, marks)
}

// setSecret replaces the values and ephemeral marks of one Secret.
func (a *fakeProviderAPI) setSecret(name string, values map[string]string, marks map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.secrets[name] = &fakeSecret{values: cloneStringMap(values), marks: cloneBoolMap(marks)}
}

func (a *fakeProviderAPI) setStatus(status int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.status = status
}

func (a *fakeProviderAPI) setBody(body string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.body = body
}

func (a *fakeProviderAPI) setResponseName(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.responseName = name
}

func (a *fakeProviderAPI) setWriteMark(mark bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.writeMark = &mark
}

func (a *fakeProviderAPI) setWriteKey(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.writeKey = &key
}

func (a *fakeProviderAPI) setBatchResponseName(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.batchName = &name
}

func (a *fakeProviderAPI) setBatchMark(key string, mark bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.batchMarks == nil {
		a.batchMarks = map[string]bool{}
	}

	a.batchMarks[key] = mark
}

func (a *fakeProviderAPI) setBatchKey(key, responseKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.batchKeys == nil {
		a.batchKeys = map[string]string{}
	}

	a.batchKeys[key] = responseKey
}

func (a *fakeProviderAPI) setBatchDropKey(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.batchDrops == nil {
		a.batchDrops = map[string]bool{}
	}

	a.batchDrops[key] = true
}

func (a *fakeProviderAPI) setRawRead(values map[string]string, marks map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.rawRead = &fakeReadResponse{
		values: cloneStringMap(values),
		marks:  cloneBoolMap(marks),
	}
}

// clearReadOverrides restores the normal per-KV mark filtering so a test can
// verify that a rejected response was not cached.
func (a *fakeProviderAPI) clearReadOverrides() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.rawRead = nil
	a.omitValues = false
	a.omitMarks = false
}

func (a *fakeProviderAPI) setOmitValues(omit bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.omitValues = omit
}

func (a *fakeProviderAPI) setOmitMarks(omit bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.omitMarks = omit
}

func (a *fakeProviderAPI) secretValues(name string) (map[string]string, map[string]bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	secret, ok := a.secrets[name]
	if !ok {
		return nil, nil, false
	}

	return cloneStringMap(secret.values), cloneBoolMap(secret.marks), true
}

func (a *fakeProviderAPI) url() string {
	return a.server.URL
}

func (a *fakeProviderAPI) calls() []recordedCall {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]recordedCall, len(a.callsList))
	copy(out, a.callsList)

	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}

	return out
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for key, value := range in {
		out[key] = value
	}

	return out
}
