package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// secretReference is a parsed <secret-name>/<key> reference. AllKeys is true for
// the wildcard form <secret-name>/*.
type secretReference struct {
	Name    string
	Key     string
	AllKeys bool
}

// parseSecretReference parses <secret-name>/<key> or <secret-name>/*, splitting
// at the last slash. Secret names may contain multiple slash-separated
// segments; the key is always the final, single segment. Invalid names and keys
// are rejected before any request is made.
func parseSecretReference(reference string) (secretReference, error) {
	index := strings.LastIndex(reference, "/")
	if index < 0 {
		return secretReference{}, fmt.Errorf(
			"secret reference %q must be <secret-name>/<key> or <secret-name>/*", reference,
		)
	}

	name, key := reference[:index], reference[index+1:]
	if err := validateSecretName(name); err != nil {
		return secretReference{}, err
	}

	if key == "*" {
		return secretReference{Name: name, AllKeys: true}, nil
	}

	if err := validateSecretKey(key); err != nil {
		return secretReference{}, err
	}

	return secretReference{Name: name, Key: key}, nil
}

const (
	secretValueFunctionName          = "secret_value"
	secretEphemeralValueFunctionName = "i_just_fucking_wanna_read_ephemeral"
)

// NewSecretValueFunction returns the provider::mgcl::secret_value function.
// The function resolves provider configuration through the provider instance
// because this framework version has no function-specific Configure hook.
func NewSecretValueFunction(p *mgclProvider) function.Function {
	return &secretValueFunction{provider: p, name: secretValueFunctionName}
}

// NewSecretValueEphemeralFunction returns
// provider::mgcl::i_just_fucking_wanna_read_ephemeral. It shares the parser,
// dynamic return and error contract of secret_value but reads every KV through
// an access=ephemeral request. Its result is an ordinary dynamic value; the
// "ephemeral" in the name does not give it OpenTofu ephemeral semantics.
func NewSecretValueEphemeralFunction(p *mgclProvider) function.Function {
	return &secretValueFunction{
		provider: p,
		name:     secretEphemeralValueFunctionName,
		all:      true,
	}
}

type secretValueFunction struct {
	provider *mgclProvider
	name     string
	all      bool
}

var _ function.Function = (*secretValueFunction)(nil)

func (f *secretValueFunction) Metadata(_ context.Context, _ function.MetadataRequest, resp *function.MetadataResponse) {
	resp.Name = f.name
}

func (f *secretValueFunction) Definition(_ context.Context, _ function.DefinitionRequest, resp *function.DefinitionResponse) {
	if f.all {
		resp.Definition = function.Definition{
			Summary: "Read one KV, or every KV including ephemeral KVs, from an mgcl secret.",
			Description: "Reads public and ephemeral KVs through the mgcl provider API. " +
				"<secret-name>/<key> returns a string; <secret-name>/* returns a map of every KV. " +
				"Secret names may contain /-separated segments; the key is the final single segment. " +
				"The result is ordinary, non-sensitive data: OpenTofu may persist it in plan files, state and logs. " +
				"The word ephemeral in this function's name does not grant OpenTofu ephemeral semantics; use the " +
				"mgcl_secret ephemeral resource when a value must never be persisted. " +
				"KVs are still encrypted at rest in mgcl.db, but the function result is not secret.",
			MarkdownDescription: "Reads every KV, public or ephemeral, from an mgcl Secret through the `mgcl run` " +
				"provider API.\n\n" +
				"- `provider::mgcl::" + secretEphemeralValueFunctionName + "(\"app-config/region\")` returns the string " +
				"value of one KV.\n" +
				"- `provider::mgcl::" + secretEphemeralValueFunctionName + "(\"team/app-config/*\")` returns a " +
				"`map(string)` of every KV in that Secret. An empty Secret returns an empty map.\n\n" +
				"Secret names may contain multiple `/`-separated segments; the reference is split at the last `/` and " +
				"the key is always a single segment without `/`.\n\n" +
				"> **Warning:** the result is normal, non-sensitive data, even for ephemeral KVs. OpenTofu can persist " +
				"it in plan files, state and logs. The `ephemeral` in this function's name is only a name: it does not " +
				"give the result OpenTofu ephemeral semantics. Use the `mgcl_secret` ephemeral resource when a value " +
				"must never be persisted.\n\n" +
				"KV values are encrypted at rest in `mgcl.db`; this function does not make the result confidential " +
				"once OpenTofu has evaluated it.",
			Parameters: []function.Parameter{
				function.StringParameter{
					Name: "reference",
					MarkdownDescription: "`<secret-name>/<key>` for one KV, or `<secret-name>/*` for every KV including " +
						"ephemeral KVs. The name is " + secretNameSchemaDescription + " The key is " + secretKeySchemaDescription,
				},
			},
			Return: function.DynamicReturn{},
		}

		return
	}

	resp.Definition = function.Definition{
		Summary: "Read one public KV, or every public KV, from an mgcl secret.",
		Description: "Reads public KVs through the mgcl provider API. " +
			"<secret-name>/<key> returns a string; <secret-name>/* returns a map of every public KV. " +
			"Secret names may contain /-separated segments; the key is the final single segment. " +
			"The result is ordinary, non-sensitive data: OpenTofu may persist it in plan files, state and logs. " +
			"Do not use this function for ephemeral KVs; use the mgcl_secret ephemeral resource instead. " +
			"Public KVs are still encrypted at rest in mgcl.db, but the function result is not secret.",
		MarkdownDescription: "Reads public KVs from an mgcl Secret through the `mgcl run` provider API.\n\n" +
			"- `provider::mgcl::secret_value(\"app-config/region\")` returns the string value of one public KV.\n" +
			"- `provider::mgcl::secret_value(\"team/app-config/*\")` returns a `map(string)` of every public KV in that " +
			"Secret. An empty public set returns an empty map.\n\n" +
			"Secret names may contain multiple `/`-separated segments; the reference is split at the last `/` and the " +
			"key is always a single segment without `/`.\n\n" +
			"> **Warning:** the result is normal, non-sensitive data. OpenTofu can persist it in plan files, state and " +
			"logs. Use this function only for KVs explicitly marked public with " +
			"`mgcl secret set <name>/<key> --public`. For ephemeral KVs, use the `mgcl_secret` ephemeral resource.\n\n" +
			"Public KV values are encrypted at rest in `mgcl.db`; this function does not make the result confidential " +
			"once OpenTofu has evaluated it.",
		Parameters: []function.Parameter{
			function.StringParameter{
				Name: "reference",
				MarkdownDescription: "`<secret-name>/<key>` for one public KV, or `<secret-name>/*` for all public KVs. The name is " +
					secretNameSchemaDescription + " The key is " + secretKeySchemaDescription,
			},
		},
		Return: function.DynamicReturn{},
	}
}

func (f *secretValueFunction) Run(ctx context.Context, req function.RunRequest, resp *function.RunResponse) {
	var reference types.String

	if funcErr := req.Arguments.Get(ctx, &reference); funcErr != nil {
		resp.Error = funcErr
		return
	}

	if reference.IsNull() {
		resp.Error = function.NewArgumentFuncError(0, "Secret reference is required and must be a non-null string.")
		return
	}

	if reference.IsUnknown() {
		resp.Error = function.NewArgumentFuncError(0,
			"Secret reference must be known before provider::mgcl::"+f.name+" runs.")
		return
	}

	ref, err := parseSecretReference(reference.ValueString())
	if err != nil {
		resp.Error = function.NewArgumentFuncError(0, "Invalid secret reference: "+err.Error())
		return
	}

	data := (*providerData)(nil)
	if f.provider != nil {
		data = f.provider.providerData()
	}

	if data == nil {
		resp.Error = function.NewFuncError(fmt.Sprintf(
			"mgcl provider is not configured: %[1]s is not set. The mgcl provider only works inside "+
				"`mgcl run -- <command>`, which injects %[1]s and %[2]s into the child process environment.",
			providerapi.EnvEndpoint, providerapi.EnvToken,
		))
		return
	}

	endpoint, token := data.credentials()
	if endpoint == "" {
		resp.Error = function.NewFuncError(fmt.Sprintf(
			"mgcl provider is not configured: %[1]s is not set. The mgcl provider only works inside "+
				"`mgcl run -- <command>`, which injects %[1]s and %[2]s into the child process environment.",
			providerapi.EnvEndpoint, providerapi.EnvToken,
		))
		return
	}

	if token == "" {
		resp.Error = function.NewFuncError(fmt.Sprintf(
			"mgcl provider token is missing: %s is not set. The mgcl provider only works inside "+
				"`mgcl run -- <command>`, which provides a short-lived provider token.",
			providerapi.EnvToken,
		))
		return
	}

	var values map[string]string

	if f.all {
		values, err = data.readAllValues(ctx, ref.Name)
	} else {
		values, err = data.readPublicValues(ctx, ref.Name)
	}

	if err != nil {
		resp.Error = secretReadError(ref, err, token, f.all)
		return
	}

	if ref.AllKeys {
		mapValue, diags := types.MapValueFrom(ctx, types.StringType, values)
		if diags.HasError() {
			resp.Error = function.FuncErrorFromDiags(ctx, diags)
			return
		}

		resp.Error = resp.Result.Set(ctx, types.DynamicValue(mapValue))
		return
	}

	value, ok := values[ref.Key]
	if !ok {
		if f.all {
			resp.Error = function.NewArgumentFuncError(0, fmt.Sprintf(
				"Secret key %q was not found in mgcl secret %q.", ref.Key, ref.Name,
			))
		} else {
			resp.Error = function.NewArgumentFuncError(0, fmt.Sprintf(
				"Secret key %q was not found among the public KVs of mgcl secret %q.", ref.Key, ref.Name,
			))
		}

		return
	}

	resp.Error = resp.Result.Set(ctx, types.DynamicValue(types.StringValue(value)))
}

// secretReadError converts a client error into a safe function diagnostic.
// Secret values are never included. The provider token is redacted as a safety
// net against misbehaving endpoints or wrapped errors. In all-KV mode the
// wording never claims that only public KVs were read.
func secretReadError(ref secretReference, err error, token string, all bool) *function.FuncError {
	var nameErr *unexpectedSecretNameError
	if errors.As(err, &nameErr) {
		return function.NewFuncError(fmt.Sprintf(
			"Unexpected mgcl secret response: %s; the returned values were discarded.",
			redactToken(nameErr.Error(), token),
		))
	}

	if all {
		var markErr *invalidAllReadError
		if errors.As(err, &markErr) {
			return function.NewFuncError(fmt.Sprintf(
				"Unexpected mgcl secret response: %s; the response did not satisfy the per-KV mark "+
					"contract, so its values were discarded.",
				redactToken(markErr.Error(), token),
			))
		}
	} else {
		var markErr *invalidPublicReadError
		if errors.As(err, &markErr) {
			return function.NewFuncError(fmt.Sprintf(
				"Unexpected mgcl secret response: %s; the response did not satisfy the per-KV public mark "+
					"contract, so its values were discarded.",
				redactToken(markErr.Error(), token),
			))
		}
	}

	switch providerAPIStatus(err) {
	case 404:
		if all {
			return function.NewFuncError(fmt.Sprintf(
				"mgcl secret %q was not found; its KV set could not be read. Create the Secret with "+
					"`mgcl secret create <name>` before reading it.",
				ref.Name,
			))
		}

		return function.NewFuncError(fmt.Sprintf(
			"mgcl secret %q was not found; its public KV set could not be read. Create the Secret and mark "+
				"KVs public with `mgcl secret set <name>/<key> --public`, or use the mgcl_secret ephemeral "+
				"resource for ephemeral KVs.",
			ref.Name,
		))
	}

	if all {
		return function.NewFuncError(fmt.Sprintf(
			"Failed to read KVs of mgcl secret %q: %s",
			ref.Name, redactToken(err.Error(), token),
		))
	}

	return function.NewFuncError(fmt.Sprintf(
		"Failed to read public KVs of mgcl secret %q: %s",
		ref.Name, redactToken(err.Error(), token),
	))
}

// providerAPIStatus extracts an HTTP status from a provider API error. The
// current client reports only the numeric status in its error text; the
// interface checks also accept a future typed error with a status accessor.
func providerAPIStatus(err error) int {
	type statusCoder interface {
		StatusCode() int
	}

	type httpStatusCoder interface {
		HTTPStatusCode() int
	}

	var coder statusCoder
	if errors.As(err, &coder) {
		return coder.StatusCode()
	}

	var httpCoder httpStatusCoder
	if errors.As(err, &httpCoder) {
		return httpCoder.HTTPStatusCode()
	}

	match := providerStatusPattern.FindStringSubmatch(err.Error())
	if match == nil {
		return 0
	}

	status, convErr := strconv.Atoi(match[1])
	if convErr != nil {
		return 0
	}

	return status
}

var providerStatusPattern = regexp.MustCompile(`\bstatus ([0-9]{3})\b`)
