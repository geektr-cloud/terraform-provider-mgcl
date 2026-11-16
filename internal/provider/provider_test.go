package provider

import (
	"context"
	"fmt"
	"testing"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

func TestProviderMetadata(t *testing.T) {
	p := New("1.2.3")()

	var resp provider.MetadataResponse
	p.Metadata(context.Background(), provider.MetadataRequest{}, &resp)

	if resp.TypeName != "mgcl" {
		t.Fatalf("TypeName = %q, want %q", resp.TypeName, "mgcl")
	}

	if resp.Version != "1.2.3" {
		t.Fatalf("Version = %q, want %q", resp.Version, "1.2.3")
	}
}

func TestProviderSchemaHasNoAttributes(t *testing.T) {
	p := New("dev")()

	var resp provider.SchemaResponse
	p.Schema(context.Background(), provider.SchemaRequest{}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	if len(resp.Schema.Attributes) != 0 {
		t.Fatalf("provider schema has attributes %v, want none", resp.Schema.Attributes)
	}

	if len(resp.Schema.Blocks) != 0 {
		t.Fatalf("provider schema has blocks %v, want none", resp.Schema.Blocks)
	}
}

func TestProviderRegistersEphemeralResourceFunctionAndManagedResource(t *testing.T) {
	p := New("dev")()

	resources := p.Resources(context.Background())
	if len(resources) != 2 {
		t.Fatalf("managed resources = %d, want 2", len(resources))
	}

	managedTypes := map[string]string{}

	for _, factory := range resources {
		managed := factory()

		var metadata resource.MetadataResponse
		managed.Metadata(context.Background(), resource.MetadataRequest{}, &metadata)

		managedTypes[fmt.Sprintf("%T", managed)] = metadata.TypeName
	}

	if got := managedTypes["*provider.secretResource"]; got != "mgcl_secret" {
		t.Fatalf("managed resource *secretResource name = %q, want mgcl_secret", got)
	}

	if got := managedTypes["*provider.secretKVResource"]; got != "mgcl_secret_kv" {
		t.Fatalf("managed resource *secretKVResource name = %q, want mgcl_secret_kv", got)
	}

	if dataSources := p.DataSources(context.Background()); dataSources != nil {
		t.Fatalf("provider registers data sources, want none")
	}

	functions := p.(provider.ProviderWithFunctions).Functions(context.Background())
	if len(functions) != 1 {
		t.Fatalf("functions = %d, want 1", len(functions))
	}

	f := functions[0]()
	if _, ok := f.(*secretValueFunction); !ok {
		t.Fatalf("function type = %T, want *secretValueFunction", f)
	}

	var metadata function.MetadataResponse
	f.Metadata(context.Background(), function.MetadataRequest{}, &metadata)

	if metadata.Name != "secret_value" {
		t.Fatalf("function name = %q, want secret_value", metadata.Name)
	}

	ephemerals := p.(provider.ProviderWithEphemeralResources).EphemeralResources(context.Background())
	if len(ephemerals) != 1 {
		t.Fatalf("ephemeral resources = %d, want 1", len(ephemerals))
	}

	r := ephemerals[0]()
	if _, ok := r.(*secretEphemeralResource); !ok {
		t.Fatalf("ephemeral resource type = %T, want *secretEphemeralResource", r)
	}
}

func TestProviderConfigureReadsEnvironmentWithoutNetwork(t *testing.T) {
	t.Setenv(providerapi.EnvEndpoint, "http://127.0.0.1:1217/")
	t.Setenv(providerapi.EnvToken, "test-token")

	p := New("dev")()

	var resp provider.ConfigureResponse
	p.Configure(context.Background(), provider.ConfigureRequest{}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	data, ok := resp.EphemeralResourceData.(*providerData)
	if !ok {
		t.Fatalf("EphemeralResourceData type = %T, want *providerData", resp.EphemeralResourceData)
	}

	if data.Endpoint != "http://127.0.0.1:1217" {
		t.Fatalf("Endpoint = %q, want trailing slash trimmed", data.Endpoint)
	}

	if data.Token != "test-token" {
		t.Fatalf("Token = %q, want token from environment", data.Token)
	}

	if resp.ResourceData != resp.EphemeralResourceData {
		t.Fatal("resource data and ephemeral resource data differ")
	}
}

func TestProviderConfigureAllowsMissingEnvironment(t *testing.T) {
	t.Setenv(providerapi.EnvEndpoint, "")
	t.Setenv(providerapi.EnvToken, "")

	p := New("dev")()

	var resp provider.ConfigureResponse
	p.Configure(context.Background(), provider.ConfigureRequest{}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure must not fail without configuration: %v", resp.Diagnostics.Errors())
	}

	data, ok := resp.EphemeralResourceData.(*providerData)
	if !ok {
		t.Fatalf("EphemeralResourceData type = %T, want *providerData", resp.EphemeralResourceData)
	}

	if data.Endpoint != "" || data.Token != "" {
		t.Fatalf("provider data = %+v, want empty endpoint and token", data)
	}

	if resp.ResourceData != resp.EphemeralResourceData {
		t.Fatal("resource data and ephemeral resource data differ")
	}
}

func TestSecretEphemeralResourceConfigureStoresProviderData(t *testing.T) {
	r := NewSecretEphemeralResource().(*secretEphemeralResource)

	data := &providerData{Endpoint: "http://127.0.0.1:1217", Token: "tok"}

	var resp ephemeral.ConfigureResponse
	r.Configure(context.Background(), ephemeral.ConfigureRequest{ProviderData: data}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	if r.providerData != data {
		t.Fatalf("providerData = %+v, want stored provider data", r.providerData)
	}
}

func TestSecretEphemeralResourceConfigureNilProviderData(t *testing.T) {
	r := NewSecretEphemeralResource().(*secretEphemeralResource)

	var resp ephemeral.ConfigureResponse
	r.Configure(context.Background(), ephemeral.ConfigureRequest{}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics.Errors())
	}

	if r.providerData != nil {
		t.Fatalf("providerData = %+v, want nil", r.providerData)
	}
}

func TestSecretEphemeralResourceConfigureRejectsUnexpectedData(t *testing.T) {
	r := NewSecretEphemeralResource().(*secretEphemeralResource)

	var resp ephemeral.ConfigureResponse
	r.Configure(context.Background(), ephemeral.ConfigureRequest{ProviderData: struct{}{}}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected error for unexpected provider data type")
	}

	// The diagnostic must not include the token (there is none here) or any
	// secret-looking value.
	if got := diagnosticsText(&resp.Diagnostics); got == "" {
		t.Fatal("expected a diagnostic detail")
	}
}
