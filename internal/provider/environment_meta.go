package provider

import (
	"errors"
	"os"
	"strings"
)

// MetaFromEnvironment builds the same client the provider would, from the
// environment alone, as a provider block with nothing in it would.
//
// It's for the test sweepers, which have to reach the API outside any
// Terraform run and should do it through the provider's own transport, with
// its retries, its redirect refusal and its logging, rather than a second
// client that drifts from it. Endpoint, credential and poll-floor validation
// are the provider's too, so a sweeper can't be pointed somewhere `terraform
// plan` would have refused.
func MetaFromEnvironment(userAgent string) (*Meta, error) {
	settings, diags := resolveSettings(blockValues{}, os.Getenv)
	if diags.HasError() {
		var problems []string
		for _, d := range diags.Errors() {
			problems = append(problems, d.Summary()+": "+d.Detail())
		}
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	return newMeta(settings, userAgent)
}
