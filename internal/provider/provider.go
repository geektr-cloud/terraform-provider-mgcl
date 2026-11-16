// Package provider implements the mgcl OpenTofu provider.
//
// The provider exposes:
//   - the mgcl_secret ephemeral resource, which reads every KV of a Secret
//     without persisting values;
//   - the mgcl_secret managed resource, which owns a set of KVs in one Secret
//     and persists their values in plan and state;
//   - the provider::mgcl::secret_value function, which reads public KVs and
//     returns ordinary string or map data;
//   - the mgcl_secret_kv managed resource, which writes one KV and persists
//     its value in plan and state.
//
// It registers no data sources.
package provider

import (
	"context"
	"os"
	"strings"
	"sync"

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

	mu   sync.RWMutex
	data *providerData
}

// providerData is the provider-level configuration handed to ephemeral
// resources, managed resources and provider functions through Configure. It
// intentionally keeps raw environment values so Configure never performs
// network I/O and missing configuration can be reported by the first Open,
// function call or resource operation with an actionable message.
//
// OpenTofu may call provider functions before ConfigureProvider, so the
// function path also initializes providerData directly from the environment.
//
// The public-KV cache lives here so it follows the provider configuration and
// its lifetime, rather than a package-level registry keyed by credentials.
type providerData struct {
	mu       sync.RWMutex
	Endpoint string
	Token    string

	cacheMu      sync.Mutex
	publicValues map[string]map[string]string
}

func newProviderDataFromEnv() *providerData {
	return newProviderData(
		normalizeEndpoint(os.Getenv(providerapi.EnvEndpoint)),
		os.Getenv(providerapi.EnvToken),
	)
}

func newProviderData(endpoint, token string) *providerData {
	return &providerData{
		Endpoint:     endpoint,
		Token:        token,
		publicValues: map[string]map[string]string{},
	}
}

func (d *providerData) setCredentials(endpoint, token string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.Endpoint = endpoint
	d.Token = token
}

func (d *providerData) credentials() (string, string) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return d.Endpoint, d.Token
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
		MarkdownDescription: "The mgcl provider reads and writes bootstrap secrets through a `mgcl run -- <command>` provider session. " +
			"Connection details are read from `MGCL_PROVIDER_ENDPOINT` and `MGCL_PROVIDER_TOKEN`; the provider has no HCL arguments. " +
			"Requires OpenTofu >= 1.11 for ephemeral resources. " +
			"The `mgcl_secret` ephemeral resource keeps values out of plan and state. " +
			"The `provider::mgcl::secret_value` function reads only KVs marked public; its result is ordinary " +
			"string or map data that OpenTofu may persist in plan, state and logs. " +
			"The `mgcl_secret` managed resource owns a set of KVs in one Secret; their values are always stored in " +
			"plan and state, and `Sensitive` only hides them from CLI output. " +
			"The `mgcl_secret_kv` managed resource writes one KV; its value is always stored in plan and state, " +
			"and `Sensitive` only hides it from CLI output.",
		Attributes: map[string]providerschema.Attribute{},
	}
}

// Configure reads connection settings from the environment without contacting
// the endpoint. Missing settings are not an error here so that `tofu validate`
// succeeds outside `mgcl run`; the first Open, function call or resource
// operation returns a precise diagnostic.
func (p *mgclProvider) Configure(_ context.Context, _ provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	data := newProviderDataFromEnv()

	p.mu.Lock()
	if p.data == nil {
		p.data = data
	} else {
		// OpenTofu can evaluate provider functions before ConfigureProvider.
		// Reuse the existing providerData so a cache populated by an earlier
		// function call survives, while still refreshing the environment
		// values.
		p.data.setCredentials(data.Endpoint, data.Token)
		data = p.data
	}
	p.mu.Unlock()

	// The framework version in use has no function-specific Configure hook, so
	// functions resolve this same providerData through their provider pointer.
	resp.EphemeralResourceData = data
	resp.ResourceData = data
}

// providerData returns the provider configuration, initializing it from the
// environment when OpenTofu evaluates a provider function before
// ConfigureProvider. Initialization performs no network I/O.
func (p *mgclProvider) providerData() *providerData {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.data == nil {
		p.data = newProviderDataFromEnv()
	}

	return p.data
}

func (p *mgclProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewSecretResource,
		NewSecretKVResource,
	}
}

func (p *mgclProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	// No data sources: their values are persisted in state.
	return nil
}

func (p *mgclProvider) Functions(_ context.Context) []func() function.Function {
	return []func() function.Function{
		func() function.Function {
			return NewSecretValueFunction(p)
		},
	}
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
