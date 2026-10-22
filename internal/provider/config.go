package provider

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// DefaultEndpoint is the API base URL used when neither the provider block nor
// the environment names one.
//
// It is the contract's declared server (the spec's `servers`). Pointing this
// provider at any other installation takes `endpoint`.
const DefaultEndpoint = "https://api.k0rdent.ai"

// globalRegion is the literal control-plane tier segment. Taken from the
// generated enum rather than written out, so it tracks the contract.
var globalRegion = string(k0rdent.RegionTypeGlobal)

// regionsOperation is how the region-discovery call is named in the messages
// that tell a user where valid region ids come from.
//
// It is a constant rather than four string literals for the reason in
// validateEndpoint: contract paths move, and this one has no compile-time tie
// to the generated client, so nothing would catch it going stale. One edit
// beats a grep. The provider never builds this path — it is help text.
const regionsOperation = "GET /v1/regions"

// exampleClusterTypeRef is an illustrative `clusterType` value, for the schema
// descriptions that have to show one. Here for the same reason as
// regionsOperation: it is documentation, not a path the provider builds, and one
// constant is easier to correct than several string literals. The region and
// ClusterType slug in it are both made up — real values come from
// `GET /v1/regions` and the cluster-type catalog.
const exampleClusterTypeRef = "/v1/regions/us-texas-1/compute/cluster-types/gpu-training"

// DefaultOperationTimeout and DefaultPollInterval bound the provider's waiters.
//
// The waiter lives here rather than in the client on purpose. ADR D4 says the
// client "never waits, polls, retries" — that is a rule about the client, not a
// claim that nothing needs to wait. A cluster create is asynchronous: the
// create returns `201` with `state: creating` and the resource is not usable
// until it settles. Terraform's contract is the opposite of the API's: when
// `apply` returns, the resource exists. Something has to bridge that, and the
// provider engine is the layer that knows which lifecycle it is in.
//
// Both are configurable, and both are estimates. The fake settles in under a
// second.
const (
	DefaultOperationTimeout = 2 * time.Hour
	// DefaultLongOperationTimeout is the default for a cluster's and an
	// instance group's create and delete, which build or free hardware.
	//
	// Building or freeing hardware has taken more than 2 hours, so these
	// wait up to 3h. A run that outlasts it loses nothing: the next apply or
	// destroy picks up the same operation (resume.go). Everything else keeps
	// 2h, so a network or a security group still fails within the hour or
	// two it would anyway.
	// `operation_timeout`, once set, applies to these as well.
	DefaultLongOperationTimeout = 3 * time.Hour
	DefaultPollInterval         = 10 * time.Second

	// MinPollInterval is the floor under poll_interval. See resolveSettings.
	MinPollInterval = time.Second

	// DefaultProvisionalFailureWindow is how long a cluster is allowed to sit
	// in `failed`, or a network to keep its state after a delete, before the
	// provider reports it. Long enough for a
	// watchdog-forced failure to be overtaken by a workflow that converges,
	// short enough that a real failure does not cost the whole timeout.
	DefaultProvisionalFailureWindow = 2 * time.Minute
)

// ProvisionalFailureWindow is the window every new provider waits. Only the
// acceptance suite lowers it, because its fake never recovers a failed object.
// It is not keyed on a loopback endpoint: a kind stack serves the real platform
// on localhost.
var ProvisionalFailureWindow = DefaultProvisionalFailureWindow

// Settings is the fully resolved provider configuration: every value that
// reaches the client, after the provider block, the environment and the
// defaults have been reconciled.
type Settings struct {
	// Endpoint is the API base URL.
	Endpoint string

	// Region is the `{region}` URL segment for regional resources. May be
	// empty; see the note on RequireRegion.
	Region string

	// Project is the `{project}` URL segment for project-scoped resources.
	// May be empty; see the note on RequireProject.
	Project string

	// OperationTimeout bounds how long a resource waits for an asynchronous
	// lifecycle change to settle. PollInterval is how often it re-reads while
	// waiting.
	OperationTimeout time.Duration
	PollInterval     time.Duration
	// LongOperationTimeout bounds a cluster's and an instance group's create
	// and delete: DefaultLongOperationTimeout, or OperationTimeout once the
	// provider block or the environment sets one.
	LongOperationTimeout time.Duration

	// Credential is which machine grant the provider will mint with, and the
	// secret material for it. It never comes from the provider block.
	Credential CredentialSource
}

// CredentialKind is which of the two machine grants a credential uses.
type CredentialKind string

const (
	// ServiceAccountCredential is the `client_credentials` grant. The
	// supported choice for a pipeline: the account is its own principal,
	// holds its own roles, and survives its creator being off-boarded.
	ServiceAccountCredential CredentialKind = "service account"

	// APIKeyCredential is the `api_key` grant. Supported because a tenant may
	// only be able to issue one, not because it is a good fit — it mints under
	// the creating user's identity and inherits that human's current roles.
	APIKeyCredential CredentialKind = "API key"
)

// CredentialSource is the resolved credential, kept as plain fields so the
// resolution logic can be tested without constructing a client.
type CredentialSource struct {
	Kind         CredentialKind
	ClientID     string
	ClientSecret string
	APIKey       string
}

// build turns the resolved source into the client's Credential.
//
// No scope narrowing is requested: it isn't supported, and the refresh grant
// is never passed one. Project targeting does not need it: `{project}` is a
// URL segment.
func (c CredentialSource) build() k0rdent.Credential {
	switch c.Kind {
	case APIKeyCredential:
		return k0rdent.NewAPIKeyCredential(c.APIKey, nil)
	case ServiceAccountCredential:
		return k0rdent.NewServiceAccountCredential(c.ClientID, c.ClientSecret, nil)
	default:
		// Unreachable: resolveSettings returns an error diagnostic rather
		// than a Settings with no credential kind.
		return nil
	}
}

// blockValues is the provider block after Terraform has decoded it, with
// null and unknown already handled by the caller.
type blockValues struct {
	Endpoint         string
	Region           string
	Project          string
	OperationTimeout string
	PollInterval     string
}

// Env var suffixes. The prefix is derived (names.go), so a rename moves these
// with everything else and no Go file spells the provisional name out.
const (
	envEndpoint         = "ENDPOINT"
	envRegion           = "REGION"
	envProject          = "PROJECT"
	envOperationTimeout = "OPERATION_TIMEOUT"
	envPollInterval     = "POLL_INTERVAL"
	envClientID         = "CLIENT_ID"
	envClientSecret     = "CLIENT_SECRET"
	envAPIKey           = "API_KEY"
)

// resolveSettings reconciles the provider block, the environment and the
// defaults into one Settings.
//
// # Precedence
//
// For the three non-secret attributes it is, highest first:
//
//  1. the attribute in the `provider` block
//  2. the environment variable
//  3. the built-in default, which only `endpoint` has
//
// A value that is empty or whitespace-only counts as absent at every layer, so
// `region = var.region` with an unset variable falls through to the
// environment rather than configuring the provider with "".
//
// # Secrets are environment-only
//
// There is no `client_secret` or `api_key` attribute, so there is no
// precedence to state for them: the environment is the only source. That is
// the "no credential in a file" requirement taken literally, and the
// reasoning is in the Schema doc comment.
//
// getenv is injected so the whole of this can be tested in parallel without
// mutating the process environment.
func resolveSettings(block blockValues, getenv func(string) string) (Settings, diag.Diagnostics) {
	var diags diag.Diagnostics

	env := func(suffix string) string { return strings.TrimSpace(getenv(EnvVar(suffix))) }

	s := Settings{
		Endpoint: firstNonEmpty(strings.TrimSpace(block.Endpoint), env(envEndpoint), DefaultEndpoint),
		Region:   firstNonEmpty(strings.TrimSpace(block.Region), env(envRegion)),
		Project:  firstNonEmpty(strings.TrimSpace(block.Project), env(envProject)),
	}

	diags.Append(validateEndpoint(s.Endpoint)...)
	diags.Append(validateRegion(s.Region)...)
	diags.Append(validateProject(s.Project)...)

	operationTimeout := firstNonEmpty(strings.TrimSpace(block.OperationTimeout), env(envOperationTimeout))
	s.OperationTimeout = resolveDuration(
		"operation_timeout", envOperationTimeout, operationTimeout, DefaultOperationTimeout, &diags,
	)
	s.LongOperationTimeout = DefaultLongOperationTimeout
	if operationTimeout != "" {
		s.LongOperationTimeout = s.OperationTimeout
	}
	s.PollInterval = resolveDuration(
		"poll_interval", envPollInterval,
		firstNonEmpty(strings.TrimSpace(block.PollInterval), env(envPollInterval)),
		DefaultPollInterval, &diags,
	)
	// A floor, not just "positive". `1ns` parses and is positive, and waitFor
	// has no backoff, so a tiny interval turns a long wait into back-to-back
	// GETs per waiting resource against finite hardware, until the gateway
	// answers 429 and the apply fails looking like a broken credential.
	// Loopback is exempt so the local fake can be polled fast.
	if s.PollInterval > 0 && s.PollInterval < MinPollInterval && !endpointIsLoopback(s.Endpoint) {
		diags.AddError(
			"Poll interval too short",
			fmt.Sprintf(
				"`poll_interval` is %s, and the minimum against a remote endpoint is %s.\n\n"+
					"The provider does not back off between polls, so a shorter interval is a "+
					"request flood rather than a faster answer. Set it with the `poll_interval` "+
					"attribute or the %s environment variable. A loopback endpoint, which is what "+
					"the local fake runs on, has no floor.",
				s.PollInterval, MinPollInterval, EnvVar(envPollInterval),
			),
		)
		s.PollInterval = DefaultPollInterval
	}

	cred, credDiags := resolveCredential(env)
	diags.Append(credDiags...)
	s.Credential = cred

	return s, diags
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolveDuration parses a Go duration string, falling back to a default when
// it is absent. An unparseable value is an error rather than a silent default:
// someone who writes `operation_timeout = "30"` meant something, and
// time.ParseDuration rejects it, so guessing minutes would be inventing intent.
func resolveDuration(attr, envSuffix, raw string, fallback time.Duration, diags *diag.Diagnostics) time.Duration {
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		diags.AddError(
			"Invalid duration",
			fmt.Sprintf(
				"`%s` is not a duration: %q.\n\n"+
					"Give a Go duration string with a unit, for example \"45m\", \"90s\" or \"1h30m\". "+
					"Set it with the `%s` attribute in the provider block or the %s environment "+
					"variable. The default is %s.",
				attr, raw, attr, EnvVar(envSuffix), fallback,
			),
		)
		return fallback
	}
	if d <= 0 {
		diags.AddError(
			"Invalid duration",
			fmt.Sprintf("`%s` must be positive; got %q.", attr, raw),
		)
		return fallback
	}
	return d
}

// apiVersionSegment matches a path segment that looks like an API version.
var apiVersionSegment = regexp.MustCompile(`^v[0-9]+$`)

// validateEndpoint checks that the endpoint is a base URL and nothing more.
//
// The interesting case is the last one. `endpoint` supplies an origin; every
// path, including the API version, comes from the generated client. Keeping
// that boundary sharp is not tidiness: over the single v1.0.1 to v1.2.0
// transition the contract removed nine public paths with no deprecation window
// — the whole `compute/networks` family relocated to `networking/networks` in
// one release. Anything in this repo that restates a path is
// a thing that silently stops matching. The provider restates none, and an
// endpoint carrying `/v1` would make the user restate one on its behalf.
func validateEndpoint(endpoint string) diag.Diagnostics {
	var diags diag.Diagnostics

	invalid := func(problem string) {
		diags.AddError(
			"Invalid API endpoint",
			fmt.Sprintf(
				"%s Got %q.\n\n"+
					"The endpoint is the API's base URL — scheme and host, for example %q. "+
					"Set it with the `endpoint` attribute in the provider block or the %s "+
					"environment variable.",
				problem, endpoint, DefaultEndpoint, EnvVar(envEndpoint),
			),
		)
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		invalid("The endpoint is not a URL.")
		return diags
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		invalid("The endpoint must be an absolute http:// or https:// URL.")
		return diags
	}
	if u.Host == "" {
		invalid("The endpoint has no host.")
		return diags
	}
	// http is allowed to a loopback address and nowhere else. The credential is
	// sent in the body of the token mint, so a cleartext endpoint puts the
	// client secret on the wire. This is CVE-2019-19316 exactly: Terraform sent
	// a SAS token and a whole state snapshot over cleartext HTTP because the
	// SDK took whatever scheme it was handed. The local fake needs http, and it
	// is always loopback.
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		diags.AddError(
			"The API endpoint is not encrypted",
			fmt.Sprintf(
				"%q is http, and the credential is sent in the body of the token request, so it "+
					"would go over the wire in cleartext.\n\n"+
					"Use https. http is accepted only for a loopback address, which is what the "+
					"local fake runs on.",
				endpoint,
			),
		)
		return diags
	}
	if u.RawQuery != "" || u.Fragment != "" {
		invalid("The endpoint must not carry a query string or fragment.")
		return diags
	}

	for _, segment := range strings.Split(u.Path, "/") {
		if !apiVersionSegment.MatchString(segment) {
			continue
		}
		diags.AddError(
			"API endpoint includes a version segment",
			fmt.Sprintf(
				"The endpoint %q contains the path segment %q, which looks like an API version.\n\n"+
					"Give the base URL only, for example %q. Operation paths — including the "+
					"version — come from the generated client, so a version here is sent twice and "+
					"every request 404s.\n\n"+
					"A base path that is not a version is fine, for a landscape served under one.",
				endpoint, segment, DefaultEndpoint,
			),
		)
		return diags
	}

	return diags
}

// endpointIsLoopback reports whether an endpoint points at this machine. It
// takes the whole URL because the caller has the URL, and a value that does not
// parse is treated as remote, which is the cautious direction.
func endpointIsLoopback(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

// isLoopbackHost reports whether a host is the local machine. The literal names
// are here because net.ParseIP does not resolve them and a DNS lookup during
// validation would be both slow and spoofable.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// pathSegment is the shape every URL path segment this provider builds has to
// have: the contract's own id pattern, anchored at both ends.
var pathSegment = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// validatePathSegment refuses a value that would edit the request URL rather
// than fill a slot in it, at plan time, so the error names the attribute that
// set it.
//
// The generated client percent-escapes a path parameter, so `/` cannot inject a
// segment. It does not escape `.`, and the builder resolves the path against
// the endpoint per RFC 3986, which removes dot segments. Measured against the
// oapi-codegen runtime before the client checked for it, region `us-west`
// built /v1/regions/us-west/networking/address-pools, `..` built
// /v1/networking/address-pools and `.` built /v1/regions/networking/address-pools.
//
// The client now refuses an empty value, `.` and `..` itself, as a
// *k0rdent.PathParamError, along with anything off the contract's own rule for
// the parameter, so an id read back from state or named in a platform answer
// can't reach a URL unchecked. This check is stricter, the contract's id shape
// for every segment, and it's here for the message: it says which attribute
// and what shape, before the plan is approved.
func validatePathSegment(what, value string) diag.Diagnostics {
	var diags diag.Diagnostics
	if value == "" || pathSegment.MatchString(value) {
		return diags
	}
	diags.AddError(
		fmt.Sprintf("Invalid %s", what),
		fmt.Sprintf(
			"`%s` is %q, which is not a valid identifier.\n\n"+
				"It has to match `[a-z]([-a-z0-9]*[a-z0-9])?`: lower-case letters, digits and "+
				"hyphens, starting with a letter. It becomes a segment of the request URL, and a "+
				"value carrying `.`, `..`, `/`, `?` or `#` rewrites that URL instead of filling "+
				"the segment.",
			what, value,
		),
	)
	return diags
}

// validateRegion checks the region is a usable URL segment.
//
// It used to refuse `global` as well, the literal segment of the control-plane
// tier, on the ground that it names no compute. But on the all-in-one install compute runs under
// `global`, and the region list reports `hostsProjects: true` for it there.
// Whether `global` is a region on a given installation is the platform's to
// say, so it's taken, and a project placed in it is checked against the region
// list at plan time (checkProjectRegion).
func validateRegion(region string) diag.Diagnostics {
	return validatePathSegment("region", region)
}

// validateProject checks the project is a usable URL segment. A generated
// project id starts `proj-`, so that prefix is an ordinary id.
func validateProject(project string) diag.Diagnostics {
	return validatePathSegment("project", project)
}

// resolveCredential picks the machine grant from the environment.
//
// Ambiguity is an error rather than a silent precedence rule. Two credentials
// in the environment means someone believes the provider is using one of them,
// and a fifty-fifty guess about which identity a `terraform apply` runs as is
// not a defensible default.
func resolveCredential(env func(string) string) (CredentialSource, diag.Diagnostics) {
	var diags diag.Diagnostics

	clientID := env(envClientID)
	clientSecret := env(envClientSecret)
	apiKey := env(envAPIKey)

	sa := clientID != "" || clientSecret != ""

	switch {
	case sa && apiKey != "":
		diags.AddError(
			"Ambiguous credentials",
			fmt.Sprintf(
				"Both a service account (%s / %s) and an API key (%s) are set in the environment, "+
					"and they are different identities.\n\n"+
					"Unset whichever one you did not mean to use. The service account is the "+
					"supported choice for a pipeline.",
				EnvVar(envClientID), EnvVar(envClientSecret), EnvVar(envAPIKey),
			),
		)
		return CredentialSource{}, diags

	case sa && clientID == "":
		diags.AddError(
			"Incomplete service account credential",
			fmt.Sprintf(
				"%s is set but %s is not. The `client_credentials` grant needs both.",
				EnvVar(envClientSecret), EnvVar(envClientID),
			),
		)
		return CredentialSource{}, diags

	case sa && clientSecret == "":
		diags.AddError(
			"Incomplete service account credential",
			fmt.Sprintf(
				"%s is set but %s is not. The `client_credentials` grant needs both.\n\n"+
					"The client secret is shown once, when the credential is issued, and cannot be "+
					"retrieved again. If it has been lost, a tenant administrator issues a new one — "+
					"a service account supports up to five concurrent credentials, so this does not "+
					"require downtime.",
				EnvVar(envClientID), EnvVar(envClientSecret),
			),
		)
		return CredentialSource{}, diags

	case sa:
		return CredentialSource{
			Kind:         ServiceAccountCredential,
			ClientID:     clientID,
			ClientSecret: clientSecret,
		}, diags

	case apiKey != "":
		diags.AddWarning(
			"Authenticating with an API key",
			fmt.Sprintf(
				"%s is set, so the provider will mint tokens with the `api_key` grant.\n\n"+
					"An API key mints under its creating user's identity: its privileges are the "+
					"intersection of the key's roles and that person's current roles, so it stops "+
					"working when they leave or their roles change.\n\n"+
					"Ask a tenant administrator for a service account instead, and set %s and %s.",
				EnvVar(envAPIKey), EnvVar(envClientID), EnvVar(envClientSecret),
			),
		)
		return CredentialSource{Kind: APIKeyCredential, APIKey: apiKey}, diags

	default:
		diags.AddError(
			"No credential",
			fmt.Sprintf(
				"The provider found no credential in the environment.\n\n"+
					"Set %s and %s for a service account, which is the supported choice.\n\n"+
					"There is no `client_secret` attribute and no credentials file: the secret is "+
					"read from the environment and nowhere else, so that it cannot be committed to a "+
					"repository or written into a saved plan.\n\n"+
					"The provider cannot create its own credential, and neither can any other "+
					"machine principal — the platform denies that by design. A tenant administrator "+
					"issues the service account once, by hand, and hands over the client id and "+
					"secret.",
				EnvVar(envClientID), EnvVar(envClientSecret),
			),
		)
		return CredentialSource{}, diags
	}
}

// Meta is what Configure hands to every resource and data source.
//
// It carries the client and the two targeting values, and nothing else. There
// is deliberately no cached lookup, no region list and no retry policy on it:
// ADR D4 keeps that kind of behaviour out of the client, and putting it one
// layer up in the provider would be the same mistake with a different address.
type Meta struct {
	// Client is the generated client, wired to mint and attach bearer tokens.
	Client *k0rdent.ClientWithResponses

	// Endpoint is the base URL the client was built for. Present for error
	// messages, which are much harder to read without it.
	Endpoint string

	// Region and Project are the resolved targeting values, either of which
	// may be empty. Read them through RequireRegion and RequireProject.
	Region  string
	Project string

	// DefaultTags is the provider block's default_tags, which every resource
	// that carries tags merges into its tags_all. See tags.go.
	DefaultTags defaultTags

	// Wait bounds every lifecycle waiter in this provider. Resources read it
	// rather than holding their own constants, so one configuration change
	// moves all of them.
	Wait waitConfig

	// vpcBindings is what each VPC binding written in this run of the
	// provider holds. One run serves one plan or one apply, so it's how a
	// destroy tells a binding a newer instance wrote moments ago from the one
	// it's destroying. See resource_vpc_security_groups.go.
	vpcBindings bindingLedger

	// webhookList is the subscription list this run read last, shared by
	// every lookup of one subscription. See webhook_list_memo.go.
	webhookList webhookListMemo

	// regionList is the region list this run read, for the project region
	// checks. See regions.go.
	regionList regionListMemo

	// projectRegions is each project's region as this run read it. See
	// region_mismatch.go.
	projectRegions projectRegionMemo

	// addressPools is each address pool as this run read it, for the
	// plan-time subnet check. See subnet_pool_check.go.
	addressPools addressPoolMemo
}

// RequireRegion returns the configured region, or an error diagnostic.
//
// Region is optional at configure time on purpose, and this is where that
// choice is paid for. Making it required would be a better error — one
// failure instead of one per resource — but it would also make the provider
// unconfigurable by anyone who does not already know a region id. Region ids
// are operator-declared topology with no documented list, and the only way to
// learn them is the region-discovery call, which needs a configured provider. A
// required attribute would close the only door to its own answer.
func (m *Meta) RequireRegion() (string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if m.Region != "" {
		return m.Region, diags
	}
	diags.AddError(
		"No region configured",
		fmt.Sprintf(
			"This resource is regional and the provider has no region.\n\n"+
				"Set the `region` attribute in the provider block or the %s environment variable. "+
				"Valid ids come from `%s`; they are deployment-specific, so there is "+
				"no default worth guessing.",
			EnvVar(envRegion), regionsOperation,
		),
	)
	return "", diags
}

// RequireProject returns the configured project slug, or an error diagnostic.
// Optional for the same reason as Region: the region-wide catalogs, including
// the cluster-type list, are not project-scoped.
func (m *Meta) RequireProject() (string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if m.Project != "" {
		return m.Project, diags
	}
	diags.AddError(
		"No project configured",
		fmt.Sprintf(
			"This resource is project-scoped and the provider has no project.\n\n"+
				"Set the `project` attribute in the provider block or the %s environment variable. "+
				"The value is the project's `id`.",
			EnvVar(envProject),
		),
	)
	return "", diags
}

// newMeta builds the client from resolved settings.
//
// It makes no API call. A connectivity or credential probe here would turn
// every `terraform plan` into a network round trip and a mint, including plans
// run in environments that legitimately have no reachable API — and it would
// spend one of the ten mints a credential is allowed per minute before any
// resource has asked for anything. The first real request is the first check.
func newMeta(s Settings, userAgent string) (*Meta, error) {
	// Outermost, so a 409 it reads is the final answer after any retry. It
	// names a refused project's region, through the client it's part of.
	mismatch := &regionMismatchTransport{}
	client, err := k0rdent.New(s.Endpoint,
		k0rdent.WithCredential(s.Credential.build()),
		// The order is load-bearing. Rate limit handling is outermost so a
		// retried request is logged as the separate request it is. A 3xx is
		// logged with its real status here, and the client refuses to follow
		// it above all of these.
		k0rdent.WithTransport(mismatch),
		// Terraform, the provider and their versions, on every request
		// including the mint. Version skew between a pinned provider and a
		// moving platform is normal here, so a server-side log that says
		// which provider version sent a request is worth the one header.
		k0rdent.WithUserAgent(userAgent),
	)
	if err != nil {
		return nil, err
	}
	// One pool per configured provider, cloned rather than shared. See
	// NewPlatformTransport.
	mismatch.next = newRateLimitTransport(loggingTransport{base: NewPlatformTransport()})
	meta := &Meta{
		Client:   client,
		Endpoint: s.Endpoint,
		Region:   s.Region,
		Project:  s.Project,
		Wait: waitConfig{
			Timeout:                  s.OperationTimeout,
			Long:                     s.LongOperationTimeout,
			Interval:                 s.PollInterval,
			ProvisionalFailureWindow: ProvisionalFailureWindow,
		},
	}
	mismatch.lookup = meta.projectRegion
	return meta, nil
}
