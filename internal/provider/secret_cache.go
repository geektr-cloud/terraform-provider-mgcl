package provider

import (
	"context"
	"fmt"
	"sort"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
)

// unexpectedSecretNameError reports a response whose name does not match the
// request. Values from such a response are never cached or returned.
type unexpectedSecretNameError struct {
	requested string
	returned  string
}

func (e *unexpectedSecretNameError) Error() string {
	return fmt.Sprintf("provider API returned secret %q while %q was requested", e.returned, e.requested)
}

// invalidPublicReadError reports a public-read response that violates the
// per-KV mark contract: every returned KV must carry an explicit false
// ephemeral mark. Values from such a response are never cached or returned.
// Only the Secret name and KV key, which mgcl treats as public metadata, appear
// in the message.
type invalidPublicReadError struct {
	secret  string
	key     string
	missing bool
}

func (e *invalidPublicReadError) Error() string {
	if e.missing {
		return fmt.Sprintf("provider API returned key %q of secret %q without an ephemeral mark", e.key, e.secret)
	}

	return fmt.Sprintf("provider API returned ephemeral-marked key %q in a public read of secret %q", e.key, e.secret)
}

// validatePublicRead verifies the per-KV contract of a public read: every
// returned key carries an explicit false ephemeral mark. Values and marks that
// are both absent are a valid empty public set, not an error.
func validatePublicRead(secret string, values map[string]string, marks map[string]bool) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		mark, ok := marks[key]
		if !ok {
			return &invalidPublicReadError{secret: secret, key: key, missing: true}
		}

		if mark {
			return &invalidPublicReadError{secret: secret, key: key}
		}
	}

	return nil
}

// readPublicValues returns the public KVs of one Secret, caching the complete
// public map per Secret name in the provider configuration.
//
// The cache lives in providerData and therefore follows one provider
// configuration and its process lifetime. It never stores failed reads or
// responses for a different Secret, and it returns copies so callers cannot
// mutate cached values. The mutex is held across the HTTP read: concurrent
// misses for the same Secret are serialized, which guarantees that only one
// request is made. Different Secrets also serialize while they are fetched.
//
// Deliberately no cache invalidation is triggered by mgcl_secret_kv or managed
// mgcl_secret writes: the provider does not promise that a read after a write
// in the same OpenTofu project observes the new value. Use separate projects
// for writing and reading, as documented.
func (d *providerData) readPublicValues(ctx context.Context, name string) (map[string]string, error) {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()

	if values, ok := d.publicValues[name]; ok {
		return cloneSecretValues(values), nil
	}

	endpoint, token := d.credentials()
	client := &providerapi.Client{
		Endpoint: endpoint,
		Token:    token,
	}

	result, err := client.ReadPublicSecret(ctx, name)
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, fmt.Errorf("provider API returned an empty response")
	}

	if result.Name != name {
		// Never cache or return values from a response for a different secret.
		// Only names, which mgcl treats as public metadata, appear in the
		// error.
		return nil, &unexpectedSecretNameError{requested: name, returned: result.Name}
	}

	// A public read must return only KVs with an explicit false mark. Reject a
	// response that omits marks or includes an ephemeral KV instead of silently
	// trusting or filtering it.
	if err := validatePublicRead(name, result.Values, result.EphemeralMarks); err != nil {
		return nil, err
	}

	values := cloneSecretValues(result.Values)
	if d.publicValues == nil {
		d.publicValues = map[string]map[string]string{}
	}
	d.publicValues[name] = values

	return cloneSecretValues(values), nil
}

// cloneSecretValues normalizes a nil map to an empty non-nil map and copies the
// contents.
func cloneSecretValues(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}

	return out
}
