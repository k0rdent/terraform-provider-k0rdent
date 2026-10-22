package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	listschema "github.com/hashicorp/terraform-plugin-framework/list/schema"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// listSpecs is one spec per managed resource, in the order Resources registers
// them. Every managed resource has one: each platform type it manages has a
// list operation the contract marks public. TestEveryResourceHasAListResource
// holds the two lists together.
func listSpecs() []listSpec {
	return []listSpec{
		clusterListSpec(),
		instanceGroupListSpec(),
		networkListSpec(),
		subnetListSpec(),
		securityGroupListSpec(),
		vpcPeeringListSpec(),
		vpcSecurityGroupsListSpec(),
		fileStoreListSpec(),
		projectListSpec(),
		webhookSubscriptionListSpec(),
	}
}

const goingAwayNote = "An object being deleted is left out: adopting it plans against something " +
	"about to be gone."

func clusterListSpec() listSpec {
	return listSpec{
		suffix:      clusterResourceSuffix,
		newResource: newClusterResource,
		description: "Every cluster in a project. " + goingAwayNote,
		attributes:  scopedAttributes(nil),
		find: func(ctx context.Context, meta *Meta, config listConfig, diags *diag.Diagnostics) []listedObject {
			target, ok := listScope(meta, config, diags)
			if !ok {
				return nil
			}
			rows, ok := listClusters(ctx, meta, target, diags)
			if !ok {
				return nil
			}
			var out []listedObject
			for _, c := range rows {
				if c.Id == "" || goingAway(stateString(c.State)) {
					continue
				}
				out = append(out, listedObject{scopedIdentity(target, "id", c.Id), displayName(c.DisplayName, c.Id)})
			}
			return out
		},
	}
}

func instanceGroupListSpec() listSpec {
	return listSpec{
		suffix:      instanceGroupResourceSuffix,
		newResource: newInstanceGroupResource,
		description: "Every instance group in a project. " + goingAwayNote,
		attributes:  scopedAttributes(nil),
		find: func(ctx context.Context, meta *Meta, config listConfig, diags *diag.Diagnostics) []listedObject {
			target, ok := listScope(meta, config, diags)
			if !ok {
				return nil
			}
			rows, ok := listInstanceGroups(ctx, meta, target, diags)
			if !ok {
				return nil
			}
			var out []listedObject
			for _, g := range rows {
				if g.Id == "" || goingAway(stateString(g.State)) {
					continue
				}
				out = append(out, listedObject{scopedIdentity(target, "id", g.Id), displayName(g.DisplayName, g.Id)})
			}
			return out
		},
	}
}

func networkListSpec() listSpec {
	return listSpec{
		suffix:      networkResourceSuffix,
		newResource: newNetworkResource,
		description: "Every network in a project that a tenant made. " + goingAwayNote,
		attributes: scopedAttributes(map[string]listschema.Attribute{
			"include_platform_owned": listPlatformOwnedAttribute("networks", "cluster_id"),
		}),
		find: func(ctx context.Context, meta *Meta, config listConfig, diags *diag.Diagnostics) []listedObject {
			target, ok := listScope(meta, config, diags)
			owned := config.flag("include_platform_owned", diags)
			if !ok || diags.HasError() {
				return nil
			}
			rows, ok := listNetworks(ctx, meta, target, diags)
			if !ok {
				return nil
			}
			var out []listedObject
			for _, n := range rows {
				view, err := networkViewOf(n)
				if err != nil {
					// A kind this build can't read can't be imported either,
					// so it's named and left out rather than failing the rest.
					diags.AddWarning("A network in this project was left out", redactDiagnosticText(err.Error()))
					continue
				}
				if view.id == "" || goingAway(view.state) || (view.clusterID != nil && !owned) {
					continue
				}
				out = append(out, listedObject{scopedIdentity(target, "id", view.id), displayName(view.displayName, view.id)})
			}
			return out
		},
	}
}

func subnetListSpec() listSpec {
	return listSpec{
		suffix:      subnetResourceSuffix,
		newResource: newSubnetResource,
		description: "Every subnet a tenant made, on one network or on every network in a project. " +
			goingAwayNote,
		attributes: scopedAttributes(map[string]listschema.Attribute{
			"network": listschema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Network to search. Left out, every network in the project is searched.",
				Validators:          idValidators(),
			},
			"include_platform_owned": listPlatformOwnedAttribute("subnets", "cluster_id"),
		}),
		find: func(ctx context.Context, meta *Meta, config listConfig, diags *diag.Diagnostics) []listedObject {
			target, scoped := listScope(meta, config, diags)
			owned := config.flag("include_platform_owned", diags)
			network, named := known(config.str("network", diags))
			if !scoped || diags.HasError() {
				return nil
			}
			networks := []string{network}
			if !named {
				rows, ok := listNetworks(ctx, meta, target, diags)
				if !ok {
					return nil
				}
				networks = networks[:0]
				for _, n := range rows {
					// A network of a kind this build can't read is skipped.
					// Naming it in `network` still lists what's under it.
					if view, err := networkViewOf(n); err == nil && view.id != "" && !goingAway(view.state) {
						networks = append(networks, view.id)
					}
				}
			}
			var out []listedObject
			found := underEach(networks, "network", !named, diags, func(net string, d *diag.Diagnostics) bool {
				rows, ok := listSubnets(ctx, meta, target, net, d)
				for _, s := range rows {
					id := deref(s.Id)
					if id == "" || goingAway(stateString(s.State)) || (s.ClusterId != nil && !owned) {
						continue
					}
					out = append(out, listedObject{scopedIdentity(target, "id", id, "network", net),
						displayName(s.DisplayName, id)})
				}
				return ok
			})
			if !found {
				return nil
			}
			return out
		},
	}
}

func securityGroupListSpec() listSpec {
	return listSpec{
		suffix:      securityGroupResourceSuffix,
		newResource: newSecurityGroupResource,
		description: "Every security group in a project that a tenant made. " + goingAwayNote,
		attributes: scopedAttributes(map[string]listschema.Attribute{
			"include_platform_owned": listPlatformOwnedAttribute("security groups, each VPC's default,", "owner_kind"),
		}),
		find: func(ctx context.Context, meta *Meta, config listConfig, diags *diag.Diagnostics) []listedObject {
			target, ok := listScope(meta, config, diags)
			owned := config.flag("include_platform_owned", diags)
			if !ok || diags.HasError() {
				return nil
			}
			rows, ok := listSecurityGroups(ctx, meta, target, k0rdent.ListSecurityGroupsParams{}, diags)
			if !ok {
				return nil
			}
			var out []listedObject
			for _, g := range rows {
				if g.Id == "" || goingAway(stateString(g.State)) || (g.OwnerKind != nil && !owned) {
					continue
				}
				out = append(out, listedObject{scopedIdentity(target, "id", g.Id), displayName(g.DisplayName, g.Id)})
			}
			return out
		},
	}
}

// underEach runs one list under each parent. walked says the parents came
// from a list of their own rather than from the configuration: then one gone
// since that list, which the call reports as its parent missing, is named in a
// warning and skipped, since a walk shouldn't fail on what it was racing. Any
// other failure, and any failure under a parent the configuration named, ends
// it.
func underEach(parents []string, noun string, walked bool, diags *diag.Diagnostics, each func(parent string, d *diag.Diagnostics) bool) bool {
	for _, parent := range parents {
		var d diag.Diagnostics
		ok := each(parent, &d)
		switch {
		case ok:
			diags.Append(d...)
		case walked && parentGone(d):
			diags.AddWarning(fmt.Sprintf("The %s %q went away during the search", noun, parent),
				fmt.Sprintf("It was listed, then answered 404 to the list under it, so nothing under %s %q is in "+
					"the results.", noun, parent))
		default:
			diags.Append(d...)
			return false
		}
	}
	return true
}

// parentGone is whether every error says the parent a list's path named
// wasn't there.
func parentGone(d diag.Diagnostics) bool {
	errs := d.Errors()
	if len(errs) == 0 {
		return false
	}
	for _, e := range errs {
		switch e.Summary() {
		case noSuchNetwork, noSuchVPC, noSuchScope, noSuchServiceAccount:
		default:
			return false
		}
	}
	return true
}

// vpcsIn is every VPC in a project not being deleted.
func vpcsIn(ctx context.Context, meta *Meta, target clusterTarget, diags *diag.Diagnostics) ([]k0rdent.VPC, bool) {
	rows, ok := listVPCs(ctx, meta, target, k0rdent.ListVPCsParams{}, diags)
	if !ok {
		return nil, false
	}
	out := rows[:0]
	for _, v := range rows {
		if v.Id != "" && !goingAway(stateString(v.State)) {
			out = append(out, v)
		}
	}
	return out, true
}

// vpcPeeringListSpec builds its peering with no default_tags: they only reach
// the tags plan modifier, and a list result is an import and a read, which
// never plan.
func vpcPeeringListSpec() listSpec {
	return listSpec{
		suffix:      vpcPeeringResourceSuffix,
		newResource: vpcPeeringResourceWith(nil),
		description: "Every peering on one VPC, or on every VPC in a project. " + goingAwayNote,
		attributes: scopedAttributes(map[string]listschema.Attribute{
			"vpc": listschema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "VPC to search. Left out, every VPC in the project is searched.",
				Validators:          idValidators(),
			},
		}),
		find: func(ctx context.Context, meta *Meta, config listConfig, diags *diag.Diagnostics) []listedObject {
			target, scoped := listScope(meta, config, diags)
			vpc, named := known(config.str("vpc", diags))
			if !scoped || diags.HasError() {
				return nil
			}
			vpcs := []string{vpc}
			if !named {
				rows, ok := vpcsIn(ctx, meta, target, diags)
				if !ok {
					return nil
				}
				vpcs = vpcs[:0]
				for _, v := range rows {
					vpcs = append(vpcs, v.Id)
				}
			}
			var out []listedObject
			found := underEach(vpcs, "VPC", !named, diags, func(v string, d *diag.Diagnostics) bool {
				rows, ok := listVPCPeerings(ctx, meta, target, v, d)
				for _, p := range rows {
					id := deref(p.Id)
					if id == "" || goingAway(stateString(p.State)) {
						continue
					}
					out = append(out, listedObject{scopedIdentity(target, "id", id, "vpc", v), displayName(p.DisplayName, id)})
				}
				return ok
			})
			if !found {
				return nil
			}
			return out
		},
	}
}

func vpcSecurityGroupsListSpec() listSpec {
	return listSpec{
		suffix:      vpcSecurityGroupsResourceSuffix,
		newResource: newVPCSecurityGroupsResource,
		description: "The security group binding of every VPC in a project. Every VPC has one, holding " +
			"its default group until something else is bound, so this finds one per VPC. " + goingAwayNote,
		attributes: scopedAttributes(nil),
		find: func(ctx context.Context, meta *Meta, config listConfig, diags *diag.Diagnostics) []listedObject {
			target, ok := listScope(meta, config, diags)
			if !ok {
				return nil
			}
			rows, ok := vpcsIn(ctx, meta, target, diags)
			if !ok {
				return nil
			}
			out := make([]listedObject, 0, len(rows))
			for _, v := range rows {
				out = append(out, listedObject{scopedIdentity(target, "vpc", v.Id), displayName(v.DisplayName, v.Id)})
			}
			return out
		},
	}
}

func fileStoreListSpec() listSpec {
	return listSpec{
		suffix:      fileStoreResourceSuffix,
		newResource: newFileStoreResource,
		description: "Every file store in a project. " + goingAwayNote,
		attributes:  scopedAttributes(nil),
		find: func(ctx context.Context, meta *Meta, config listConfig, diags *diag.Diagnostics) []listedObject {
			target, ok := listScope(meta, config, diags)
			if !ok {
				return nil
			}
			rows, ok := listFileStores(ctx, meta, target, diags)
			if !ok {
				return nil
			}
			var out []listedObject
			for _, s := range rows {
				if s.Id == "" || goingAway(stateString(s.State)) {
					continue
				}
				out = append(out, listedObject{scopedIdentity(target, "id", s.Id), displayName(s.DisplayName, s.Id)})
			}
			return out
		},
	}
}

// listOrganizationAttribute is the organization filter, required where the
// operation needs one in its path.
func listOrganizationAttribute(required bool, what string) listschema.StringAttribute {
	return listschema.StringAttribute{
		Required:            required,
		Optional:            !required,
		MarkdownDescription: "Organization to search. " + what,
		Validators:          idValidators(),
	}
}

func projectListSpec() listSpec {
	return listSpec{
		suffix:      projectResourceSuffix,
		newResource: newProjectResource,
		description: "Every project the provider's credential can see, in one organization or in all " +
			"of them. " + goingAwayNote,
		attributes: map[string]listschema.Attribute{
			"organization_id": listOrganizationAttribute(false,
				"Left out, every organization the credential can see is searched."),
		},
		find: func(ctx context.Context, meta *Meta, config listConfig, diags *diag.Diagnostics) []listedObject {
			var params k0rdent.ListProjectsParams
			if org, ok := known(config.str("organization_id", diags)); ok {
				params.OrganizationId = &org
			}
			if diags.HasError() {
				return nil
			}
			rows, ok := listProjects(ctx, meta, params, diags)
			if !ok {
				return nil
			}
			var out []listedObject
			for _, p := range rows {
				id, org := deref(p.Id), deref(p.OrganizationId)
				if id == "" || org == "" || goingAway(stateString(p.Status)) {
					continue
				}
				out = append(out, listedObject{map[string]string{"organization_id": org, "id": id},
					displayName(p.DisplayName, id)})
			}
			return out
		},
	}
}

func webhookSubscriptionListSpec() listSpec {
	return listSpec{
		suffix:      webhookSubscriptionResourceSuffix,
		newResource: newWebhookSubscriptionResource,
		description: "Every webhook subscription the provider's credential can see. Each is shown by " +
			"its id, never its URL, since a receiver's URL can carry a token: `url` is in the object " +
			"`include_resource` returns, and in state once adopted, as it always was. An adopted " +
			"subscription has no signing secret in state, since the platform " +
			"answers it only when it's set: the import guide says what a configuration that sets one " +
			"plans.",
		attributes: map[string]listschema.Attribute{},
		find: func(ctx context.Context, meta *Meta, _ listConfig, diags *diag.Diagnostics) []listedObject {
			rows, d := listWebhookSubscriptions(ctx, meta)
			diags.Append(d...)
			if diags.HasError() {
				return nil
			}
			out := make([]listedObject, 0, len(rows))
			for _, s := range rows {
				id := s.Id.String()
				out = append(out, listedObject{map[string]string{"id": id}, id})
			}
			return out
		},
	}
}
