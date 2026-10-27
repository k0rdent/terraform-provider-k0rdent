package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ resource.Resource                 = (*projectResource)(nil)
	_ resource.ResourceWithConfigure    = (*projectResource)(nil)
	_ resource.ResourceWithImportState  = (*projectResource)(nil)
	_ resource.ResourceWithIdentity     = (*projectResource)(nil)
	_ resource.ResourceWithModifyPlan   = (*projectResource)(nil)
	_ resource.ResourceWithUpgradeState = (*projectResource)(nil)
)

const projectResourceSuffix = "project"

func newProjectResource() resource.Resource {
	return guardReplacements(&projectResource{}, projectTenantState)
}

// projectResource manages the scope every other resource in this provider is
// created inside.
//
// The organization is an input: creating one is operator-plane only, so a
// tenant credential cannot reach it.
//
// It takes no project, and its region isn't part of its address. A project
// lives on the global tier and is addressed inside an organization. Since
// v1.3.0-dev.4 it's created in one region and locked to it, so the
// region is an argument that defaults like a regional resource's and replaces
// the project when it moves.
//
// A create does not wait: the public create inserts the project `active` and
// answers 201, and `ProjectStatus` declares no in-progress create state. A
// delete does wait, to the first 404, because a project at `deleting` still
// holds its id and a replacement in the same apply would collide with it.
type projectResource struct{ meta *Meta }

type projectModel struct {
	ID             types.String   `tfsdk:"id"`
	OrganizationID types.String   `tfsdk:"organization_id"`
	Region         types.String   `tfsdk:"region"`
	DisplayName    types.String   `tfsdk:"display_name"`
	Description    types.String   `tfsdk:"description"`
	Status         types.String   `tfsdk:"status"`
	SelfLink       types.String   `tfsdk:"self_link"`
	Timeouts       timeouts.Value `tfsdk:"timeouts"`

	// uid is the platform's uid for the project read, which own_object.go
	// keeps in private state. Not an attribute.
	uid string
}

func (m projectModel) metadata() tenantMetadata {
	return tenantMetadata{
		DisplayName: m.DisplayName,
		Description: m.Description,
	}
}

func (r *projectResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = ResourceName(projectResourceSuffix)
}

// UpgradeState migrates a state written at an earlier schema version. See
// state_history.go.
func (r *projectResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return stateHistories[projectResourceSuffix].upgraders()
}

func (r *projectResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Version: stateHistories[projectResourceSuffix].version(),
		MarkdownDescription: "A project, which is the scope most other resources in this provider are " +
			"created inside.\n\n" +
			"Unlike every other resource here it takes no `project`, and its `region` isn't part of " +
			"its address: it lives on the global control-plane tier and is addressed inside an " +
			"organization. It's created in one region and locked to it, and every regional resource " +
			"in it has to be in that region. The value it " +
			"produces in `id` is what goes in the provider block's `project`, or in another " +
			"resource's `project` argument.\n\n" +
			"The provider block cannot take a value that is unknown at plan time. When one " +
			"configuration both creates a project and points the provider block at it, set `id` " +
			"here so the value is known; a resource's own `project` argument has no such limit.\n\n" +
			"**This provider cannot create an organization.** That is an operator-plane operation " +
			"and is not reachable by a tenant credential, so `organization_id` is an input. The `" +
			ResourceName(projectsDataSourceSuffix) + "` data source reports it per project.",
		Blocks: map[string]schema.Block{
			// Only delete waits. See the type comment: the contract declares no
			// in-progress state for a create, so there is nothing to wait out.
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Delete: true}),
		},
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Project id, the value the provider block's `project` takes. " +
					"Unique inside its organization. Omit it and the provider " +
					"generates one, `tf-` and 16 hex digits, which is known only after apply, so " +
					"that a create whose answer is lost can be read back rather than sent twice.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
				Validators: idValidators(),
			},
			"organization_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The organization to create the project in. A project cannot be " +
					"moved between organizations, so changing this replaces it. The platform refuses " +
					"to delete a project that still holds clusters, so a replacement fails until they " +
					"are gone. It does not remove the project's networks, subnets or VPCs, and it does " +
					"not disable project-scoped API keys: revoke those yourself. The `" +
					ResourceName(organizationsDataSourceSuffix) + "` data source lists the " +
					"organizations the credential can see." +
					knownGap("An API key may list no organizations, and `"+
						ResourceName(projectsDataSourceSuffix)+"` can only report an organization that "+
						"already holds a project. So the first project in an organization takes "+
						"its id as a literal."),
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{urlSafeIdentifier{}},
			},
			"region": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The region the project is created in and locked to. Every regional " +
					"resource in the project has to be in this region. Defaults to the provider's `region`, " +
					"and a project needs one or the other. The region has to be one that can hold a project: " +
					"the `" + ResourceName(regionsDataSourceSuffix) + "` data source lists each region's " +
					"`hosts_projects`. A project's region can't change, so changing this, or the provider " +
					"region it defaults from, replaces the project. A project made before regions were " +
					"required reads back with none and can't be given one: leave `region` out for it.",
				Validators: regionIDValidators(),
			},
			"display_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Human-friendly name. Mutable. Removing it clears it.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description. Mutable. Removing it clears it.",
			},
			"status": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Lifecycle status as last read. `active` is usable. `suspended` and " +
					"`disabled` are steady states the platform can hold a project in, and are " +
					"reported rather than waited out or treated as a failure.",
				// Only the platform's lifecycle moves it; an update changes the
				// two text fields and nothing else.
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this project.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// ModifyPlan resolves the project's region. The replacement guard runs after
// it (replace_guard_resource.go).
func (r *projectResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	r.planRegion(ctx, req, resp)
}

// planRegion decides `region` the way modifyScopePlan decides a regional
// resource's: the configuration's value, else the provider's, written into
// the plan so a changed default shows, and a replacement when it moves.
//
// Two things differ. A project with no region anywhere is refused here,
// naming where to set one, since the create would only get the platform's
// 422. And a project the platform reports with no region, one made before
// regions were required, keeps the null it has, since planning the provider's
// region onto it would be a change no apply can make.
func (r *projectResource) planRegion(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // a destroy
	}
	at := path.Root("region")
	var config, state types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, at, &config)...)
	creating := req.State.Raw.IsNull()
	if !creating {
		resp.Diagnostics.Append(req.State.GetAttribute(ctx, at, &state)...)
	}
	if resp.Diagnostics.HasError() || config.IsUnknown() {
		return
	}
	if !creating && state.IsNull() && config.IsNull() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, at, types.StringNull())...)
		return
	}
	if !creating && state.IsNull() {
		// The platform refuses a region on an update, and an update that sent
		// none would read the null back, so the apply would fail with an
		// inconsistent result every time. Local: decided from state.
		resp.Diagnostics.AddAttributeError(at, "This project has no region to set",
			fmt.Sprintf("The platform reports no region for this project: it was made before projects "+
				"were locked to a region, and its region can't be set afterwards. So remove `region` from "+
				"this resource, and the project keeps working as it is. A project in %q is a new "+
				"project, with a new id. If this plan ran with -refresh=false, state may not have "+
				"read the project's region yet: plan again with a refresh first.", config.ValueString()))
		return
	}

	deflt := ""
	if r.meta != nil {
		deflt = r.meta.Region
	}
	out := resolveScopeValue(config, state, deflt, false, creating)
	switch {
	case !out.Write && creating:
		// Local: decided from the configuration and the provider block.
		resp.Diagnostics.AddAttributeError(at, "A project needs a region",
			redactDiagnosticText("A project is created in one region and locked to it, and neither this "+
				"resource's `region` nor the provider's `region` is set. Set one of them. "+whereProjectsGo(ctx, r.meta)))
		return
	case !out.Write:
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, at, state)...)
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, at, out.Value)...)
	if out.Replace {
		resp.RequiresReplace = append(resp.RequiresReplace, at)
	}
	if creating || out.Replace {
		checkProjectRegion(ctx, r.meta, out.Value.ValueString(), &resp.Diagnostics)
	}
}

func (r *projectResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (r *projectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan projectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// A project has no tags, so it carries no create token: only a generated
	// id proves a project found after a lost answer is this one.
	key, err := newCreateKey(plan.ID, false)
	if err != nil {
		addCreateKeyError(&resp.Diagnostics, "project", err)
		return
	}
	org := plan.OrganizationID.ValueString()
	// Required since v1.3.0-dev.4. ModifyPlan never leaves it empty on a
	// create: a project with no region anywhere is refused there.
	body := k0rdent.ProjectCreateRequest{
		Id:     &key.id,
		Region: plan.Region.ValueString(),
	}
	md, diags := plan.metadata().create(ctx)
	resp.Diagnostics.Append(diags...)
	body.DisplayName, body.Description = md.DisplayName, md.Description

	// A create is synchronous, so its answer is the project. When the answer
	// is lost the read that proves the project is this one supplies it.
	var got *k0rdent.Project
	outcome, id, diags := convergingCreate{
		key: key, kind: "project", what: "create the project",
		resourceName: ResourceName(projectResourceSuffix),
		importID:     func(id string) string { return org + "/" + id },
		send: func(ctx context.Context) createAnswer {
			created, err := r.meta.Client.CreateProjectInOrgWithResponse(ctx, org, body)
			if err != nil {
				return createAnswer{err: err}
			}
			a := createAnswer{resp: created.HTTPResponse, body: created.Body}
			if created.JSON201 != nil {
				got, a.id = created.JSON201, deref(created.JSON201.Id)
			}
			return a
		},
		lookup: func(ctx context.Context, id string) createLookup {
			read, err := r.meta.Client.GetProjectInOrgWithResponse(ctx, org, id)
			if err != nil {
				return createLookup{err: err}
			}
			if read.JSON200 != nil {
				got = read.JSON200
			}
			return lookupFrom(read.HTTPResponse, read.Body, read.JSON200 != nil, func() string { return "" })
		},
	}.run(ctx)
	resp.Diagnostics.Append(diags...)
	switch {
	case outcome == createFailed:
		return
	case outcome == createUnconfirmed || got == nil:
		// Recorded so the next refresh reads it back rather than creating
		// another. A 2xx with no body to read lands here too.
		resp.Diagnostics.Append(recordUnconfirmed(ctx, req.Plan, &resp.State, id)...)
		if outcome == createDone {
			// Local: a 2xx the provider could not read.
			resp.Diagnostics.AddError("The new project could not be read",
				fmt.Sprintf("`createProjectInOrg` answered %q without a project body.", id))
		}
		return
	}

	out := projectStateFrom(*got, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
	resp.Diagnostics.Append(recordUID(ctx, resp.Private, out.uid)...)
}

func (r *projectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State, &req.State)
	var state projectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	got, err := r.meta.Client.GetProjectInOrgWithResponse(ctx,
		state.OrganizationID.ValueString(), state.ID.ValueString())
	if err != nil {
		addAPIError(&resp.Diagnostics, "read the project", nil, nil, err)
		return
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		// Deleted outside the provider. Absent, not an error: the next plan
		// proposes to create it.
		resp.State.RemoveResource(ctx)
		return
	}
	if got.JSON200 == nil {
		addAPIError(&resp.Diagnostics, "read the project", got.HTTPResponse, got.Body, nil)
		return
	}

	out := projectStateFrom(*got.JSON200, state)
	if !refreshedOwnObject(ctx, req.Private, resp, "project", state.ID.ValueString(), out.uid) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}

func (r *projectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan, state projectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	md, changed, diags := plan.metadata().patch(ctx, state.metadata())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !changed {
		// Only `timeouts` changed, which needs no request, and the contract
		// refuses an empty PATCH. `allow_replace` alone never reaches here
		// (replace_guard_resource.go).
		state.Timeouts = plan.Timeouts
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}
	body := k0rdent.ProjectUpdateRequest{
		DisplayName: md.DisplayName,
		Description: md.Description,
	}
	updated, err := r.meta.Client.UpdateProjectInOrgWithResponse(ctx,
		state.OrganizationID.ValueString(), state.ID.ValueString(), body)
	if err != nil {
		addAPIError(&resp.Diagnostics, "update the project", nil, nil, err)
		return
	}
	if updated.JSON200 == nil {
		addAPIError(&resp.Diagnostics, "update the project", updated.HTTPResponse, updated.Body, nil)
		return
	}

	out := projectStateFrom(*updated.JSON200, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}

func (r *projectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state projectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := withTimeout(ctx, state.Timeouts.Delete, projectStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}
	org, id := state.OrganizationID.ValueString(), state.ID.ValueString()

	send := func(ctx context.Context) (*http.Response, []byte, error) {
		deleted, err := r.meta.Client.DeleteProjectInOrgWithResponse(ctx, org, id)
		if err != nil {
			return nil, nil, err
		}
		return deleted.HTTPResponse, deleted.Body, nil
	}
	read := func(ctx context.Context) (*k0rdent.Project, *http.Response, []byte, error) {
		got, err := r.meta.Client.GetProjectInOrgWithResponse(ctx, org, id)
		if err != nil {
			return nil, nil, nil, err
		}
		return got.JSON200, got.HTTPResponse, got.Body, nil
	}
	projectStates.destroy(ctx, r.meta.Wait, send, read, &resp.Diagnostics)
}

// projectStates is what a project's delete makes of each status it reports. A
// project is only waited on while it is deleted, and reports `status` where
// everything else reports `state`.
var projectStates = stateKind[k0rdent.Project]{
	name:     "project",
	resource: projectResourceSuffix,
	// deleteProjectInOrg answers 409 for two causes: the project is already
	// `deleting`, which a timed-out earlier destroy or a resent DELETE leaves
	// behind, or it still holds clusters, which the platform's message names.
	deleting: string(k0rdent.ProjectStatusDeleting),
	state: func(p k0rdent.Project) (string, bool) {
		return stateString(p.Status), stateString(p.Status) != ""
	},
	delete: stateTable{
		// A status the provider cannot read is not progress.
		missing: stateRule{verdict: unrecognised},
		states: map[string]stateRule{
			string(k0rdent.ProjectStatusDeleting):  {verdict: keepWaiting},
			string(k0rdent.ProjectStatusActive):    {verdict: failsTerminally, says: afterDelete},
			string(k0rdent.ProjectStatusSuspended): {verdict: failsTerminally, says: afterDelete},
			string(k0rdent.ProjectStatusDisabled):  {verdict: failsTerminally, says: afterDelete},
		},
	},
}

// ImportState takes `organization/project`, or a bare project id whose
// organization is looked up. A bare id that more than one organization holds is
// refused rather than guessed. An identity is the same two parts, named.
func (r *projectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	raw := req.ID
	if raw == "" && req.Identity != nil && !req.Identity.Raw.IsNull() {
		raw = projectIdentityImportID(ctx, req.Identity, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	org, id, ok := splitProjectImportID(raw)
	if !ok {
		// Local: the import id is checked before any request is made.
		resp.Diagnostics.AddError(
			"The import id is not in the expected form",
			fmt.Sprintf("Importing a %s needs `organization/project` or a project id, and %q is "+
				"neither. Both halves are lower-case letters, digits and hyphens.",
				ResourceName(projectResourceSuffix), raw))
		return
	}
	if org == "" {
		if org, ok = findProjectOrganization(ctx, r.meta, id, &resp.Diagnostics); !ok {
			return
		}
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("organization_id"), org)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}

// projectIdentityImportID turns an identity into the string form, so both go
// through one check. A slash in either part would make a different string id,
// so it's refused here, naming the attribute it was in.
func projectIdentityImportID(ctx context.Context, identity *tfsdk.ResourceIdentity, diags *diag.Diagnostics) string {
	var org, id types.String
	diags.Append(identity.GetAttribute(ctx, path.Root("organization_id"), &org)...)
	diags.Append(identity.GetAttribute(ctx, path.Root("id"), &id)...)
	for name, v := range map[string]string{"organization_id": org.ValueString(), "id": id.ValueString()} {
		if strings.Contains(v, "/") {
			diags.AddAttributeError(path.Root(name), "The import identity is not in the expected form",
				fmt.Sprintf("`%s` is %q, and it's a single id, with no slash: lower-case letters, digits "+
					"and hyphens.", name, v))
		}
	}
	if diags.HasError() {
		return ""
	}
	if org.ValueString() == "" {
		return id.ValueString()
	}
	return org.ValueString() + "/" + id.ValueString()
}

// IdentitySchema is what an import block's `identity` takes. A project is
// named by its organization rather than a region and project, and the
// organization is optional for the same reason it is in the string form: a
// bare id is looked up.
func (r *projectResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = identityschema.Schema{Attributes: map[string]identityschema.Attribute{
		"organization_id": identityschema.StringAttribute{
			OptionalForImport: true,
			Description: "Id of the organization that holds the project. Left out on import, it is " +
				"looked up, and an id more than one organization holds is refused.",
		},
		"id": identityschema.StringAttribute{
			RequiredForImport: true,
			Description:       "The project's id.",
		},
	}}
}

// splitProjectImportID returns an empty org for a bare id. Both halves become
// segments of the request URL, and an import id writes straight to state
// without passing the schema validator, so they are checked here.
func splitProjectImportID(raw string) (org, id string, ok bool) {
	parts := strings.Split(raw, "/")
	switch len(parts) {
	case 1:
		id = parts[0]
	case 2:
		org, id = parts[0], parts[1]
		if !pathSegment.MatchString(org) {
			return "", "", false
		}
	default:
		return "", "", false
	}
	if !pathSegment.MatchString(id) {
		return "", "", false
	}
	return org, id, true
}

// findProjectOrganization looks up the organization that holds a live project.
func findProjectOrganization(ctx context.Context, meta *Meta, id string, diags *diag.Diagnostics) (string, bool) {
	p, ok := findProject(ctx, meta, id, diags)
	if !ok {
		return "", false
	}
	return stateString(p.OrganizationId), true
}

// findProject finds a live project by its id alone, from the project list.
// `search` is a substring match, so only an exact id counts. The contract
// promises an id is unique inside an organization, not across them, so every
// page is read and a second organization holding the id is an error. Taking
// the first match would adopt the wrong project, and a later destroy would
// delete it.
//
// A list row is the same Project a read by id answers, so the row is the
// project. The list is also the one project read a credential scoped to a
// project can make: it shows that credential its own projects, and the read by
// id is checked against the organization.
func findProject(ctx context.Context, meta *Meta, id string, diags *diag.Diagnostics) (k0rdent.Project, bool) {
	search := id
	params := k0rdent.ListProjectsParams{Search: &search}
	rows, err := readProjects(ctx, meta, params)
	var unended errListDidNotEnd
	switch {
	case errors.As(err, &unended):
		// The bound alone doesn't say what to do about it, and here there is
		// a way round it.
		diags.AddError("The list of projects did not end",
			fmt.Sprintf("%s So there's no telling whether another organization also holds %q. Name "+
				"the organization: `organization/project` when importing, or `organization_id` when "+
				"reading it with the data source.", unended.Error(), id))
		return k0rdent.Project{}, false
	case err != nil:
		addListError(diags, "projects", err)
		return k0rdent.Project{}, false
	}
	var orgs []string
	var found k0rdent.Project
	for _, p := range rows {
		if o := stateString(p.OrganizationId); stateString(p.Id) == id && o != "" && !slices.Contains(orgs, o) {
			orgs = append(orgs, o)
			found = p
		}
	}
	sort.Strings(orgs)
	switch len(orgs) {
	case 1:
		return found, true
	case 0:
	default:
		// Local: the list answered with the id in more than one organization.
		diags.AddError(
			"More than one organization has this project id",
			redactDiagnosticText(fmt.Sprintf("Project %q exists in %s. Name the organization: `organization/project` "+
				"when importing, or `organization_id` when reading it with the data source.",
				id, strings.Join(orgs, ", "))))
		return k0rdent.Project{}, false
	}
	// Local: the list answered, and nothing in it is this project.
	diags.AddError(
		"No such project",
		fmt.Sprintf("No project with id %q is visible to this credential. Check the id, or name "+
			"the organization as well if the credential cannot list projects.", id))
	return k0rdent.Project{}, false
}

// projectStateFrom maps a Project onto the model.
//
// `organizationId` is taken from the request rather than the response where the
// response omits it. Every field on Project is a pointer because the whole type
// is `readOnly`, so a nil is a contract violation the generated client accepts
// silently, and the organization is the one field this resource cannot function
// without. It is also the one field the caller definitely knows, because it was
// in the URL.
func projectStateFrom(p k0rdent.Project, prior projectModel) projectModel {
	md := prior.metadata().read(metadataAnswer{DisplayName: p.DisplayName, Description: p.Description})
	out := projectModel{

		ID:             stringOrNull(p.Id),
		OrganizationID: prior.OrganizationID,
		DisplayName:    md.DisplayName,
		Description:    md.Description,
		SelfLink:       stringOrNull(p.SelfLink),
		Status:         types.StringNull(),
		Timeouts:       prior.Timeouts,
		uid:            uidOf(p.Uid),
	}
	if p.OrganizationId != nil {
		out.OrganizationID = types.StringValue(*p.OrganizationId)
	}
	if p.Status != nil {
		out.Status = types.StringValue(string(*p.Status))
	}
	// A project made before regions were required reads back with none, and
	// keeps what state had. Otherwise the platform's answer is where the
	// project is.
	out.Region = prior.Region
	if p.Region != nil && *p.Region != "" {
		out.Region = types.StringValue(*p.Region)
	}
	return out
}
