package provider

import (
	"context"
	"fmt"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func isAdoptOnly(ctx context.Context, config tfsdk.Config) bool {
	if config.Schema == nil || config.Raw.IsNull() {
		return false
	}
	var value types.Bool
	return !config.GetAttribute(ctx, path.Root(adoptOnlyAttribute), &value).HasError() && value.ValueBool()
}

// Scope identifies what to adopt. Settings that control Terraform's next
// operation stay configurable. Everything else comes from the refresh or import.
var adoptInputs = []string{
	"cluster_type", "description", "display_name", "node_pool", "security_groups",
	"ssh_access", "storage_ceiling_gib", "tags",
}

func planAdoptedCluster(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() {
		return
	}
	var prior, configured, planned map[string]tftypes.Value
	if req.State.Raw.As(&prior) != nil || req.Config.Raw.As(&configured) != nil || resp.Plan.Raw.As(&planned) != nil {
		resp.Diagnostics.AddError("The adopt plan couldn't read cluster state", "Plan again to refresh the cluster before adopting it.")
		return
	}
	for _, name := range adoptInputs {
		observed, input := prior[name], configured[name]
		if !input.IsNull() && !observed.Equal(input) {
			if observed.IsNull() {
				resp.Diagnostics.AddAttributeError(path.Root(name), "The adopt plan can't keep an unset platform value",
					fmt.Sprintf("The platform doesn't report `%s`. Terraform can't plan an unset value over this configured value. Omit `%s` for the adopt apply.", name, name))
				continue
			}
			resp.Diagnostics.AddAttributeWarning(path.Root(name), "Adopt-only keeps the platform value for "+name,
				fmt.Sprintf("This run keeps the platform value for `%s`. Set `adopt_only = false` to apply a change to the cluster.", name))
		}
		planned[name] = observed
	}
	if value := configured["ssh_public_keys_wo"]; !value.IsNull() {
		resp.Diagnostics.AddAttributeWarning(path.Root("ssh_public_keys_wo"), "Adopt-only ignores configured SSH keys",
			"This run doesn't change or compare `ssh_public_keys_wo`. Set `adopt_only = false` to apply a change to the cluster.")
	}
	resp.RequiresReplace = slices.DeleteFunc(resp.RequiresReplace, func(p path.Path) bool {
		for _, name := range adoptInputs {
			if p.Equal(path.Root(name)) {
				return true
			}
		}
		return false
	})
	for name, attribute := range resp.Plan.Schema.GetAttributes() {
		if attribute.IsComputed() && !attribute.IsOptional() {
			planned[name] = prior[name]
		}
	}
	resp.Plan.Raw = tftypes.NewValue(resp.Plan.Raw.Type(), planned)
}

// Optional metadata is readable in adopt-only mode. In normal mode, an
// omitted value still clears that metadata, as it did before adoption.
func planUnconfiguredClusterMetadata(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if resp.Plan.Raw.IsNull() || req.Config.Raw.IsNull() {
		return
	}
	for name, zero := range map[string]any{
		"display_name": types.StringNull(),
		"description":  types.StringNull(),
		"tags":         types.MapNull(types.StringType),
	} {
		p := path.Root(name)
		var value attr.Value
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, p, &value)...)
		if value != nil && value.IsNull() {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, p, zero)...)
		}
	}
}
