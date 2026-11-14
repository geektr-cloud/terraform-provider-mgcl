// Package provider implements the mgcl OpenTofu provider.
//
// The provider deliberately exposes a single ephemeral resource,
// mgcl_secret. It registers no managed resources, data sources or functions,
// because every other resource kind would persist bootstrap secret values in
// plan or state.
package provider

import (
	"context"
	"os"
	"strings"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	providerschema "github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// New returns a provider factory for providerserver.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &mgclProvider{version: version}
	}
}

type mgclProvider struct {
	version string
}

// providerData is the provider-level configuration handed to ephemeral
// resources through Configure. It intentionally keeps raw environment values so
// Configure never performs network I/O and missing configuration can be
// reported by Open with an actionable message.
type providerData struct {
	Endpoint string
	Token    string
}

var (
	_ provider.Provider                       = (*mgclProvider)(nil)
	_ provider.ProviderWithFunctions          = (*mgclProvider)(nil)
	_ provider.ProviderWithEphemeralResources = (*mgclProvider)(nil)
)

func (p *mgclProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "mgcl"
	resp.Version = p.version
}

// Schema has no attributes on purpose: the provider takes no HCL
// configuration. Connection details come from MGCL_PROVIDER_ENDPOINT and
// MGCL_PROVIDER_TOKEN, which `mgcl run -- <command>` injects into the child
// process environment.
func (p *mgclProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = providerschema.Schema{
		MarkdownDescription: "The mgcl provider reads bootstrap secrets from a `mgcl run -- <command>` provider session. " +
			"Connection details are read from `MGCL_PROVIDER_ENDPOINT` and `MGCL_PROVIDER_TOKEN`; the provider has no HCL arguments. " +
			"Requires OpenTofu >= 1.11 for ephemeral resources.",
		Attributes: map[string]providerschema.Attribute{},
	}
}

// Configure reads connection settings from the environment without contacting
// the endpoint. Missing settings are not an error here so that `tofu validate`
// succeeds outside `mgcl run`; the first Open returns a precise diagnostic.
func (p *mgclProvider) Configure(_ context.Context, _ provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	data := &providerData{
		Endpoint: normalizeEndpoint(os.Getenv(providerapi.EnvEndpoint)),
		Token:    os.Getenv(providerapi.EnvToken),
	}

	resp.EphemeralResourceData = data
}

func (p *mgclProvider) Resources(_ context.Context) []func() resource.Resource {
	// No managed resources: values from mgcl must never reach plan or state.
	return nil
}

func (p *mgclProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	// No data sources: their values are persisted in state.
	return nil
}

func (p *mgclProvider) Functions(_ context.Context) []func() function.Function {
	// Provider functions cannot mark results sensitive or ephemeral.
	return nil
}

func (p *mgclProvider) EphemeralResources(_ context.Context) []func() ephemeral.EphemeralResource {
	return []func() ephemeral.EphemeralResource{
		NewSecretEphemeralResource,
	}
}

// normalizeEndpoint trims whitespace and any trailing slash so the client can
// append providerapi.SecretsReadPath without producing a double slash.
func normalizeEndpoint(endpoint string) string {
	return strings.TrimRight(strings.TrimSpace(endpoint), "/")
}
