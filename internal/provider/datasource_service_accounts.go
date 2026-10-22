package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// Service accounts and their credentials, read only.
//
// Only a person can create a service account or a credential, and the
// provider signs in with client_credentials, so it manages neither and reads
// both. The
// reads answer what a pipeline needs to know about its own identity: whether
// the account is still active, what roles it holds, and when each credential
// expires.

var (
	_ datasource.DataSource                   = (*serviceAccountsDataSource)(nil)
	_ datasource.DataSourceWithConfigure      = (*serviceAccountsDataSource)(nil)
	_ datasource.DataSourceWithValidateConfig = (*serviceAccountsDataSource)(nil)
	_ datasource.DataSource                   = (*serviceAccountCredentialsDataSource)(nil)
	_ datasource.DataSourceWithConfigure      = (*serviceAccountCredentialsDataSource)(nil)
	_ datasource.DataSourceWithValidateConfig = (*serviceAccountCredentialsDataSource)(nil)
)

const (
	serviceAccountsDataSourceSuffix           = "service_accounts"
	serviceAccountCredentialsDataSourceSuffix = "service_account_credentials"
)

func newServiceAccountsDataSource() datasource.DataSource { return &serviceAccountsDataSource{} }

func newServiceAccountCredentialsDataSource() datasource.DataSource {
	return &serviceAccountCredentialsDataSource{}
}

type serviceAccountsDataSource struct{ meta *Meta }
type serviceAccountCredentialsDataSource struct{ meta *Meta }

// serviceAccountScopeAttributes narrow a search to one scope. The credentials
// form shares them, to search the accounts they find when it isn't named one.
func serviceAccountScopeAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"organization_id": schema.StringAttribute{
			Required:            true,
			MarkdownDescription: "Organization to search. The service account operations are all inside one.",
			Validators:          idValidators(),
		},
		"scope": schema.StringAttribute{
			Optional: true,
			MarkdownDescription: "Search only the accounts at this level: " +
				enumProse(k0rdent.ListServiceAccountsParamsScopeValues()) + ". Left out, the organization's own accounts and those of every project in it " +
				"are searched. A folder's accounts are found only by naming the folder, since the " +
				"contract has no operation that lists folders.",
			Validators: []validator.String{oneOfEnum(k0rdent.ListServiceAccountsParamsScopeValues())},
		},
		"scope_id": schema.StringAttribute{
			Optional: true,
			MarkdownDescription: "What `scope` names: a folder id for `folder`, a project id for " +
				"`project`. For `organization` it's the organization's id, and that's the default." +
				knownGap("The contract declares no folder operation, so a folder's id can't be read."),
			Validators: idValidators(),
		},
	}
}

// validateServiceAccountScope refuses a scope_id without a scope, and a
// folder or project scope without the id it needs.
func validateServiceAccountScope(scope, scopeID types.String, diags *diag.Diagnostics) {
	if scope.IsUnknown() || scopeID.IsUnknown() {
		return
	}
	switch {
	case scope.IsNull() && !scopeID.IsNull():
		diags.AddAttributeError(path.Root("scope_id"), "`scope_id` needs a `scope`",
			"Name the level `scope_id` is at: `organization`, `folder` or `project`.")
	case !scope.IsNull() && scope.ValueString() != string(k0rdent.ListServiceAccountsParamsScopeOrganization) && scopeID.IsNull():
		diags.AddAttributeError(path.Root("scope_id"), "This scope needs a `scope_id`",
			fmt.Sprintf("A `%s` scope names the %s to search in `scope_id`.", scope.ValueString(), scope.ValueString()))
	}
}

// serviceAccountsIn is every account the scope filters find in an
// organization: one scope when the configuration names it, or the
// organization's own and every project's.
func serviceAccountsIn(ctx context.Context, meta *Meta, org string, scope, scopeID types.String, diags *diag.Diagnostics) ([]k0rdent.ServiceAccount, bool) {
	type at struct {
		scope k0rdent.ListServiceAccountsParamsScope
		id    string
	}
	var scopes []at
	s, named := known(scope)
	if named {
		id, given := known(scopeID)
		if !given {
			id = org
		}
		scopes = append(scopes, at{k0rdent.ListServiceAccountsParamsScope(s), id})
	} else {
		scopes = append(scopes, at{k0rdent.ListServiceAccountsParamsScopeOrganization, org})
		projects, ok := listProjects(ctx, meta, k0rdent.ListProjectsParams{OrganizationId: &org}, diags)
		if !ok {
			return nil, false
		}
		for _, p := range projects {
			if id := deref(p.Id); id != "" && deref(p.OrganizationId) == org && !goingAway(stateString(p.Status)) {
				scopes = append(scopes, at{k0rdent.ListServiceAccountsParamsScopeProject, id})
			}
		}
	}
	var out []k0rdent.ServiceAccount
	for i, s := range scopes {
		// The organization's own scope, and one the configuration named, are
		// never a walk: a 404 there is an error. A project found by the
		// project list and gone since is skipped with a warning.
		walked := !named && i > 0
		ok := underEach([]string{s.id}, string(s.scope), walked, diags, func(_ string, d *diag.Diagnostics) bool {
			rows, ok := listServiceAccounts(ctx, meta, org, s.scope, s.id, d)
			out = append(out, rows...)
			return ok
		})
		if !ok {
			return nil, false
		}
	}
	out = firstOfEach(out, func(sa k0rdent.ServiceAccount) string { return sa.Id })
	out = slices.DeleteFunc(out, func(sa k0rdent.ServiceAccount) bool { return sa.Id == "" })
	// The contract gives the list no order, and a list that came back in a
	// new one would show as a change on every refresh.
	slices.SortFunc(out, func(a, b k0rdent.ServiceAccount) int { return strings.Compare(a.Id, b.Id) })
	return out, true
}

// listedObjectType is the object type of one row of a nested list.
func listedObjectType(attrs map[string]schema.Attribute) types.ObjectType {
	out := map[string]attr.Type{}
	for name, a := range attrs {
		out[name] = a.GetType()
	}
	return types.ObjectType{AttrTypes: out}
}

// ---------------------------------------------------------------------------
// Every service account
// ---------------------------------------------------------------------------

type serviceAccountView struct {
	ID                    types.String `tfsdk:"id"`
	DisplayName           types.String `tfsdk:"display_name"`
	Description           types.String `tfsdk:"description"`
	Scope                 types.String `tfsdk:"scope"`
	ScopeID               types.String `tfsdk:"scope_id"`
	Roles                 types.Set    `tfsdk:"roles"`
	Status                types.String `tfsdk:"status"`
	ClientID              types.String `tfsdk:"client_id"`
	ActiveCredentialCount types.Int64  `tfsdk:"active_credential_count"`
	CreatedBy             types.String `tfsdk:"created_by"`
	CreatedAt             types.String `tfsdk:"created_at"`
	UpdatedAt             types.String `tfsdk:"updated_at"`
	SelfLink              types.String `tfsdk:"self_link"`
}

func serviceAccountViewFrom(ctx context.Context, sa k0rdent.ServiceAccount) (serviceAccountView, diag.Diagnostics) {
	roles, diags := types.SetValueFrom(ctx, types.StringType, rowsOf(sa.Roles))
	return serviceAccountView{
		ID:                    types.StringValue(sa.Id),
		DisplayName:           types.StringValue(sa.DisplayName),
		Description:           stringOrNull(sa.Description),
		Scope:                 types.StringValue(string(sa.Scope)),
		ScopeID:               types.StringValue(sa.ScopeId),
		Roles:                 roles,
		Status:                enumOrNull(sa.Status),
		ClientID:              stringOrNull(sa.ClientId),
		ActiveCredentialCount: int64OrNull(sa.ActiveCredentialCount),
		CreatedBy:             stringOrNull(sa.CreatedBy),
		CreatedAt:             timeOrNull(sa.CreatedAt),
		UpdatedAt:             timeOrNull(sa.UpdatedAt),
		SelfLink:              stringOrNull(sa.SelfLink),
	}, diags
}

func serviceAccountViewAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{Computed: true,
			MarkdownDescription: "The service account's id, which `" + ResourceName(serviceAccountDataSourceSuffix) + "` reads one by."},
		"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-friendly name the account is shown under."},
		"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "What the account is for, if anyone said."},
		"scope": schema.StringAttribute{Computed: true,
			MarkdownDescription: "The level its roles apply at: `organization`, `folder` or `project`."},
		"scope_id": schema.StringAttribute{Computed: true, MarkdownDescription: "The organization, folder or project `scope` names."},
		"roles": schema.SetAttribute{Computed: true, ElementType: types.StringType,
			MarkdownDescription: "Role slugs bound to the account at its scope."},
		"status": schema.StringAttribute{Computed: true,
			MarkdownDescription: "`active`, or `disabled`, in which case no credential of it mints a token."},
		"client_id": schema.StringAttribute{Computed: true, MarkdownDescription: "The OAuth2 client id the account authenticates with."},
		"active_credential_count": schema.Int64Attribute{Computed: true,
			MarkdownDescription: "Credentials that haven't expired, of the five an account can hold."},
		"created_by": schema.StringAttribute{Computed: true, MarkdownDescription: "The user id of the administrator who created it."},
		"created_at": schema.StringAttribute{Computed: true, MarkdownDescription: "When it was created, as RFC 3339."},
		"updated_at": schema.StringAttribute{Computed: true, MarkdownDescription: "When it last changed, as RFC 3339."},
		"self_link":  schema.StringAttribute{Computed: true, MarkdownDescription: "Platform-defined reference to this service account."},
	}
}

type serviceAccountsModel struct {
	OrganizationID   types.String `tfsdk:"organization_id"`
	Scope            types.String `tfsdk:"scope"`
	ScopeID          types.String `tfsdk:"scope_id"`
	Status           types.String `tfsdk:"status"`
	DisplayNameRegex types.String `tfsdk:"display_name_regex"`
	ServiceAccounts  types.List   `tfsdk:"service_accounts"`
}

func (d *serviceAccountsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(serviceAccountsDataSourceSuffix)
}

func (d *serviceAccountsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := serviceAccountScopeAttributes()
	attrs["status"] = stateFilterAttribute("accounts", "status", enumStrings(k0rdent.ServiceAccountStatusValues()...), false)
	attrs["display_name_regex"] = displayNameRegexAttribute("accounts")
	attrs["service_accounts"] = schema.ListNestedAttribute{
		Computed:            true,
		MarkdownDescription: "The accounts, in id order.",
		NestedObject:        schema.NestedAttributeObject{Attributes: serviceAccountViewAttributes()},
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every service account in an organization, or at one scope in it.\n\n" +
			"Use it to see which accounts exist, what roles each holds, and whether it's still " +
			"`active`. The provider reads service accounts and doesn't make them: the platform refuses " +
			"to create an account or a credential for a `client_credentials` login, which is the only " +
			"login the provider has.",
		Attributes: attrs,
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *serviceAccountsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *serviceAccountsDataSource) ValidateConfig(ctx context.Context, req datasource.ValidateConfigRequest, resp *datasource.ValidateConfigResponse) {
	var data serviceAccountsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Local: configuration validation, no request made.
	validateServiceAccountScope(data.Scope, data.ScopeID, &resp.Diagnostics)
}

func (d *serviceAccountsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data serviceAccountsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() || !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}
	filter, diags := readObjectFilter(ctx, types.MapNull(types.StringType), data.DisplayNameRegex, data.Status)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	accounts, ok := serviceAccountsIn(ctx, d.meta, data.OrganizationID.ValueString(), data.Scope, data.ScopeID, &resp.Diagnostics)
	if !ok {
		return
	}
	views := []serviceAccountView{}
	for _, sa := range accounts {
		if !filter.keeps(filterSubject{displayName: &sa.DisplayName, state: stateString(sa.Status)}) {
			continue
		}
		view, viewDiags := serviceAccountViewFrom(ctx, sa)
		resp.Diagnostics.Append(viewDiags...)
		views = append(views, view)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	list, diags := types.ListValueFrom(ctx, listedObjectType(serviceAccountViewAttributes()), views)
	resp.Diagnostics.Append(diags...)
	data.ServiceAccounts = list
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// ---------------------------------------------------------------------------
// Every credential
// ---------------------------------------------------------------------------

type serviceAccountCredentialView struct {
	ID               types.String `tfsdk:"id"`
	ServiceAccountID types.String `tfsdk:"service_account_id"`
	Status           types.String `tfsdk:"status"`
	CreatedBy        types.String `tfsdk:"created_by"`
	CreatedAt        types.String `tfsdk:"created_at"`
	ExpiresAt        types.String `tfsdk:"expires_at"`
	LastUsedAt       types.String `tfsdk:"last_used_at"`
	LastUsedIP       types.String `tfsdk:"last_used_ip"`
	SelfLink         types.String `tfsdk:"self_link"`
}

func serviceAccountCredentialViewFrom(account string, c k0rdent.ServiceAccountCredential) serviceAccountCredentialView {
	// The row's own parent wins. The account searched is the fallback for a
	// row that leaves it out.
	parent := deref(c.ServiceAccountId)
	if parent == "" {
		parent = account
	}
	return serviceAccountCredentialView{
		ID:               types.StringValue(deref(c.Id)),
		ServiceAccountID: types.StringValue(parent),
		Status:           enumOrNull(c.Status),
		CreatedBy:        stringOrNull(c.CreatedBy),
		CreatedAt:        timeOrNull(c.CreatedAt),
		ExpiresAt:        timeOrNull(c.ExpiresAt),
		LastUsedAt:       timeOrNull(c.LastUsedAt),
		LastUsedIP:       stringOrNull(c.LastUsedIp),
		SelfLink:         stringOrNull(c.SelfLink),
	}
}

func serviceAccountCredentialViewAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{Computed: true,
			MarkdownDescription: "The credential's id, which the platform assigns. It names the credential in the platform's audit log."},
		"service_account_id": schema.StringAttribute{Computed: true, MarkdownDescription: "The service account it belongs to."},
		"status": schema.StringAttribute{Computed: true,
			MarkdownDescription: "`active` or `expired`. The platform marks a credential `expired` a little after " +
				"`expires_at` passes, so read `expires_at` for the exact moment it stops working."},
		"created_by": schema.StringAttribute{Computed: true, MarkdownDescription: "The user id of the administrator who created it."},
		"created_at": schema.StringAttribute{Computed: true, MarkdownDescription: "When it was created, as RFC 3339."},
		"expires_at": schema.StringAttribute{Computed: true, MarkdownDescription: "When it stops authenticating, as RFC 3339."},
		"last_used_at": schema.StringAttribute{Computed: true,
			MarkdownDescription: "When it last minted a token, as RFC 3339. Null if it never has."},
		"last_used_ip": schema.StringAttribute{Computed: true, MarkdownDescription: "The address it was last used from."},
		"self_link":    schema.StringAttribute{Computed: true, MarkdownDescription: "Platform-defined reference to this credential."},
	}
}

type serviceAccountCredentialsModel struct {
	OrganizationID   types.String `tfsdk:"organization_id"`
	Scope            types.String `tfsdk:"scope"`
	ScopeID          types.String `tfsdk:"scope_id"`
	ServiceAccountID types.String `tfsdk:"service_account_id"`
	Status           types.String `tfsdk:"status"`
	Issued           types.List   `tfsdk:"issued"`
}

func (d *serviceAccountCredentialsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(serviceAccountCredentialsDataSourceSuffix)
}

func (d *serviceAccountCredentialsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := serviceAccountScopeAttributes()
	attrs["service_account_id"] = schema.StringAttribute{
		Optional: true,
		MarkdownDescription: "Service account whose credentials to find. Left out, the credentials of " +
			"every account `scope` and `scope_id` find are.",
		Validators: idValidators(),
	}
	status := stateFilterAttribute("credentials", "status", enumStrings(k0rdent.ServiceAccountCredentialStatusValues()...), false)
	status.MarkdownDescription = "Return only the credentials whose `status` is this value. Left out, " +
		"every credential but the `expired` ones: expiry is final, and they authenticate nothing. " +
		filteredInProvider
	attrs["status"] = status
	// Not `credentials`: the credential gate reads that name as a secret, and
	// none is here.
	attrs["issued"] = schema.ListNestedAttribute{
		Computed: true,
		MarkdownDescription: "Each credential issued, by account then id: when it expires and when it was " +
			"last used. No secret is here or anywhere else: the platform hands a credential's secret out " +
			"once, to whoever creates it.",
		NestedObject: schema.NestedAttributeObject{Attributes: serviceAccountCredentialViewAttributes()},
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "The credentials of one service account, or of every account in an organization, " +
			"with when each expires.\n\n" +
			"Use it to warn before a pipeline's credential lapses. The provider reads credentials and " +
			"doesn't issue them: the platform refuses that to a `client_credentials` login.",
		Attributes: attrs,
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *serviceAccountCredentialsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *serviceAccountCredentialsDataSource) ValidateConfig(ctx context.Context, req datasource.ValidateConfigRequest, resp *datasource.ValidateConfigResponse) {
	var data serviceAccountCredentialsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Local: configuration validation, no request made.
	validateServiceAccountScope(data.Scope, data.ScopeID, &resp.Diagnostics)
	// Unknown under `terraform validate`, and it may still turn out null.
	if !data.ServiceAccountID.IsNull() && !data.ServiceAccountID.IsUnknown() && !data.Scope.IsNull() && !data.Scope.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("scope"), "`scope` and `service_account_id` don't go together",
			"`service_account_id` names the one account to search, and `scope` narrows a search of "+
				"every account. Set one of them.")
	}
}

func (d *serviceAccountCredentialsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data serviceAccountCredentialsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() || !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}
	org := data.OrganizationID.ValueString()
	var accounts []string
	account, named := known(data.ServiceAccountID)
	if named {
		accounts = []string{account}
	} else {
		found, ok := serviceAccountsIn(ctx, d.meta, org, data.Scope, data.ScopeID, &resp.Diagnostics)
		if !ok {
			return
		}
		for _, sa := range found {
			accounts = append(accounts, sa.Id)
		}
	}
	want, filtered := known(data.Status)
	views := []serviceAccountCredentialView{}
	ok := underEach(accounts, "service account", !named, &resp.Diagnostics, func(account string, diags *diag.Diagnostics) bool {
		rows, ok := listServiceAccountCredentials(ctx, d.meta, org, account, diags)
		rows = slices.DeleteFunc(rows, func(c k0rdent.ServiceAccountCredential) bool {
			// Expired is terminal in the contract: the credential
			// authenticates nothing and is never revived.
			status := stateString(c.Status)
			return deref(c.Id) == "" || (filtered && status != want) ||
				(!filtered && status == string(k0rdent.ServiceAccountCredentialStatusExpired))
		})
		slices.SortFunc(rows, func(a, b k0rdent.ServiceAccountCredential) int { return strings.Compare(deref(a.Id), deref(b.Id)) })
		for _, c := range rows {
			views = append(views, serviceAccountCredentialViewFrom(account, c))
		}
		return ok
	})
	if !ok {
		return
	}
	list, diags := types.ListValueFrom(ctx, listedObjectType(serviceAccountCredentialViewAttributes()), views)
	resp.Diagnostics.Append(diags...)
	data.Issued = list
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
