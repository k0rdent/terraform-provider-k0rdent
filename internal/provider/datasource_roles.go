package provider

import (
	"context"
	"slices"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ datasource.DataSource              = (*rolesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*rolesDataSource)(nil)
)

const rolesDataSourceSuffix = "roles"

func newRolesDataSource() datasource.DataSource { return &rolesDataSource{} }

// rolesDataSource lists the roles an organization defines, over listRoles.
//
// It exists for the service account resource's `roles`, which take a role's `id`.
// The contract's service account examples name roles with dots
// (`compute.deployer`), and on the dev.4 stack those are refused with
// `role "compute.deployer" does not exist`, while the ids listRoles answers
// (`compute-admin`) are accepted. So the id is what a binding names, and this
// is where a configuration reads one rather than writing a literal.
type rolesDataSource struct{ meta *Meta }

type rolesModel struct {
	OrganizationID   types.String     `tfsdk:"organization_id"`
	Scope            types.String     `tfsdk:"scope"`
	DisplayNameRegex types.String     `tfsdk:"display_name_regex"`
	Roles            []roleEntryModel `tfsdk:"roles"`
	IDs              types.List       `tfsdk:"ids"`
}

type roleEntryModel struct {
	ID          types.String `tfsdk:"id"`
	DisplayName types.String `tfsdk:"display_name"`
	Description types.String `tfsdk:"description"`
	Scopes      types.List   `tfsdk:"scopes"`
	Permissions types.List   `tfsdk:"permissions"`
}

func (d *rolesDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(rolesDataSourceSuffix)
}

func (d *rolesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The roles an organization defines.\n\n" +
			"A service account's `roles`, as `" + ResourceName(serviceAccountsDataSourceSuffix) + "` reports " +
			"them, are role `id`s such as `compute-admin`. This is where to read what each one is. A role " +
			"can be bound only at the tiers its `scopes` list.",
		Attributes: map[string]schema.Attribute{
			"organization_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The organization whose roles to list.",
				Validators:          idValidators(),
			},
			"scope": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Only return roles that can be bound at this tier: " +
					enumProse(k0rdent.RoleScopesValues()) + ". Filtered by the provider, after the whole list is read.",
				Validators: []validator.String{oneOfEnum(k0rdent.RoleScopesValues())},
			},
			"display_name_regex": displayNameRegexAttribute("roles"),
			// A List because a catalog has to be indexable and a Set is not.
			"roles": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The roles, sorted by `id`. Address them with `for_each` rather " +
					"than an index: a new role shifts every index after it.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The id a service account's `roles` list.",
						},
						"display_name": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Human-readable name the role is shown under.",
						},
						"description": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "What the role is for, if the platform says.",
						},
						"scopes": schema.ListAttribute{
							Computed:            true,
							ElementType:         types.StringType,
							MarkdownDescription: "The tiers the role can be bound at: `platform`, `org`, `project`.",
						},
						"permissions": schema.ListAttribute{
							Computed:            true,
							ElementType:         types.StringType,
							MarkdownDescription: "The permissions the role grants, in the platform's order.",
						},
					},
				},
			},
			"ids": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Just the ids, for `contains` and `for_each`. Same order as `roles`.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *rolesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *rolesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data rolesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() || !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}
	org := data.OrganizationID.ValueString()
	filter, filterDiags := readObjectFilter(ctx, types.MapNull(types.StringType), data.DisplayNameRegex, types.StringNull())
	resp.Diagnostics.Append(filterDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var params k0rdent.ListRolesParams
	roles, ok := listByOffset(&resp.Diagnostics, "roles", func(r k0rdent.Role) string { return r.Id },
		func(offset, limit int32) (page[k0rdent.Role], error) {
			params.Offset, params.Limit = &offset, &limit
			got, err := d.meta.Client.ListRolesWithResponse(ctx, org, &params)
			if err != nil {
				return page[k0rdent.Role]{}, err
			}
			if got.JSON200 == nil {
				return page[k0rdent.Role]{}, unreadPage(got.HTTPResponse, got.Body)
			}
			return page[k0rdent.Role]{items: rowsOf(got.JSON200.Roles), total: got.JSON200.TotalCount}, nil
		})
	if !ok {
		return
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Id < roles[j].Id })

	data.Roles = []roleEntryModel{}
	ids := []string{}
	for _, r := range roles {
		scopes := make([]string, len(r.Scopes))
		for i, s := range r.Scopes {
			scopes[i] = string(s)
		}
		if !data.Scope.IsNull() && !slices.Contains(scopes, data.Scope.ValueString()) {
			continue
		}
		if !filter.keeps(filterSubject{displayName: &r.DisplayName}) {
			continue
		}
		scopeList, diags := types.ListValueFrom(ctx, types.StringType, scopes)
		resp.Diagnostics.Append(diags...)
		perms, diags := types.ListValueFrom(ctx, types.StringType, r.Permissions)
		resp.Diagnostics.Append(diags...)
		data.Roles = append(data.Roles, roleEntryModel{
			ID:          types.StringValue(r.Id),
			DisplayName: types.StringValue(r.DisplayName),
			Description: stringOrNull(r.Description),
			Scopes:      scopeList,
			Permissions: perms,
		})
		ids = append(ids, r.Id)
	}
	list, diags := types.ListValueFrom(ctx, types.StringType, ids)
	resp.Diagnostics.Append(diags...)
	data.IDs = list
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
