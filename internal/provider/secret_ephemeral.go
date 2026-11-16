package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	ephemeralschema "github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// secretNameSegmentPattern mirrors one segment of the mgcl Secret name and the
// KV key grammar. Names are validated here so a typo fails before any HTTP
// request is made.
var secretNameSegmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// maxSecretNameBytes is the maximum length of a full Secret name, including
// its separating slashes.
const maxSecretNameBytes = 64

// NewSecretEphemeralResource returns the mgcl_secret ephemeral resource.
func NewSecretEphemeralResource() ephemeral.EphemeralResource {
	return &secretEphemeralResource{}
}

type secretEphemeralResource struct {
	providerData *providerData
}

var (
	_ ephemeral.EphemeralResource                   = (*secretEphemeralResource)(nil)
	_ ephemeral.EphemeralResourceWithConfigure      = (*secretEphemeralResource)(nil)
	_ ephemeral.EphemeralResourceWithValidateConfig = (*secretEphemeralResource)(nil)
	_ ephemeral.EphemeralResourceWithClose          = (*secretEphemeralResource)(nil)
)

// secretModel maps the mgcl_secret schema.
type secretModel struct {
	Name   types.String `tfsdk:"name"`
	Values types.Map    `tfsdk:"values"`
}

func (r *secretEphemeralResource) Metadata(_ context.Context, _ ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = "mgcl_secret"
}

func (r *secretEphemeralResource) Schema(_ context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = ephemeralschema.Schema{
		MarkdownDescription: "Reads every key/value pair of one mgcl Secret as an ephemeral resource, including public " +
			"and ephemeral KVs. Values exist only in memory during plan and apply: OpenTofu rejects using them where " +
			"they would be persisted, and the provider never writes them to plan or state. Use " +
			"`provider::mgcl::secret_value` only for KVs explicitly marked public.",
		Attributes: map[string]ephemeralschema.Attribute{
			"name": ephemeralschema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Name of the mgcl Secret to read. One or more `/`-separated segments, each matching `^[a-z0-9][a-z0-9_-]{0,63}$`, with a total length of at most 64 bytes including slashes.",
			},
			"values": ephemeralschema.MapAttribute{
				Computed:            true,
				Sensitive:           true,
				ElementType:         types.StringType,
				MarkdownDescription: "Current key/value pairs of the Secret, including public and ephemeral KVs. Empty Secrets return an empty map.",
			},
		},
	}
}

// Configure stores the provider-level environment configuration. It performs no
// I/O; the endpoint is contacted from Open.
func (r *secretEphemeralResource) Configure(_ context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
	if req.ProviderData == nil {
		// Configure was called without provider data (for example by a unit
		// test or an older framework path). Open reports the missing
		// configuration.
		return
	}

	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected mgcl provider data",
			fmt.Sprintf("The mgcl provider received unexpected data of type %T while configuring mgcl_secret.", req.ProviderData),
		)

		return
	}

	r.providerData = data
}

// ValidateConfig checks the name grammar when the value is already known.
// Unknown values are skipped because validation must not require evaluation.
func (r *secretEphemeralResource) ValidateConfig(ctx context.Context, req ephemeral.ValidateConfigRequest, resp *ephemeral.ValidateConfigResponse) {
	var name types.String

	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("name"), &name)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if name.IsNull() || name.IsUnknown() {
		return
	}

	if err := validateSecretName(name.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid secret name", err.Error())
	}
}

// Open reads the secret once through the provider API. Every Open performs a
// fresh request; nothing is cached across opens.
func (r *secretEphemeralResource) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var data secretModel

	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if data.Name.IsNull() {
		resp.Diagnostics.AddAttributeError(
			path.Root("name"),
			"Secret name is required",
			"The name attribute is required and must be set to a non-null string.",
		)

		return
	}

	if data.Name.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("name"),
			"Secret name must be known",
			"The name of an mgcl_secret must be known before the ephemeral resource is opened. "+
				"Use a literal string or another value known during plan; secrets cannot be selected by an unknown expression.",
		)

		return
	}

	name := data.Name.ValueString()

	if err := validateSecretName(name); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid secret name", err.Error())

		return
	}

	if r.providerData == nil {
		resp.Diagnostics.AddError(
			"mgcl provider is not configured",
			fmt.Sprintf("%[1]s is not set. The mgcl provider only works inside `mgcl run -- <command>`, "+
				"which injects %[1]s and %[2]s into the child process environment.", providerapi.EnvEndpoint, providerapi.EnvToken),
		)

		return
	}

	endpoint, token := r.providerData.credentials()

	if endpoint == "" {
		resp.Diagnostics.AddError(
			"mgcl provider is not configured",
			fmt.Sprintf("%[1]s is not set. The mgcl provider only works inside `mgcl run -- <command>`, "+
				"which injects %[1]s and %[2]s into the child process environment.", providerapi.EnvEndpoint, providerapi.EnvToken),
		)

		return
	}

	if token == "" {
		resp.Diagnostics.AddError(
			"mgcl provider token is missing",
			fmt.Sprintf("%s is not set. The mgcl provider only works inside `mgcl run -- <command>`, "+
				"which provides a short-lived provider token.", providerapi.EnvToken),
		)

		return
	}

	client := &providerapi.Client{
		Endpoint: endpoint,
		Token:    token,
	}

	result, err := client.ReadSecret(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to read mgcl secret",
			fmt.Sprintf("Could not read secret %q from %s: %s",
				name, endpoint, redactToken(err.Error(), token)),
		)

		return
	}

	if result == nil {
		resp.Diagnostics.AddError(
			"Unexpected mgcl secret response",
			"The provider API returned an empty response; no secret values were used.",
		)

		return
	}

	if result.Name != name {
		// Never adopt values from a response for a different secret. Only
		// names, which mgcl treats as public metadata, appear in the message;
		// token redaction is a safety net.
		resp.Diagnostics.AddError(
			"Unexpected mgcl secret response",
			redactToken(fmt.Sprintf("The provider API returned secret %q while %q was requested; the returned values were discarded.",
				result.Name, name), token),
		)

		return
	}

	values := map[string]string{}
	if result.Values != nil {
		values = result.Values
	}

	valuesMap, diags := types.MapValueFrom(ctx, types.StringType, values)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.Values = valuesMap

	resp.Diagnostics.Append(resp.Result.Set(ctx, &data)...)
}

// Close releases nothing: the ephemeral resource holds no remote object and
// performs no persistence. The method exists so the lifecycle is explicit.
func (r *secretEphemeralResource) Close(_ context.Context, _ ephemeral.CloseRequest, _ *ephemeral.CloseResponse) {
}

// validateSecretName accepts a hierarchical Secret name: one or more
// slash-separated segments, each matching the v1 segment grammar, with a total
// length of at most 64 bytes including the slashes.
func validateSecretName(name string) error {
	if name == "" {
		return errors.New("secret name must not be empty")
	}

	if len(name) > maxSecretNameBytes {
		return fmt.Errorf("secret name %q must be at most %d bytes including slashes", name, maxSecretNameBytes)
	}

	for _, segment := range strings.Split(name, "/") {
		if !secretNameSegmentPattern.MatchString(segment) {
			return fmt.Errorf("secret name %q has segment %q that must match %s: 1 to 64 lowercase ASCII letters, digits, '_' or '-', starting with a letter or digit",
				name, segment, secretNameSegmentPattern.String())
		}
	}

	return nil
}

// validateSecretKey accepts one KV key. Keys are always a single segment and
// never contain a slash, even when the owning Secret name is hierarchical.
func validateSecretKey(key string) error {
	if !secretNameSegmentPattern.MatchString(key) {
		return fmt.Errorf("secret key %q must match %s: a single segment of 1 to 64 lowercase ASCII letters, digits, '_' or '-', starting with a letter or digit; keys cannot contain '/'",
			key, secretNameSegmentPattern.String())
	}

	return nil
}

// redactToken removes the provider token from error text as a safety net. The
// provider neither logs tokens nor includes them in diagnostics, but wrapped
// errors may echo the bearer credential.
func redactToken(message, token string) string {
	if token == "" {
		return message
	}

	return strings.ReplaceAll(message, token, "(redacted)")
}
