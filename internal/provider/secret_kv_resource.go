package provider

import (
	"context"
	"fmt"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	resourceschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// NewSecretKVResource returns the mgcl_secret_kv managed resource.
func NewSecretKVResource() resource.Resource {
	return &secretKVResource{}
}

type secretKVResource struct {
	providerData *providerData
}

var (
	_ resource.Resource                   = (*secretKVResource)(nil)
	_ resource.ResourceWithConfigure      = (*secretKVResource)(nil)
	_ resource.ResourceWithValidateConfig = (*secretKVResource)(nil)
)

// secretKVModel maps the mgcl_secret_kv schema. Value is sensitive only for
// display: the framework still stores it in plan and state.
type secretKVModel struct {
	Key           types.String `tfsdk:"key"`
	Value         types.String `tfsdk:"value"`
	EphemeralMark types.Bool   `tfsdk:"ephemeral_mark"`
}

func (r *secretKVResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "mgcl_secret_kv"
}

func (r *secretKVResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = resourceschema.Schema{
		MarkdownDescription: "Writes one KV into an mgcl Secret. The Secret is created atomically by the provider API " +
			"when it does not exist. Each KV must be owned by only one resource or managed `mgcl_secret`; different " +
			"resources may manage disjoint keys of the same Secret, but overlapping ownership is unsupported. The " +
			"value is always stored in plan and state; `Sensitive` only hides it from CLI " +
			"output. Changing `key` replaces the resource; changing `value` or `ephemeral_mark` updates the same KV in place.\n\n" +
			"> **Warning:** do not read the same KV through `provider::mgcl::secret_value` or `mgcl_secret` in the same " +
			"> OpenTofu project that manages it with `mgcl_secret_kv`. The mgcl provider does not promise idempotence or " +
			"> transaction ordering between resource writes and function/ephemeral reads; use a separate project for reads.",
		Attributes: map[string]resourceschema.Attribute{
			"key": resourceschema.StringAttribute{
				Required: true,
				MarkdownDescription: "KV reference in the form `<secret-name>/<key>`, split at the last `/`. " +
					"`<secret-name>` is " + secretNameSchemaDescription + " `<key>` is " +
					secretKeySchemaDescription + " Changing this attribute replaces the resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"value": resourceschema.StringAttribute{
				Required:            true,
				Sensitive:           true,
				MarkdownDescription: "Value to store. The value is always persisted in plan and state; `Sensitive` only hides it from CLI output.",
			},
			"ephemeral_mark": resourceschema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether this KV is marked ephemeral. Defaults to `true`. Ephemeral KVs cannot be read by `provider::mgcl::secret_value`; only `mgcl_secret` returns them.",
			},
		},
	}
}

// Configure stores the shared provider data. It performs no I/O.
func (r *secretKVResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		// Configure was called without provider data (for example by a unit
		// test). Operations report the missing configuration.
		return
	}

	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected mgcl provider data",
			fmt.Sprintf("The mgcl provider received unexpected data of type %T while configuring mgcl_secret_kv.", req.ProviderData),
		)

		return
	}

	r.providerData = data
}

// ValidateConfig checks the KV reference when the value is already known.
// Unknown values are skipped because validation must not require evaluation.
func (r *secretKVResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var key types.String

	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("key"), &key)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if key.IsNull() || key.IsUnknown() {
		return
	}

	if _, err := validateConcreteSecretReference(key.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("key"), "Invalid secret KV reference", err.Error())
	}
}

func (r *secretKVResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan secretKVModel

	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.write(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secretKVResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan secretKVModel

	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.write(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read fetches every KV of the Secret, then synchronizes this resource's value
// and mark so out-of-band changes are detected as drift. A missing Secret or KV
// removes the resource from state.
func (r *secretKVResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state secretKVModel

	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ref, ok := r.validReference(ctx, state.Key, "MGCL secret KV", &resp.Diagnostics)
	if !ok {
		return
	}

	client, token, ok := r.client(ctx, &resp.Diagnostics)
	if !ok {
		return
	}

	result, err := client.ReadSecret(ctx, ref.Name)
	if err != nil {
		if providerAPIStatus(err) == 404 {
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError(
			"Failed to read mgcl secret KV",
			fmt.Sprintf("Could not read secret %q from %s: %s",
				ref.Name, client.Endpoint, redactToken(err.Error(), token)),
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

	if result.Name != ref.Name {
		resp.Diagnostics.AddError(
			"Unexpected mgcl secret response",
			redactToken(fmt.Sprintf("The provider API returned secret %q while %q was requested; the returned values were discarded.",
				result.Name, ref.Name), token),
		)

		return
	}

	value, exists := result.Values[ref.Key]
	if !exists {
		resp.State.RemoveResource(ctx)
		return
	}

	mark, marked := result.EphemeralMarks[ref.Key]
	if !marked {
		// Never guess a mark: a wrong guess could silently turn an ephemeral
		// KV into a public one or the reverse.
		resp.Diagnostics.AddError(
			"Missing ephemeral mark in mgcl secret response",
			fmt.Sprintf("The provider API returned a value for key %q in secret %q without its ephemeral mark; "+
				"refusing to guess whether the KV is public or ephemeral.", ref.Key, ref.Name),
		)

		return
	}

	state.Value = types.StringValue(value)
	state.EphemeralMark = types.BoolValue(mark)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *secretKVResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state secretKVModel

	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ref, ok := r.validReference(ctx, state.Key, "MGCL secret KV", &resp.Diagnostics)
	if !ok {
		return
	}

	client, token, ok := r.client(ctx, &resp.Diagnostics)
	if !ok {
		return
	}

	// DeleteSecretKV is idempotent: a missing Secret or KV is not an error and
	// never creates a Secret. The Secret itself is left in place, possibly
	// empty.
	if _, err := client.DeleteSecretKV(ctx, state.Key.ValueString()); err != nil {
		resp.Diagnostics.AddError(
			"Failed to delete mgcl secret KV",
			fmt.Sprintf("Could not delete key %q from secret %q: %s",
				ref.Key, ref.Name, redactToken(err.Error(), token)),
		)
	}
}

// write sends the planned KV to the provider API. It never adopts a response
// value; the write response contains only metadata. On success the planned
// ephemeral mark is normalized to the value actually written, so state never
// records a null mark.
func (r *secretKVResource) write(ctx context.Context, plan *secretKVModel, diags *diag.Diagnostics) {
	ref, ok := r.validReference(ctx, plan.Key, "MGCL secret KV", diags)
	if !ok {
		return
	}

	if plan.Value.IsNull() || plan.Value.IsUnknown() {
		diags.AddAttributeError(path.Root("value"), "Secret value must be known", "The value must be a known string before it can be written.")
		return
	}

	isEphemeral := effectiveEphemeralMark(plan.EphemeralMark)

	client, token, ok := r.client(ctx, diags)
	if !ok {
		return
	}

	result, err := client.WriteSecretKV(ctx, plan.Key.ValueString(), plan.Value.ValueString(), isEphemeral)
	if err != nil {
		diags.AddError(
			"Failed to write mgcl secret KV",
			fmt.Sprintf("Could not write key %q to secret %q: %s",
				ref.Key, ref.Name, redactToken(err.Error(), token)),
		)

		return
	}

	if result == nil {
		diags.AddError(
			"Unexpected mgcl secret response",
			"The provider API returned an empty write response; the KV write was not confirmed.",
		)

		return
	}

	if result.Name != ref.Name || result.Key != ref.Key || result.IsEphemeral != isEphemeral {
		diags.AddError(
			"Unexpected mgcl secret response",
			redactToken(fmt.Sprintf(
				"The provider API confirmed secret %q key %q with ephemeral mark %t while secret %q key %q with mark %t was written; "+
					"the response metadata did not match the request.",
				result.Name, result.Key, result.IsEphemeral, ref.Name, ref.Key, isEphemeral), token),
		)

		return
	}

	plan.EphemeralMark = types.BoolValue(isEphemeral)
}

// effectiveEphemeralMark maps a planned mark to the value sent to the API. The
// schema default already fills a null mark; this keeps the write path correct
// even when a plan bypasses the default.
func effectiveEphemeralMark(mark types.Bool) bool {
	if mark.IsNull() || mark.IsUnknown() {
		return true
	}

	return mark.ValueBool()
}

// validateConcreteSecretReference parses a concrete <secret-name>/<key>
// reference. Wildcards are never valid for a managed resource.
func validateConcreteSecretReference(reference string) (secretReference, error) {
	ref, err := parseSecretReference(reference)
	if err != nil {
		return secretReference{}, err
	}

	if ref.AllKeys {
		return secretReference{}, fmt.Errorf(
			"secret reference %q uses a wildcard; mgcl_secret_kv requires exactly <secret-name>/<key>", reference,
		)
	}

	return ref, nil
}

// validReference parses a concrete <secret-name>/<key> attribute.
func (r *secretKVResource) validReference(_ context.Context, value types.String, label string, diags *diag.Diagnostics) (secretReference, bool) {
	if value.IsNull() || value.IsUnknown() {
		diags.AddAttributeError(path.Root("key"), label+" key is required", "The key must be a known string in the form <secret-name>/<key>.")
		return secretReference{}, false
	}

	ref, err := validateConcreteSecretReference(value.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("key"), "Invalid secret KV reference", err.Error())
		return secretReference{}, false
	}

	return ref, true
}

// client returns the configured API client and token. The token is used only
// for redaction.
func (r *secretKVResource) client(_ context.Context, diags *diag.Diagnostics) (*providerapi.Client, string, bool) {
	if r.providerData == nil {
		diags.AddError(
			"mgcl provider is not configured",
			fmt.Sprintf("%[1]s is not set. The mgcl provider only works inside `mgcl run -- <command>`, "+
				"which injects %[1]s and %[2]s into the child process environment.", providerapi.EnvEndpoint, providerapi.EnvToken),
		)

		return nil, "", false
	}

	endpoint, token := r.providerData.credentials()
	if endpoint == "" {
		diags.AddError(
			"mgcl provider is not configured",
			fmt.Sprintf("%[1]s is not set. The mgcl provider only works inside `mgcl run -- <command>`, "+
				"which injects %[1]s and %[2]s into the child process environment.", providerapi.EnvEndpoint, providerapi.EnvToken),
		)

		return nil, "", false
	}

	if token == "" {
		diags.AddError(
			"mgcl provider token is missing",
			fmt.Sprintf("%s is not set. The mgcl provider only works inside `mgcl run -- <command>`, "+
				"which provides a short-lived provider token.", providerapi.EnvToken),
		)

		return nil, "", false
	}

	return &providerapi.Client{Endpoint: endpoint, Token: token}, token, true
}
