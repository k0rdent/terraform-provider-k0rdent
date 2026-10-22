package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The summaries of the refusals a list call gives when the parent its path
// names isn't there. A walk over every parent reads these as a parent gone
// since it was listed (parentGone), where a parent the configuration named
// stays an error.
const (
	noSuchNetwork        = "No such network"
	noSuchVPC            = "No such VPC"
	noSuchScope          = "No such scope"
	noSuchServiceAccount = "No such service account"
)

// The list calls the plural data sources and the list resources share. Each
// reads every page through pages.go, so the two can't disagree about where a
// list ends or what a failed page says. The order is the platform's: a caller
// that shows the rows sorts them its own way. A read call is its list call
// returning the error rather than a diagnostic, for a caller that reports a
// failure its own way, on the same pager.

func listNetworks(ctx context.Context, meta *Meta, target clusterTarget, diags *diag.Diagnostics) ([]k0rdent.Network, bool) {
	return listByOffset(diags, "networks", networkKey, func(offset, limit int32) (page[k0rdent.Network], error) {
		got, err := meta.Client.ListNetworksWithResponse(ctx, target.region, target.project,
			&k0rdent.ListNetworksParams{Offset: &offset, Limit: &limit})
		if err != nil {
			return page[k0rdent.Network]{}, err
		}
		if got.JSON200 == nil {
			return page[k0rdent.Network]{}, unreadPage(got.HTTPResponse, got.Body)
		}
		return page[k0rdent.Network]{items: got.JSON200.Networks, total: got.JSON200.TotalCount}, nil
	})
}

func listSubnets(ctx context.Context, meta *Meta, target clusterTarget, network string, diags *diag.Diagnostics) ([]k0rdent.Subnet, bool) {
	return listByOffset(diags, "subnets", func(s k0rdent.Subnet) string { return deref(s.Id) },
		func(offset, limit int32) (page[k0rdent.Subnet], error) {
			got, err := meta.Client.ListSubnetsWithResponse(ctx, target.region, target.project, network,
				&k0rdent.ListSubnetsParams{Offset: &offset, Limit: &limit})
			if err != nil {
				return page[k0rdent.Subnet]{}, err
			}
			if isNotFound(got.HTTPResponse, got.Body) {
				// Local: the provider's reading of a 404 on a path it built from
				// configuration. The upstream body says only "not found", and
				// naming the three values that make up the path is the useful part.
				return page[k0rdent.Subnet]{}, refusePage(noSuchNetwork,
					fmt.Sprintf("No network named %q in project %q of region %q.", network, target.project, target.region))
			}
			if got.JSON200 == nil {
				return page[k0rdent.Subnet]{}, unreadPage(got.HTTPResponse, got.Body)
			}
			return page[k0rdent.Subnet]{items: got.JSON200.Subnets, total: got.JSON200.TotalCount}, nil
		})
}

// listSecurityGroups reads every group params matches. Its `state` is the one
// filter the operation takes.
func listSecurityGroups(ctx context.Context, meta *Meta, target clusterTarget, params k0rdent.ListSecurityGroupsParams, diags *diag.Diagnostics) ([]k0rdent.SecurityGroup, bool) {
	return listByOffset(diags, "security groups", func(g k0rdent.SecurityGroup) string { return g.Id },
		func(offset, limit int32) (page[k0rdent.SecurityGroup], error) {
			params.Offset, params.Limit = &offset, &limit
			got, err := meta.Client.ListSecurityGroupsWithResponse(ctx, target.region, target.project, &params)
			if err != nil {
				return page[k0rdent.SecurityGroup]{}, err
			}
			if got.JSON200 == nil {
				return page[k0rdent.SecurityGroup]{}, unreadPage(got.HTTPResponse, got.Body)
			}
			return page[k0rdent.SecurityGroup]{items: got.JSON200.SecurityGroups, total: got.JSON200.TotalCount}, nil
		})
}

// listVPCs reads every VPC the filters in params match.
func listVPCs(ctx context.Context, meta *Meta, target clusterTarget, params k0rdent.ListVPCsParams, diags *diag.Diagnostics) ([]k0rdent.VPC, bool) {
	return listByOffset(diags, "VPCs", func(v k0rdent.VPC) string { return v.Id }, func(offset, limit int32) (page[k0rdent.VPC], error) {
		params.Offset, params.Limit = &offset, &limit
		got, err := meta.Client.ListVPCsWithResponse(ctx, target.region, target.project, &params)
		if err != nil {
			return page[k0rdent.VPC]{}, err
		}
		if got.JSON200 == nil {
			return page[k0rdent.VPC]{}, unreadPage(got.HTTPResponse, got.Body)
		}
		return page[k0rdent.VPC]{items: got.JSON200.Vpcs, total: got.JSON200.TotalCount}, nil
	})
}

func listAddressPools(ctx context.Context, meta *Meta, region string, diags *diag.Diagnostics) ([]k0rdent.AddressPool, bool) {
	return listByOffset(diags, "address pools", func(p k0rdent.AddressPool) string { return deref(p.Id) },
		func(offset, limit int32) (page[k0rdent.AddressPool], error) {
			got, err := meta.Client.ListAddressPoolsWithResponse(ctx, region, &k0rdent.ListAddressPoolsParams{Offset: &offset, Limit: &limit})
			if err != nil {
				return page[k0rdent.AddressPool]{}, err
			}
			if got.JSON200 == nil {
				return page[k0rdent.AddressPool]{}, unreadPage(got.HTTPResponse, got.Body)
			}
			return page[k0rdent.AddressPool]{items: got.JSON200.AddressPools, total: got.JSON200.TotalCount}, nil
		})
}

func listVPCPeerings(ctx context.Context, meta *Meta, target clusterTarget, vpc string, diags *diag.Diagnostics) ([]k0rdent.VPCPeering, bool) {
	rows, err := readVPCPeerings(ctx, meta, target, vpc)
	var resp pageResponse
	switch {
	case errors.As(err, &resp) && isNotFound(resp.resp, resp.body):
		// Local: the provider's reading of a 404 on a path it built from
		// configuration. Naming the VPC is the useful part.
		diags.AddError(noSuchVPC,
			fmt.Sprintf("No VPC named %q in project %q of region %q.", vpc, target.project, target.region))
		return nil, false
	case err != nil:
		addListError(diags, "VPC peerings", err)
		return nil, false
	}
	return rows, true
}

// readVPCPeerings is listVPCPeerings returning the error, for the check on a
// peering's other side, which turns any failure into a warning.
func readVPCPeerings(ctx context.Context, meta *Meta, target clusterTarget, vpc string) ([]k0rdent.VPCPeering, error) {
	return byToken(catalog, "VPC peerings", func(token *string, size *int) (page[k0rdent.VPCPeering], error) {
		got, err := meta.Client.ListVPCPeeringsWithResponse(ctx, target.region, target.project, vpc,
			&k0rdent.ListVPCPeeringsParams{PageToken: token, PageSize: size})
		if err != nil {
			return page[k0rdent.VPCPeering]{}, err
		}
		if got.JSON200 == nil {
			return page[k0rdent.VPCPeering]{}, unreadPage(got.HTTPResponse, got.Body)
		}
		return page[k0rdent.VPCPeering]{items: got.JSON200.Peerings, next: got.JSON200.NextPageToken}, nil
	})
}

// listProjects reads every project params matches.
func listProjects(ctx context.Context, meta *Meta, params k0rdent.ListProjectsParams, diags *diag.Diagnostics) ([]k0rdent.Project, bool) {
	rows, err := readProjects(ctx, meta, params)
	if err != nil {
		addListError(diags, "projects", err)
		return nil, false
	}
	return rows, true
}

// readProjects is listProjects returning the error, for the import lookup,
// which says its own thing at the bound, and the region check, which only
// logs a failure. Keyed on the organization too: one project id can be in two
// of them.
func readProjects(ctx context.Context, meta *Meta, params k0rdent.ListProjectsParams) ([]k0rdent.Project, error) {
	// This operation documents `offset` and `limit`, as the five networking
	// lists do. The other catalogs are still on `pageToken`.
	return readByOffset("projects",
		func(p k0rdent.Project) string { return deref(p.OrganizationId) + "/" + deref(p.Id) },
		func(offset, limit int32) (page[k0rdent.Project], error) {
			params.Offset, params.Limit = &offset, &limit
			got, err := meta.Client.ListProjectsWithResponse(ctx, &params)
			if err != nil {
				return page[k0rdent.Project]{}, err
			}
			if got.JSON200 == nil {
				return page[k0rdent.Project]{}, unreadPage(got.HTTPResponse, got.Body)
			}
			return page[k0rdent.Project]{items: rowsOf(got.JSON200.Projects), total: got.JSON200.TotalCount}, nil
		})
}

func listClusters(ctx context.Context, meta *Meta, target clusterTarget, diags *diag.Diagnostics) ([]k0rdent.Cluster, bool) {
	return listByToken(diags, "clusters", func(token *string, size *int) (page[k0rdent.Cluster], error) {
		got, err := meta.Client.ListClustersWithResponse(ctx, target.region, target.project,
			&k0rdent.ListClustersParams{PageToken: token, PageSize: size})
		if err != nil {
			return page[k0rdent.Cluster]{}, err
		}
		if got.JSON200 == nil {
			return page[k0rdent.Cluster]{}, unreadPage(got.HTTPResponse, got.Body)
		}
		return page[k0rdent.Cluster]{items: got.JSON200.Clusters, next: got.JSON200.NextPageToken}, nil
	})
}

// listInstanceGroups leaves out the groups already deleted. `showDeleted`
// defaults to false, and is left to.
func listInstanceGroups(ctx context.Context, meta *Meta, target clusterTarget, diags *diag.Diagnostics) ([]k0rdent.InstanceGroup, bool) {
	return listByToken(diags, "instance groups", func(token *string, size *int) (page[k0rdent.InstanceGroup], error) {
		got, err := meta.Client.ListInstanceGroupsWithResponse(ctx, target.region, target.project,
			&k0rdent.ListInstanceGroupsParams{PageToken: token, PageSize: size})
		if err != nil {
			return page[k0rdent.InstanceGroup]{}, err
		}
		if got.JSON200 == nil {
			return page[k0rdent.InstanceGroup]{}, unreadPage(got.HTTPResponse, got.Body)
		}
		return page[k0rdent.InstanceGroup]{items: got.JSON200.InstanceGroups, next: got.JSON200.NextPageToken}, nil
	})
}

func listFileStores(ctx context.Context, meta *Meta, target clusterTarget, diags *diag.Diagnostics) ([]k0rdent.FileStore, bool) {
	return listByToken(diags, "file stores", func(token *string, size *int) (page[k0rdent.FileStore], error) {
		got, err := meta.Client.ListFileStoresWithResponse(ctx, target.region, target.project,
			&k0rdent.ListFileStoresParams{PageToken: token, PageSize: size})
		if err != nil {
			return page[k0rdent.FileStore]{}, err
		}
		if got.JSON200 == nil {
			return page[k0rdent.FileStore]{}, unreadPage(got.HTTPResponse, got.Body)
		}
		return page[k0rdent.FileStore]{items: rowsOf(got.JSON200.FileStores), next: got.JSON200.NextPageToken}, nil
	})
}

// listServiceAccounts reads the accounts at one scope. The operation requires
// both `scope` and `scopeId`, so there's no reading every account in an
// organization at once.
func listServiceAccounts(ctx context.Context, meta *Meta, org string, scope k0rdent.ListServiceAccountsParamsScope, scopeID string, diags *diag.Diagnostics) ([]k0rdent.ServiceAccount, bool) {
	params := k0rdent.ListServiceAccountsParams{Scope: scope, ScopeId: scopeID}
	return listByOffset(diags, "service accounts", func(sa k0rdent.ServiceAccount) string { return sa.Id },
		func(offset, limit int32) (page[k0rdent.ServiceAccount], error) {
			params.Offset, params.Limit = &offset, &limit
			got, err := meta.Client.ListServiceAccountsWithResponse(ctx, org, &params)
			if err != nil {
				return page[k0rdent.ServiceAccount]{}, err
			}
			if isNotFound(got.HTTPResponse, got.Body) {
				// Local: the provider's reading of a 404 on a path and
				// scope it built. Naming the scope is the useful part.
				return page[k0rdent.ServiceAccount]{}, refusePage(noSuchScope,
					fmt.Sprintf("No %s %q in organization %q to list service accounts at.", scope, scopeID, org))
			}
			if got.JSON200 == nil {
				return page[k0rdent.ServiceAccount]{}, unreadPage(got.HTTPResponse, got.Body)
			}
			return page[k0rdent.ServiceAccount]{items: got.JSON200.ServiceAccounts, total: got.JSON200.TotalCount}, nil
		})
}

func listServiceAccountCredentials(ctx context.Context, meta *Meta, org, account string, diags *diag.Diagnostics) ([]k0rdent.ServiceAccountCredential, bool) {
	var params k0rdent.ListServiceAccountCredentialsParams
	return listByOffset(diags, "credentials", func(c k0rdent.ServiceAccountCredential) string { return deref(c.Id) },
		func(offset, limit int32) (page[k0rdent.ServiceAccountCredential], error) {
			params.Offset, params.Limit = &offset, &limit
			got, err := meta.Client.ListServiceAccountCredentialsWithResponse(ctx, org, account, &params)
			if err != nil {
				return page[k0rdent.ServiceAccountCredential]{}, err
			}
			if isNotFound(got.HTTPResponse, got.Body) {
				// Local: as listSubnets, naming the account the path was built from.
				return page[k0rdent.ServiceAccountCredential]{}, refusePage(noSuchServiceAccount,
					fmt.Sprintf("No service account %q in organization %q.", account, org))
			}
			if got.JSON200 == nil {
				return page[k0rdent.ServiceAccountCredential]{}, unreadPage(got.HTTPResponse, got.Body)
			}
			return page[k0rdent.ServiceAccountCredential]{items: got.JSON200.Credentials, total: got.JSON200.TotalCount}, nil
		})
}
