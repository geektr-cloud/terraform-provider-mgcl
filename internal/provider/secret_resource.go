package provider

import (
	"context"
	"fmt"
	"sort"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	resourceschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

// NewSecretResource returns the mgcl_secret managed resource. It owns the KVs
// listed in its contents map and never deletes the Secret, its DEK or KVs it
// does not manage.
func NewSecretResource() resource.Resource {
	return &secretResource{}
}

type secretResource struct {
	providerData *providerData
}

var (
	_ resource.Resource                   = (*secretResource)(nil)
	_ resource.ResourceWithConfigure      = (*secretResource)(nil)
	_ resource.ResourceWithValidateConfig = (*secretResource)(nil)
)

// managedSecretModel maps the mgcl_secret schema. Contents keys are bare KV
// keys; the Secret name may be hierarchical.
type managedSecretModel struct {
	Name     types.String `tfsdk:"name"`
	Contents types.Map    `tfsdk:"contents"`
}

// secretContentModel is one entry of the contents map. Value is sensitive only
// for display: the framework still stores it in plan and state.
type secretContentModel struct {
	Value         types.String `tfsdk:"value"`
	EphemeralMark types.Bool   `tfsdk:"ephemeral_mark"`
}

var secretContentAttrTypes = map[string]attr.Type{
	"value":          types.StringType,
	"ephemeral_mark": types.BoolType,
}

var secretContentObjectType = types.ObjectType{AttrTypes: secretContentAttrTypes}

// Plan-local size prechecks mirror the server limits. They only count the
// values in the plan, not keys removed from state or KVs already stored on the
// server; the server still validates the final KV set.
const (
	maxSecretKVs        = 64
	maxSecretValueBytes = 64 << 10
	maxSecretTotalBytes = 256 << 10
)

func (r *secretResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "mgcl_secret"
}

func (r *secretResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = resourceschema.Schema{
		MarkdownDescription: "Manages a set of KVs in one mgcl Secret. It writes every key in `contents` in one atomic " +
			"provider API request, creates the Secret (with its DEK) when it does not exist, and deletes only the keys " +
			"it owns. It never deletes the Secret, its DEK or KVs that are not listed in `contents`. Values are always " +
			"stored in plan and state; `Sensitive` only hides them from CLI output. The Secret name may contain " +
			"`/`-separated segments; each `contents` key is one bare KV key without `/`. Each KV must be owned by " +
			"only one resource or `mgcl_secret_kv`; different resources may manage disjoint keys of the same Secret, " +
			"but overlapping ownership is unsupported. This resource cannot be imported.\n\n" +
			"> **Warning:** do not read the same KV through `provider::mgcl::secret_value` or the `mgcl_secret` ephemeral " +
			"resource in the same OpenTofu project that manages it with the `mgcl_secret` managed resource. The mgcl " +
			"provider does not promise idempotence or transaction ordering between resource writes and function or " +
			"ephemeral reads; use a separate project for reads.",
		Attributes: map[string]resourceschema.Attribute{
			"name": resourceschema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the Secret. " + secretNameSchemaDescription + " " +
					"Changing this attribute replaces the resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"contents": resourceschema.MapNestedAttribute{
				Required: true,
				MarkdownDescription: "KVs owned by this resource. The map key is the bare KV key. " +
					secretKeySchemaDescription + " An empty map is allowed; " +
					"`null` is not. Removing a key from the map deletes that KV but leaves the Secret in place.",
				NestedObject: resourceschema.NestedAttributeObject{
					Attributes: map[string]resourceschema.Attribute{
						"value": resourceschema.StringAttribute{
							Required:            true,
							Sensitive:           true,
							MarkdownDescription: "KV value. The value is always persisted in plan and state; `Sensitive` only hides it from CLI output. An empty string is allowed; `null` is not.",
						},
						"ephemeral_mark": resourceschema.BoolAttribute{
							Optional:            true,
							Computed:            true,
							Default:             booldefault.StaticBool(true),
							MarkdownDescription: "Whether this KV is marked ephemeral. Defaults to `true`. Ephemeral KVs cannot be read by `provider::mgcl::secret_value`; the `mgcl_secret` ephemeral resource returns them.",
						},
					},
				},
			},
		},
	}
}

// Configure stores the shared provider data. It performs no I/O.
func (r *secretResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		// Configure was called without provider data (for example by a unit
		// test). Operations report the missing configuration.
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

// ValidateConfig checks the Secret name and bare KV keys when they are already
// known. Unknown values are skipped because validation must not require
// evaluation.
func (r *secretResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var name types.String

	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("name"), &name)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !name.IsNull() && !name.IsUnknown() {
		if err := validateSecretName(name.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid secret name", err.Error())
		}
	}

	var contents types.Map

	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("contents"), &contents)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if contents.IsNull() || contents.IsUnknown() {
		return
	}

	// Decode entries element-by-element. A null or unknown entry object is
	// skipped here so OpenTofu core can report the missing required value;
	// ElementsAs on a null element would surface a framework "provider bug"
	// conversion error instead.
	validationEntries := map[string]secretContentModel{}

	for key, element := range contents.Elements() {
		if element.IsNull() || element.IsUnknown() {
			continue
		}

		object, ok := element.(types.Object)
		if !ok {
			resp.Diagnostics.AddAttributeError(
				path.Root("contents").AtMapKey(key),
				"Secret contents must be known",
				fmt.Sprintf("The contents entry for key %q is not a known object; every entry must be a known object with a known value.", key),
			)

			continue
		}

		var entry secretContentModel

		resp.Diagnostics.Append(object.As(ctx, &entry, basetypes.ObjectAsOptions{})...)
		if resp.Diagnostics.HasError() {
			return
		}

		validationEntries[key] = entry

		if err := validateSecretKey(key); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("contents").AtMapKey(key), "Invalid secret key", err.Error())
		}

		if entry.Value.IsNull() {
			resp.Diagnostics.AddAttributeError(
				path.Root("contents").AtMapKey(key).AtName("value"),
				"Secret value must not be null",
				"Every contents entry requires a string value; use an empty string for an empty value.",
			)
		}
	}

	validateContentsLimits(validationEntries, &resp.Diagnostics)
}

func (r *secretResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan managedSecretModel

	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	entries, ok := r.write(ctx, &plan, nil, &resp.Diagnostics)
	if !ok {
		return
	}

	setManagedContents(ctx, &plan, entries, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secretResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state managedSecretModel

	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	deleteKeys := keysRemovedFromState(ctx, state.Contents, plan.Contents, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	entries, ok := r.write(ctx, &plan, deleteKeys, &resp.Diagnostics)
	if !ok {
		return
	}

	setManagedContents(ctx, &plan, entries, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read fetches every KV and mark of the Secret. Managed keys that still exist
// are synchronized (so out-of-band changes show up as drift); managed keys that
// disappeared are dropped from state so the next Update writes them back. Extra
// remote KVs are ignored. A missing Secret removes the resource from state.
func (r *secretResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state managedSecretModel

	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name, ok := r.validName(ctx, state.Name, &resp.Diagnostics)
	if !ok {
		return
	}

	entries, ok := contentsFromValue(ctx, state.Contents, &resp.Diagnostics)
	if !ok {
		return
	}

	client, token, ok := r.client(ctx, &resp.Diagnostics)
	if !ok {
		return
	}

	result, err := client.ReadSecret(ctx, name)
	if err != nil {
		if providerAPIStatus(err) == 404 {
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError(
			"Failed to read mgcl secret",
			fmt.Sprintf("Could not read secret %q from %s: %s",
				name, client.Endpoint, redactToken(err.Error(), token)),
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
		resp.Diagnostics.AddError(
			"Unexpected mgcl secret response",
			redactToken(fmt.Sprintf("The provider API returned secret %q while %q was requested; the returned values were discarded.",
				result.Name, name), token),
		)

		return
	}

	synced := make(map[string]secretContentModel, len(entries))

	keys := sortedContentKeys(entries)
	for _, key := range keys {
		value, exists := result.Values[key]
		if !exists {
			// Drop the key from state. The configuration still lists it, so
			// the next plan shows an update that writes it back.
			continue
		}

		mark, marked := result.EphemeralMarks[key]
		if !marked {
			// Never guess a mark: a wrong guess could silently turn a public
			// KV into an ephemeral one or the reverse.
			resp.Diagnostics.AddError(
				"Missing ephemeral mark in mgcl secret response",
				fmt.Sprintf("The provider API returned a value for key %q in secret %q without its ephemeral mark; "+
					"refusing to guess whether the KV is public or ephemeral.", key, name),
			)

			return
		}

		synced[key] = secretContentModel{
			Value:         types.StringValue(value),
			EphemeralMark: types.BoolValue(mark),
		}
	}

	setManagedContents(ctx, &state, synced, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Delete removes the KVs recorded in state in one batch. It never deletes the
// Secret, its DEK or KVs that are not in state. A missing Secret or KV is
// idempotent and succeeds with a 200 response from the batch endpoint; an HTTP
// 404 means the endpoint is unavailable (for example an older mgcl), so it is
// reported as an error rather than silently dropping state.
func (r *secretResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state managedSecretModel

	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name, ok := r.validName(ctx, state.Name, &resp.Diagnostics)
	if !ok {
		return
	}

	entries, ok := contentsFromValue(ctx, state.Contents, &resp.Diagnostics)
	if !ok {
		return
	}

	deleteKeys := sortedContentKeys(entries)
	if len(deleteKeys) == 0 {
		// Nothing is owned, so there is nothing to delete. Skipping the request
		// also guarantees an empty Secret is never created.
		return
	}

	client, token, ok := r.client(ctx, &resp.Diagnostics)
	if !ok {
		return
	}

	result, err := client.WriteSecretKVs(ctx, name, nil, deleteKeys, false)
	if err != nil {
		resp.Diagnostics.AddError(
			"Failed to delete mgcl secret KVs",
			fmt.Sprintf("Could not delete keys from secret %q: %s%s",
				name, redactToken(err.Error(), token), writeBatchAPIHint(err)),
		)

		return
	}

	if result == nil {
		resp.Diagnostics.AddError(
			"Unexpected mgcl secret response",
			"The provider API returned an empty response; the KV deletes were not confirmed.",
		)

		return
	}

	if result.Name != name {
		resp.Diagnostics.AddError(
			"Unexpected mgcl secret response",
			redactToken(fmt.Sprintf("The provider API confirmed secret %q while %q was requested; "+
				"the deletion of KVs was not confirmed.", result.Name, name), token),
		)

		return
	}
}

// write sends the planned contents and optional delete keys in one atomic
// request. It never adopts a response value; the response contains metadata
// only. On success it returns the planned entries with their concrete marks,
// normalized from the response.
func (r *secretResource) write(
	ctx context.Context,
	plan *managedSecretModel,
	deleteKeys []string,
	diags *diag.Diagnostics,
) (map[string]secretContentModel, bool) {
	name, ok := r.validName(ctx, plan.Name, diags)
	if !ok {
		return nil, false
	}

	entries, ok := plannedContents(ctx, plan.Contents, diags)
	if !ok {
		return nil, false
	}

	keys := sortedContentKeys(entries)
	writes := make([]providerapi.SecretKVWriteItem, 0, len(keys))

	for _, key := range keys {
		mark := effectiveEphemeralMark(entries[key].EphemeralMark)
		writes = append(writes, providerapi.SecretKVWriteItem{
			Key:         key,
			Value:       entries[key].Value.ValueString(),
			IsEphemeral: &mark,
		})
	}

	client, token, ok := r.client(ctx, diags)
	if !ok {
		return nil, false
	}

	result, err := client.WriteSecretKVs(ctx, name, writes, deleteKeys, true)
	if err != nil {
		diags.AddError(
			"Failed to write mgcl secret",
			fmt.Sprintf("Could not write %d keys to secret %q: %s%s",
				len(writes), name, redactToken(err.Error(), token), writeBatchAPIHint(err)),
		)

		return nil, false
	}

	marks, err := validateBatchWriteResponse(name, result, writes)
	if err != nil {
		diags.AddError(
			"Unexpected mgcl secret response",
			redactToken(err.Error(), token),
		)

		return nil, false
	}

	normalized := make(map[string]secretContentModel, len(entries))
	for _, key := range keys {
		normalized[key] = secretContentModel{
			Value:         entries[key].Value,
			EphemeralMark: types.BoolValue(marks[key]),
		}
	}

	return normalized, true
}

// validateBatchWriteResponse checks that the response echoes exactly the
// requested Secret name and every upserted key with its requested mark. It
// never inspects or adopts a value (the response carries none).
func validateBatchWriteResponse(
	requestedName string,
	result *providerapi.WriteSecretKVsResponse,
	writes []providerapi.SecretKVWriteItem,
) (map[string]bool, error) {
	if result == nil {
		return nil, fmt.Errorf("The provider API returned an empty response; the KV write was not confirmed.")
	}

	if result.Name != requestedName {
		return nil, fmt.Errorf("The provider API confirmed secret %q while %q was requested; "+
			"the response metadata did not match the request.", result.Name, requestedName)
	}

	if len(result.KVs) != len(writes) {
		return nil, fmt.Errorf("The provider API confirmed %d keys while %d were written; "+
			"the response metadata did not match the request.", len(result.KVs), len(writes))
	}

	requested := make(map[string]bool, len(writes))
	for _, write := range writes {
		mark := true
		if write.IsEphemeral != nil {
			mark = *write.IsEphemeral
		}

		requested[write.Key] = mark
	}

	marks := make(map[string]bool, len(result.KVs))

	for _, kv := range result.KVs {
		wantMark, ok := requested[kv.Key]
		if !ok {
			return nil, fmt.Errorf("The provider API confirmed key %q which was not written; "+
				"the response metadata did not match the request.", kv.Key)
		}

		if _, duplicate := marks[kv.Key]; duplicate {
			return nil, fmt.Errorf("The provider API confirmed key %q more than once; "+
				"the response metadata did not match the request.", kv.Key)
		}

		if kv.IsEphemeral != wantMark {
			return nil, fmt.Errorf("The provider API confirmed key %q with ephemeral mark %t while %t was written; "+
				"the response metadata did not match the request.", kv.Key, kv.IsEphemeral, wantMark)
		}

		marks[kv.Key] = kv.IsEphemeral
	}

	for key := range requested {
		if _, ok := marks[key]; !ok {
			return nil, fmt.Errorf("The provider API did not confirm key %q; "+
				"the response metadata did not match the request.", key)
		}
	}

	return marks, nil
}

// writeBatchAPIHint adds an actionable hint to a generic batch-write error.
// It performs no version probing: 400/404 only suggest that the running mgcl
// may predate the batch endpoint or hierarchical Secret names.
func writeBatchAPIHint(err error) string {
	switch providerAPIStatus(err) {
	case 400, 404:
		return " This operation requires an mgcl version that supports secret-kvs:write-batch and hierarchical Secret names."
	}

	return ""
}

// plannedContents decodes a plan's contents map, rejecting unknown, null and
// malformed entries. An empty map is valid.
func plannedContents(ctx context.Context, contents types.Map, diags *diag.Diagnostics) (map[string]secretContentModel, bool) {
	if contents.IsNull() {
		diags.AddAttributeError(
			path.Root("contents"),
			"Secret contents are required",
			"`contents` must be a map of KV entries; use an empty map `{}` to manage a Secret without any KVs.",
		)

		return nil, false
	}

	if contents.IsUnknown() {
		diags.AddAttributeError(
			path.Root("contents"),
			"Secret contents must be known",
			"`contents` must be known before the Secret can be written. Every key and value must be known during apply.",
		)

		return nil, false
	}

	entries, ok := contentsFromValue(ctx, contents, diags)
	if !ok {
		return nil, false
	}

	for _, key := range sortedContentKeys(entries) {
		if err := validateSecretKey(key); err != nil {
			diags.AddAttributeError(path.Root("contents").AtMapKey(key), "Invalid secret key", err.Error())
		}

		entry := entries[key]

		if entry.Value.IsNull() {
			diags.AddAttributeError(
				path.Root("contents").AtMapKey(key).AtName("value"),
				"Secret value must not be null",
				"Every contents entry requires a string value; use an empty string for an empty value.",
			)
		}

		if entry.Value.IsUnknown() {
			diags.AddAttributeError(
				path.Root("contents").AtMapKey(key).AtName("value"),
				"Secret value must be known",
				"The value must be a known string before it can be written.",
			)
		}
	}

	if diags.HasError() {
		return nil, false
	}

	validateContentsLimits(entries, diags)
	if diags.HasError() {
		return nil, false
	}

	return entries, true
}

// validateContentsLimits enforces the server's KV count and value size limits
// on the plan before any request is made. Only planned values are counted;
// unknown values are skipped here because Create/Update rejects them
// separately, and the server still validates the final KV set.
func validateContentsLimits(entries map[string]secretContentModel, diags *diag.Diagnostics) {
	if len(entries) > maxSecretKVs {
		diags.AddAttributeError(
			path.Root("contents"),
			"Too many Secret contents entries",
			fmt.Sprintf("mgcl allows at most %d KVs per Secret; this plan has %d entries.", maxSecretKVs, len(entries)),
		)
	}

	total := 0

	for _, key := range sortedContentKeys(entries) {
		value := entries[key].Value
		if value.IsNull() || value.IsUnknown() {
			continue
		}

		size := len(value.ValueString())
		total += size

		if size > maxSecretValueBytes {
			diags.AddAttributeError(
				path.Root("contents").AtMapKey(key).AtName("value"),
				"Secret value exceeds size limit",
				fmt.Sprintf("The value for key %q is %d bytes; mgcl allows at most %d bytes per value.",
					key, size, maxSecretValueBytes),
			)
		}
	}

	if total > maxSecretTotalBytes {
		diags.AddAttributeError(
			path.Root("contents"),
			"Secret contents exceed size limit",
			fmt.Sprintf("The planned values total %d bytes; mgcl allows at most %d bytes per Secret.",
				total, maxSecretTotalBytes),
		)
	}
}

// contentsFromValue decodes a known map into entries without validating entry
// contents. A null or unknown map, and a null or unknown entry object, are
// rejected with a safe diagnostic instead of a framework conversion error.
func contentsFromValue(ctx context.Context, contents types.Map, diags *diag.Diagnostics) (map[string]secretContentModel, bool) {
	if contents.IsNull() || contents.IsUnknown() {
		diags.AddAttributeError(
			path.Root("contents"),
			"Secret contents must be known",
			"`contents` must be a known map of KV entries; use an empty map `{}` for no KVs.",
		)

		return nil, false
	}

	entries := map[string]secretContentModel{}

	for key, element := range contents.Elements() {
		if element.IsNull() || element.IsUnknown() {
			diags.AddAttributeError(
				path.Root("contents").AtMapKey(key),
				"Secret contents must be known",
				fmt.Sprintf("The contents entry for key %q is null or unknown; every entry must be a known object with a known value.", key),
			)

			continue
		}

		object, ok := element.(types.Object)
		if !ok {
			diags.AddAttributeError(
				path.Root("contents").AtMapKey(key),
				"Secret contents must be known",
				fmt.Sprintf("The contents entry for key %q is not a known object; every entry must be a known object with a known value.", key),
			)

			continue
		}

		var entry secretContentModel

		if d := object.As(ctx, &entry, basetypes.ObjectAsOptions{}); d.HasError() {
			diags.Append(d...)

			continue
		}

		entries[key] = entry
	}

	if diags.HasError() {
		return nil, false
	}

	return entries, true
}

// setManagedContents stores entries as a concrete contents map.
func setManagedContents(ctx context.Context, model *managedSecretModel, entries map[string]secretContentModel, diags *diag.Diagnostics) {
	if entries == nil {
		entries = map[string]secretContentModel{}
	}

	value, d := types.MapValueFrom(ctx, secretContentObjectType, entries)
	diags.Append(d...)
	if diags.HasError() {
		return
	}

	model.Contents = value
}

// keysRemovedFromState returns the sorted keys present in state but absent from
// the plan. They are deleted from the Secret in the same Update request.
func keysRemovedFromState(ctx context.Context, state, plan types.Map, diags *diag.Diagnostics) []string {
	oldEntries, ok := contentsFromValue(ctx, state, diags)
	if !ok {
		return nil
	}

	newEntries, ok := contentsFromValue(ctx, plan, diags)
	if !ok {
		return nil
	}

	removed := make([]string, 0, len(oldEntries))
	for key := range oldEntries {
		if _, kept := newEntries[key]; !kept {
			removed = append(removed, key)
		}
	}

	sort.Strings(removed)

	return removed
}

func sortedContentKeys(entries map[string]secretContentModel) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

// validName validates a concrete Secret name attribute.
func (r *secretResource) validName(_ context.Context, value types.String, diags *diag.Diagnostics) (string, bool) {
	if value.IsNull() {
		diags.AddAttributeError(path.Root("name"), "Secret name is required", "The name must be set to a non-null string.")
		return "", false
	}

	if value.IsUnknown() {
		diags.AddAttributeError(
			path.Root("name"),
			"Secret name must be known",
			"The name of an mgcl_secret must be known before it can be written.",
		)

		return "", false
	}

	name := value.ValueString()

	if err := validateSecretName(name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid secret name", err.Error())
		return "", false
	}

	return name, true
}

// client returns the configured API client and token. The token is used only
// for redaction.
func (r *secretResource) client(_ context.Context, diags *diag.Diagnostics) (*providerapi.Client, string, bool) {
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
