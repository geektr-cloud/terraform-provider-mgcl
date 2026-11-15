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

// parseSecretReference parses exactly <secret-name>/<key> or
// <secret-name>/*. Other formats, multiple slashes, invalid names and invalid
// keys are rejected before any request is made.
func parseSecretReference(reference string) (secretReference, error) {
	if strings.Count(reference, "/") != 1 {
		return secretReference{}, fmt.Errorf(
			"secret reference %q must be <secret-name>/<key> or <secret-name>/*", reference,
		)
	}

	name, key, _ := strings.Cut(reference, "/")
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

// NewSecretValueFunction returns the provider::mgcl::secret_value function.
// The function resolves provider configuration through the provider instance
// because this framework version has no function-specific Configure hook.
func NewSecretValueFunction(p *mgclProvider) function.Function {
	return &secretValueFunction{provider: p}
}

type secretValueFunction struct {
	provider *mgclProvider
}

var _ function.Function = (*secretValueFunction)(nil)

func (f *secretValueFunction) Metadata(_ context.Context, _ function.MetadataRequest, resp *function.MetadataResponse) {
	resp.Name = "secret_value"
}

func (f *secretValueFunction) Definition(_ context.Context, _ function.DefinitionRequest, resp *function.DefinitionResponse) {
	resp.Definition = function.Definition{
		Summary: "Read one public KV, or every public KV, from an mgcl secret.",
		Description: "Reads public KVs through the mgcl provider API. " +
			"<secret-name>/<key> returns a string; <secret-name>/* returns a map of every public KV. " +
			"The result is ordinary, non-sensitive data: OpenTofu may persist it in plan files, state and logs. " +
			"Do not use this function for ephemeral KVs; use the mgcl_secret ephemeral resource instead. " +
			"Public KVs are still encrypted at rest in mgcl.db, but the function result is not secret.",
		MarkdownDescription: "Reads public KVs from an mgcl Secret through the `mgcl run` provider API.\n\n" +
			"- `provider::mgcl::secret_value(\"app-config/region\")` returns the string value of one public KV.\n" +
			"- `provider::mgcl::secret_value(\"app-config/*\")` returns a `map(string)` of every public KV in that Secret. " +
			"An empty public set returns an empty map.\n\n" +
			"> **Warning:** the result is normal, non-sensitive data. OpenTofu can persist it in plan files, state and " +
			"> logs. Use this function only for KVs explicitly marked public with " +
			"> `mgcl secret set <name>/<key> --public`. For ephemeral KVs, use the `mgcl_secret` ephemeral resource.\n\n" +
			"Public KV values are encrypted at rest in `mgcl.db`; this function does not make the result confidential " +
			"once OpenTofu has evaluated it.",
		Parameters: []function.Parameter{
			function.StringParameter{
				Name:                "reference",
				MarkdownDescription: "`<secret-name>/<key>` for one public KV, or `<secret-name>/*` for all public KVs. Names and keys must match `^[a-z0-9][a-z0-9_-]{0,63}$`.",
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
		resp.Error = function.NewArgumentFuncError(0, "Secret reference must be known before provider::mgcl::secret_value runs.")
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

	values, err := data.readPublicValues(ctx, ref.Name)
	if err != nil {
		resp.Error = publicSecretReadError(ref, err, token)
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
		resp.Error = function.NewArgumentFuncError(0, fmt.Sprintf(
			"Secret key %q was not found among the public KVs of mgcl secret %q.", ref.Key, ref.Name,
		))
		return
	}

	resp.Error = resp.Result.Set(ctx, types.DynamicValue(types.StringValue(value)))
}

// publicSecretReadError converts a client error into a safe function
// diagnostic. Secret values are never included. The provider token is redacted
// as a safety net against misbehaving endpoints or wrapped errors.
func publicSecretReadError(ref secretReference, err error, token string) *function.FuncError {
	var nameErr *unexpectedSecretNameError
	if errors.As(err, &nameErr) {
		return function.NewFuncError(fmt.Sprintf(
			"Unexpected mgcl secret response: %s; the returned values were discarded.",
			redactToken(nameErr.Error(), token),
		))
	}

	var markErr *invalidPublicReadError
	if errors.As(err, &markErr) {
		return function.NewFuncError(fmt.Sprintf(
			"Unexpected mgcl secret response: %s; the response did not satisfy the per-KV public mark "+
				"contract, so its values were discarded.",
			redactToken(markErr.Error(), token),
		))
	}

	switch providerAPIStatus(err) {
	case 404:
		return function.NewFuncError(fmt.Sprintf(
			"mgcl secret %q was not found; its public KV set could not be read. Create the Secret and mark "+
				"KVs public with `mgcl secret set <name>/<key> --public`, or use the mgcl_secret ephemeral "+
				"resource for ephemeral KVs.",
			ref.Name,
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
