// Package provider implements the k0rdent AI Terraform provider.
package provider

import "strings"

// Provider naming lives here. Every resource type, data source type,
// environment variable, binary and user agent derives from this constant.
// `make name-audit` makes the unavoidable non-Go references enumerable.

const (
	// ProviderTypeName is the name Terraform users write in a
	// `required_providers` block and the prefix on every resource and data
	// source this provider exposes.
	ProviderTypeName = "k0rdent"

	// resourceNameSeparator sits between the type name and a resource's own
	// suffix. Terraform requires that resource names begin with the provider
	// type name followed by an underscore.
	resourceNameSeparator = "_"

	// RegistryNamespace is the owner half of the registry source address:
	// registry.terraform.io/k0rdent/k0rdent.
	RegistryNamespace = "k0rdent"

	// registryHost is the public Terraform registry. It is not name-dependent
	// and is here only so RegistryAddress reads as one string.
	registryHost = "registry.terraform.io"
)

// RegistryAddress is the source address Terraform resolves this provider by,
// e.g. "registry.terraform.io/k0rdent/k0rdent". Both halves are derived.
//
// This address is what `dev_overrides` keys on during local development, and
// it is the one string that becomes permanent at first publish — the registry
// source cannot be renamed afterwards.
func RegistryAddress() string {
	return registryHost + "/" + RegistryNamespace + "/" + ProviderTypeName
}

// ResourcePrefix is the string every resource and data source name starts
// with, e.g. "k0rdent_". Derived, never written out.
func ResourcePrefix() string {
	return ProviderTypeName + resourceNameSeparator
}

// ResourceName builds a fully qualified resource or data source name from its
// suffix: ResourceName("kubernetes_cluster") -> "k0rdent_kubernetes_cluster".
//
// Every resource must build its name this way. A
// resource that spells the prefix out defeats the single-constant rule and
// turns the rename into a sweep.
func ResourceName(suffix string) string {
	return ResourcePrefix() + suffix
}

// EnvPrefix is the prefix on every environment variable this provider reads,
// e.g. "K0RDENT_". Derived from the same constant.
//
// Hyphens become underscores: `k0rdent-ai` is a legal Terraform provider type
// name but `K0RDENT-AI_ENDPOINT` is not a legal shell identifier, so a rename
// to a hyphenated name would otherwise produce variables nobody can export.
func EnvPrefix() string {
	return strings.ToUpper(strings.ReplaceAll(ProviderTypeName, "-", "_")) + "_"
}

// EnvVar builds a fully qualified environment variable name from its suffix:
// EnvVar("CLIENT_SECRET") -> "K0RDENT_CLIENT_SECRET".
func EnvVar(suffix string) string {
	return EnvPrefix() + suffix
}

// UserAgent is what the provider identifies itself as to the API, e.g.
// "Terraform/1.9.5 (+https://www.terraform.io) terraform-provider-k0rdent/0.1.0".
// The binary name and the type name are the same string by registry rule, so
// this derives too.
//
// The Terraform product comes first, in the form HashiCorp's own SDK writes
// it, so a gateway log reads the same as one for any other provider. It's left
// out when Terraform didn't say which version it is, as a test harness
// driving Configure directly doesn't.
//
// appended is the value of appendUserAgentEnv. It goes last, trimmed, the way
// the SDK adds it, so a pipeline that names itself there is named on this
// provider's requests as on every other's.
func UserAgent(version, terraformVersion, appended string) string {
	if version == "" {
		version = "dev"
	}
	ua := "terraform-provider-" + ProviderTypeName + "/" + version
	if terraformVersion != "" {
		ua = "Terraform/" + terraformVersion + " (+https://www.terraform.io) " + ua
	}
	if appended = strings.TrimSpace(appended); appended != "" {
		ua += " " + appended
	}
	return ua
}

// appendUserAgentEnv is the variable Terraform's tooling reads for product
// tokens to add to every provider's User-Agent.
const appendUserAgentEnv = "TF_APPEND_USER_AGENT"
