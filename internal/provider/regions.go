package provider

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// A project's region, which v1.3.0-dev.4 makes part of every project. The
// project is created in one region and locked to it, and
// every compute, networking and storage route under the project has to name
// that region.

// regionIDPattern is the contract's pattern for ProjectCreateRequest.region.
// It's the id of a region, which, unlike a resource id, may start with a
// digit.
var regionIDPattern = regexp.MustCompile(k0rdent.ProjectCreateRequestRegionPattern)

// regionIDValidators checks a project's region against the contract at plan
// time. Whether the region exists, and can hold a project, is the region
// list's to say, which checkProjectRegion asks.
func regionIDValidators() []validator.String {
	return []validator.String{
		stringvalidator.LengthBetween(k0rdent.ProjectCreateRequestRegionMinLength, k0rdent.ProjectCreateRequestRegionMaxLength),
		stringvalidator.RegexMatches(regionIDPattern, "must be a region id: lower-case letters, digits and "+
			"hyphens, starting and ending with a letter or digit"),
	}
}

// regionListMemo is the region list this provider process read, shared by
// every project planned in it. Regions are operator-declared topology that
// doesn't move inside one plan. A failed read isn't kept, so the next caller
// asks again.
type regionListMemo struct {
	mu   sync.Mutex
	rows []k0rdent.Region
	held bool
}

// regions reads the region list once per process.
func (m *Meta) regions(ctx context.Context) ([]k0rdent.Region, error) {
	m.regionList.mu.Lock()
	defer m.regionList.mu.Unlock()
	if m.regionList.held {
		return m.regionList.rows, nil
	}
	got, err := m.Client.ListRegionsWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if got.JSON200 == nil {
		if err := k0rdent.CheckResponse(got.HTTPResponse, got.Body); err != nil {
			return nil, err
		}
		return nil, answeredWithout(got.HTTPResponse, "region list")
	}
	m.regionList.rows, m.regionList.held = got.JSON200.Regions, true
	return m.regionList.rows, nil
}

// projectRegionIDs is every region in rows that can hold a project, sorted.
func projectRegionIDs(rows []k0rdent.Region) []string {
	var out []string
	for _, r := range rows {
		if r.HostsProjects {
			out = append(out, r.Id)
		}
	}
	sort.Strings(out)
	return out
}

// whereProjectsGo is the sentence naming the regions a project can go in, as
// far as the region list says.
func whereProjectsGo(ctx context.Context, meta *Meta) string {
	lister := "The `" + ResourceName(regionsDataSourceSuffix) + "` data source lists every region: one " +
		"whose `hosts_projects` is true can hold a project."
	if meta == nil {
		return lister
	}
	rows, err := meta.regions(ctx)
	if err != nil {
		return lister
	}
	ids := projectRegionIDs(rows)
	if len(ids) == 0 {
		return "None of the regions this credential can list holds projects. " + lister
	}
	for i, id := range ids {
		ids[i] = oneLine(id)
	}
	return fmt.Sprintf("Regions that can hold a project: %s. %s", strings.Join(ids, ", "), lister)
}

// checkProjectRegion refuses at plan time a project region the region list
// says can't hold one. It's a better error than the platform's 422, and no
// more than that: a list it can't read leaves the decision to the create.
func checkProjectRegion(ctx context.Context, meta *Meta, region string, diags *diag.Diagnostics) {
	if meta == nil {
		return
	}
	rows, err := meta.regions(ctx)
	if err != nil {
		tflog.Debug(ctx, "could not read the region list, so the project's region is left to the create",
			map[string]any{"region": region, "error": redactDiagnosticText(err.Error())})
		return
	}
	for _, r := range rows {
		if r.Id == region && r.HostsProjects {
			return
		}
	}
	why := "isn't in the region list"
	for _, r := range rows {
		if r.Id == region {
			why = "is listed with `hostsProjects` false"
			if r.Id == globalRegion {
				why += ": on this installation `global` is the control-plane tier alone, and holds no " +
					"compute"
			}
		}
	}
	// Local: decided from the region list, which the platform answered.
	diags.AddAttributeError(path.Root("region"), "This region can't hold a project",
		redactDiagnosticText(fmt.Sprintf("The platform creates a project only in a region whose `hostsProjects` "+
			"is true, and %q %s. %s", region, why, whereProjectsGo(ctx, meta))))
}
