package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// Sweepers, in the terraform-plugin-testing style: each managed kind
// registers one with resource.AddTestSweepers, and
//
//	make sweep SWEEP_REGION=<region> SWEEP_CONFIRM=<endpoint host>
//
// runs the sweepers against whatever API the provider's environment names,
// children first. They exist for one case: an acceptance run against a real
// stack that was killed before its destroys ran. On finite hardware a leaked
// cluster holds machines somebody else needs, and nothing else would ever
// remove it.
//
// Everything this suite creates has an id starting with sweepPrefix, and that
// prefix is the only thing a sweeper deletes. A generated `tf-` id is never
// swept, even though a test that leaks one leaves it behind, because `tf-` is
// also what every tenant configuration without an id gets, and there's no
// telling a test's from anybody else's.

// sweepPrefix is the start of every id this suite gives an object.
const sweepPrefix = "acc-"

// sweepConfirm has to name the endpoint's host before anything is swept, as
// LIVE_CONFIRM does for `make live`, so an environment left pointing at the
// wrong stack fails rather than deleting from it.
const sweepConfirm = "SWEEP_CONFIRM"

// guardSweep is the rule every deletion goes through: an id that doesn't
// start with sweepPrefix is refused, whatever the list that found it said.
// It sits in sweepOne, on the path to the delete, rather than in the lists,
// so a list that matched more than it should can't get anything past it.
func guardSweep(kind, id string) error {
	prefix := sweepPrefix
	if strings.HasPrefix(id, prefix) && len(id) > len(prefix) {
		return nil
	}
	return fmt.Errorf("refusing to sweep %s %q: only an id that starts with %q is one this suite made",
		kind, id, prefix)
}

// ours is whether a listed object is a sweep candidate at all, so that a list
// full of other people's objects doesn't read as a list full of refusals.
func ours(id string) bool { return guardSweep("", id) == nil }

// sweeper is one kind's cleanup.
type sweeper struct {
	// kind is the resource suffix, so the registered name is the resource's
	// own type name.
	kind string
	// after names the kinds that have to be swept first, because the platform
	// refuses to delete this kind while one of those still references it.
	after []string
	sweep func(context.Context, *sweepRun) error
}

// sweepers is every kind's cleanup, in an order that already puts every kind
// after what it names in `after`, so runSweepers can walk it top to bottom.
// TestSweepersComeAfterWhatTheyNeed holds the order to that.
//
// The VPC security-group binding has none. It's a property of a VPC rather
// than an object, and the VPC goes with the cluster or group that owns it.
var sweepers = []sweeper{
	{kind: "file_store", sweep: sweepFileStores},
	{kind: "vpc_peering", sweep: sweepPeerings},
	{kind: "kubernetes_cluster", after: []string{"file_store", "vpc_peering"}, sweep: sweepClusters},
	{kind: "instance_group", after: []string{"file_store", "vpc_peering"}, sweep: sweepInstanceGroups},
	{kind: "subnet", after: []string{"kubernetes_cluster", "instance_group"}, sweep: sweepSubnets},
	{kind: "network", after: []string{"subnet"}, sweep: sweepNetworks},
	{kind: "security_group", after: []string{"kubernetes_cluster", "instance_group"}, sweep: sweepSecurityGroups},
	{kind: "project", after: []string{"network", "security_group"}, sweep: sweepProjects},
	{kind: "webhook_subscription", sweep: sweepWebhookSubscriptions},
}

func init() {
	for _, s := range sweepers {
		var deps []string
		for _, d := range s.after {
			deps = append(deps, provider.ResourceName(d))
		}
		resource.AddTestSweepers(provider.ResourceName(s.kind), &resource.Sweeper{
			Name:         provider.ResourceName(s.kind),
			Dependencies: deps,
			F: func(region string) error {
				run, err := sweepRunFor(region)
				if err != nil {
					return err
				}
				return s.sweep(context.Background(), run)
			},
		})
	}
}

// sweeping is whether `go test` was asked to sweep rather than test. The
// flag is terraform-plugin-testing's, so it has to be parsed first.
func sweeping() bool {
	f := flag.Lookup("sweep")
	return f != nil && f.Value.String() != ""
}

// runSweepers runs every sweeper for one region in order, which is how the
// acceptance run checks the sweepers, and returns every failure rather than
// the first.
func runSweepers(region string) error {
	run, err := sweepRunFor(region)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range sweepers {
		if err := s.sweep(context.Background(), run); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.kind, err))
		}
	}
	return errors.Join(errs...)
}

// sweepRun is what every sweeper in one region shares: one client, so one
// token mint, and the projects to look in.
type sweepRun struct {
	meta   *provider.Meta
	region string
	// projects is every project-scoped place a test object can be: the
	// configured project, and every project this suite made.
	projects []string
	// orgOf is the organization of each project this suite made, which is
	// what deleting one is addressed by.
	orgOf map[string]string
}

var (
	sweepRunsMu sync.Mutex
	sweepRuns   = map[string]*sweepRun{}
)

// sweepRunFor builds the shared state for a region once. A credential is
// allowed ten mints a minute, and eight sweepers each minting a token
// would spend most of that before deleting anything.
func sweepRunFor(region string) (*sweepRun, error) {
	sweepRunsMu.Lock()
	defer sweepRunsMu.Unlock()
	if run, ok := sweepRuns[region]; ok {
		return run, nil
	}

	// Without an endpoint the provider falls back to the production default.
	// A plan can do that safely and a sweep can't, so a sweep only goes where
	// it was pointed.
	if strings.TrimSpace(os.Getenv(provider.EnvVar("ENDPOINT"))) == "" {
		return nil, fmt.Errorf("a sweep deletes, so it needs %s set to the stack it's meant for, and it "+
			"never falls back to the provider's default endpoint", provider.EnvVar("ENDPOINT"))
	}
	// No Terraform runs a sweep, so it names no Terraform version. A pipeline
	// that names itself in TF_APPEND_USER_AGENT is named here too.
	meta, err := provider.MetaFromEnvironment(provider.UserAgent("sweep", "", os.Getenv("TF_APPEND_USER_AGENT")))
	if err != nil {
		return nil, fmt.Errorf("the provider's environment doesn't configure a client: %w", err)
	}
	target, err := url.Parse(meta.Endpoint)
	if err != nil {
		return nil, err
	}
	// The host with its port, as LIVE_CONFIRM is, since two stacks on one
	// machine differ only by port.
	// The refusal doesn't print the value to set: typing the host is the
	// confirmation, and a line to paste would make it a formality.
	if got := os.Getenv(sweepConfirm); got != target.Host {
		return nil, fmt.Errorf("sweeping deletes from %s. Set %s to that endpoint's host and port to "+
			"confirm it's the stack meant, as `make live` asks for LIVE_CONFIRM (it's %q now)",
			meta.Endpoint, sweepConfirm, got)
	}

	run := &sweepRun{meta: meta, region: region, orgOf: map[string]string{}}
	if meta.Project != "" {
		run.projects = append(run.projects, meta.Project)
	}
	ctx := context.Background()
	search := sweepPrefix
	rows, err := run.rows("projects", byOffset, func(offset int32, _ *string, limit int32) (int, []byte, error) {
		got, listErr := meta.Client.ListProjectsWithResponse(ctx, &k0rdent.ListProjectsParams{
			Search: &search, Offset: &offset, Limit: &limit,
		})
		if listErr != nil {
			return 0, nil, listErr
		}
		return got.StatusCode(), got.Body, nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	for _, p := range rows {
		if ours(p.ID) && p.OrganizationID != "" {
			run.orgOf[p.ID] = p.OrganizationID
			if !slices.Contains(run.projects, p.ID) {
				run.projects = append(run.projects, p.ID)
			}
		}
	}
	sweepRuns[region] = run
	return run, nil
}

// sweepRow is the part of any listed object a sweeper needs.
type sweepRow struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organizationId"`
	// OwnerKind is set on a security group the platform owns, which goes
	// with its VPC rather than being anything a sweep deletes.
	OwnerKind string `json:"ownerKind"`
	// Type tells a regional region from the global tier.
	Type string `json:"type"`
}

// paging is which of the two pagination styles a list is read with: the one
// the provider reads it with.
type paging int

const (
	byOffset paging = iota
	byToken
)

// pageFetch reads one page. Only the argument that matches the list's paging
// is meaningful.
type pageFetch func(offset int32, token *string, limit int32) (int, []byte, error)

// sweepPageSize is the contract's largest page, and sweepMaxPages bounds a
// server that keeps saying there's more.
const (
	sweepPageSize = 500
	sweepMaxPages = 1000
)

// rows reads every page of a list and returns its rows under key.
func (r *sweepRun) rows(key string, style paging, fetch pageFetch) ([]sweepRow, error) {
	var (
		all    []sweepRow
		offset int32
		token  *string
	)
	for range sweepMaxPages {
		code, body, err := fetch(offset, token, sweepPageSize)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, fmt.Errorf("the list answered %d: %s", code, body)
		}
		var page map[string]json.RawMessage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, err
		}
		var rows []sweepRow
		if raw, ok := page[key]; ok {
			if err := json.Unmarshal(raw, &rows); err != nil {
				return nil, err
			}
		}
		all = append(all, rows...)

		var next struct {
			NextPageToken *string `json:"nextPageToken"`
			TotalCount    *int32  `json:"totalCount"`
		}
		if err := json.Unmarshal(body, &next); err != nil {
			return nil, err
		}
		switch style {
		case byToken:
			if next.NextPageToken == nil || *next.NextPageToken == "" {
				return all, nil
			}
			token = next.NextPageToken
		case byOffset:
			offset += int32(len(rows)) //nolint:gosec // a page is at most sweepPageSize rows
			if len(rows) == 0 || (next.TotalCount != nil && offset >= *next.TotalCount) ||
				(next.TotalCount == nil && len(rows) < sweepPageSize) {
				return all, nil
			}
		}
	}
	return nil, fmt.Errorf("the %s list was still going after %d pages", key, sweepMaxPages)
}

// sweepOne deletes one object and waits for it to stop resolving. It's the
// only way any sweeper deletes or changes anything, and guardSweep is the
// first thing in it.
func (r *sweepRun) sweepOne(ctx context.Context, kind, id string, del, get func(context.Context) (int, error)) error {
	if err := guardSweep(kind, id); err != nil {
		return err
	}
	log.Printf("sweep: deleting %s %s in %s", kind, id, r.region)
	status, err := del(ctx)
	if err != nil {
		return fmt.Errorf("deleting %s %q: %w", kind, id, err)
	}
	switch {
	case status == http.StatusNotFound:
		return nil
	case status < 200 || status > 299:
		return fmt.Errorf("deleting %s %q answered %d", kind, id, status)
	}
	return r.until(ctx, kind+" "+id+" to be gone", func(ctx context.Context) (bool, error) {
		status, err := get(ctx)
		return status == http.StatusNotFound, err
	})
}

// until polls at the provider's interval, within its operation timeout.
func (r *sweepRun) until(ctx context.Context, what string, done func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, r.meta.Wait.Timeout)
	defer cancel()
	for {
		ok, err := done(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for %s after %s", what, r.meta.Wait.Timeout)
		case <-time.After(r.meta.Wait.Interval):
		}
	}
}

// eachProject runs a sweep in every project a test object can be in, and
// carries on past a failure in one.
func (r *sweepRun) eachProject(f func(project string) error) error {
	var errs []error
	for _, project := range r.projects {
		if err := f(project); err != nil {
			errs = append(errs, fmt.Errorf("in project %s: %w", project, err))
		}
	}
	return errors.Join(errs...)
}

// sweepEach sweeps every listed row that's ours, carrying on past a failure.
func sweepEach(rows []sweepRow, sweep func(id string) error) error {
	var errs []error
	for _, row := range rows {
		if !ours(row.ID) {
			continue
		}
		if err := sweep(row.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func sweepFileStores(ctx context.Context, r *sweepRun) error {
	c := r.meta.Client
	return r.eachProject(func(project string) error {
		rows, err := r.rows("fileStores", byToken, func(_ int32, token *string, limit int32) (int, []byte, error) {
			size := int(limit)
			got, err := c.ListFileStoresWithResponse(ctx, r.region, project,
				&k0rdent.ListFileStoresParams{PageToken: token, PageSize: &size})
			if err != nil {
				return 0, nil, err
			}
			return got.StatusCode(), got.Body, nil
		})
		if err != nil {
			return err
		}
		return sweepEach(rows, func(id string) error {
			get := func(ctx context.Context) (int, error) {
				return status(c.GetFileStoreWithResponse(ctx, r.region, project, id))
			}
			del := func(ctx context.Context) (int, error) {
				var last int
				err := r.until(ctx, "file store "+id+" to accept its delete", func(ctx context.Context) (bool, error) {
					code, err := r.tryFileStoreDelete(ctx, project, id)
					last = code
					return code != 0 && code != http.StatusConflict, err
				})
				return last, err
			}
			return r.sweepOne(ctx, "file store", id, del, get)
		})
	})
}

// tryFileStoreDelete is one attempt at deleting a store, and answers 0 when
// it isn't ready to be asked yet. The platform refuses to delete a store that
// still lists consumers, and refuses while a change is still being applied:
// "wait until `reconciling` is false and retry". So the consumers are emptied,
// the store is read until it has caught up, and only then is the delete sent.
func (r *sweepRun) tryFileStoreDelete(ctx context.Context, project, id string) (int, error) {
	c := r.meta.Client
	got, err := c.GetFileStoreWithResponse(ctx, r.region, project, id)
	if err != nil {
		return 0, err
	}
	if got.StatusCode() == http.StatusNotFound {
		return http.StatusNotFound, nil
	}
	var store struct {
		Consumers    []json.RawMessage `json:"consumers"`
		Reconciling  bool              `json:"reconciling"`
		State        string            `json:"state"`
		StatusReason string            `json:"statusReason"`
	}
	if err = json.Unmarshal(got.Body, &store); err != nil {
		return 0, err
	}
	if len(store.Consumers) > 0 {
		// Sent again on every pass: the empty list is refused too while an
		// earlier change is still being applied.
		none := []k0rdent.Consumer{}
		_, err = status(c.UpdateFileStoreWithResponse(ctx, r.region, project, id,
			k0rdent.FileStoreUpdateRequest{Consumers: &none}))
		return 0, err
	}
	// A failed store, or one whose last change failed, keeps `reconciling`
	// true for good, so that isn't waited on.
	if store.Reconciling && store.State != "failed" && store.StatusReason == "" {
		return 0, nil
	}
	return status(c.DeleteFileStoreWithResponse(ctx, r.region, project, id, nil))
}

func sweepPeerings(ctx context.Context, r *sweepRun) error {
	c := r.meta.Client
	return r.eachProject(func(project string) error {
		vpcs, err := r.rows("vpcs", byOffset, func(offset int32, _ *string, limit int32) (int, []byte, error) {
			got, err := c.ListVPCsWithResponse(ctx, r.region, project,
				&k0rdent.ListVPCsParams{Offset: &offset, Limit: &limit})
			if err != nil {
				return 0, nil, err
			}
			return got.StatusCode(), got.Body, nil
		})
		if err != nil {
			return err
		}
		var errs []error
		for _, vpc := range vpcs {
			rows, err := r.rows("peerings", byToken, func(_ int32, token *string, limit int32) (int, []byte, error) {
				size := int(limit)
				got, err := c.ListVPCPeeringsWithResponse(ctx, r.region, project, vpc.ID,
					&k0rdent.ListVPCPeeringsParams{PageToken: token, PageSize: &size})
				if err != nil {
					return 0, nil, err
				}
				return got.StatusCode(), got.Body, nil
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("peerings of %s: %w", vpc.ID, err))
				continue
			}
			errs = append(errs, sweepEach(rows, func(id string) error {
				return r.sweepOne(ctx, "VPC peering", id,
					func(ctx context.Context) (int, error) {
						return status(c.DeleteVPCPeeringWithResponse(ctx, r.region, project, vpc.ID, id))
					},
					func(ctx context.Context) (int, error) {
						return status(c.GetVPCPeeringWithResponse(ctx, r.region, project, vpc.ID, id))
					})
			}))
		}
		return errors.Join(errs...)
	})
}

func sweepClusters(ctx context.Context, r *sweepRun) error {
	c := r.meta.Client
	return r.eachProject(func(project string) error {
		rows, err := r.rows("clusters", byToken, func(_ int32, token *string, limit int32) (int, []byte, error) {
			size := int(limit)
			got, err := c.ListClustersWithResponse(ctx, r.region, project,
				&k0rdent.ListClustersParams{PageToken: token, PageSize: &size})
			if err != nil {
				return 0, nil, err
			}
			return got.StatusCode(), got.Body, nil
		})
		if err != nil {
			return err
		}
		return sweepEach(rows, func(id string) error {
			get := func(ctx context.Context) (int, error) {
				return status(c.GetClusterWithResponse(ctx, r.region, project, id, nil))
			}
			// A cluster in the middle of an operation refuses a delete. A
			// killed run is likely to leave one like that, so its operation
			// is cancelled and the delete sent once more, which is what the
			// provider's own destroy does.
			del := func(ctx context.Context) (int, error) {
				code, err := status(c.DeleteClusterWithResponse(ctx, r.region, project, id))
				if err != nil || code != http.StatusConflict {
					return code, err
				}
				log.Printf("sweep: cluster %s is busy, cancelling its operation", id)
				// A cancel names the operation it stops, as the cluster reads it now.
				now, err := c.GetClusterWithResponse(ctx, r.region, project, id, nil)
				if err != nil {
					return 0, err
				}
				if now.JSON200 == nil || now.JSON200.Operation == nil || now.JSON200.Operation.ExecutionId == nil {
					return now.StatusCode(), fmt.Errorf("cluster %s refused its delete and names no operation to cancel", id)
				}
				body := k0rdent.CancelClusterJSONRequestBody{ExecutionId: *now.JSON200.Operation.ExecutionId}
				if _, err = status(c.CancelClusterWithResponse(ctx, r.region, project, id, body)); err != nil {
					return 0, err
				}
				err = r.until(ctx, "cluster "+id+" to accept its delete", func(ctx context.Context) (bool, error) {
					again, deleteErr := status(c.DeleteClusterWithResponse(ctx, r.region, project, id))
					code = again
					return again != http.StatusConflict, deleteErr
				})
				return code, err
			}
			return r.sweepOne(ctx, "cluster", id, del, get)
		})
	})
}

func sweepInstanceGroups(ctx context.Context, r *sweepRun) error {
	c := r.meta.Client
	return r.eachProject(func(project string) error {
		rows, err := r.rows("instanceGroups", byToken, func(_ int32, token *string, limit int32) (int, []byte, error) {
			size := int(limit)
			got, err := c.ListInstanceGroupsWithResponse(ctx, r.region, project,
				&k0rdent.ListInstanceGroupsParams{PageToken: token, PageSize: &size})
			if err != nil {
				return 0, nil, err
			}
			return got.StatusCode(), got.Body, nil
		})
		if err != nil {
			return err
		}
		return sweepEach(rows, func(id string) error {
			return r.sweepOne(ctx, "instance group", id,
				func(ctx context.Context) (int, error) {
					return status(c.DeleteInstanceGroupWithResponse(ctx, r.region, project, id))
				},
				func(ctx context.Context) (int, error) {
					return status(c.GetInstanceGroupWithResponse(ctx, r.region, project, id, nil))
				})
		})
	})
}

// networksIn lists every network in a project, ours or not, because a subnet
// this suite made can sit in a network it didn't.
func networksIn(ctx context.Context, r *sweepRun, project string) ([]sweepRow, error) {
	return r.rows("networks", byOffset, func(offset int32, _ *string, limit int32) (int, []byte, error) {
		got, err := r.meta.Client.ListNetworksWithResponse(ctx, r.region, project,
			&k0rdent.ListNetworksParams{Offset: &offset, Limit: &limit})
		if err != nil {
			return 0, nil, err
		}
		return got.StatusCode(), got.Body, nil
	})
}

func sweepSubnets(ctx context.Context, r *sweepRun) error {
	c := r.meta.Client
	return r.eachProject(func(project string) error {
		networks, err := networksIn(ctx, r, project)
		if err != nil {
			return err
		}
		var errs []error
		for _, network := range networks {
			rows, err := r.rows("subnets", byOffset, func(offset int32, _ *string, limit int32) (int, []byte, error) {
				got, err := c.ListSubnetsWithResponse(ctx, r.region, project, network.ID,
					&k0rdent.ListSubnetsParams{Offset: &offset, Limit: &limit})
				if err != nil {
					return 0, nil, err
				}
				return got.StatusCode(), got.Body, nil
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("subnets of %s: %w", network.ID, err))
				continue
			}
			errs = append(errs, sweepEach(rows, func(id string) error {
				return r.sweepOne(ctx, "subnet", id,
					func(ctx context.Context) (int, error) {
						return status(c.DeleteSubnetWithResponse(ctx, r.region, project, network.ID, id))
					},
					func(ctx context.Context) (int, error) {
						return status(c.GetSubnetWithResponse(ctx, r.region, project, network.ID, id))
					})
			}))
		}
		return errors.Join(errs...)
	})
}

func sweepNetworks(ctx context.Context, r *sweepRun) error {
	c := r.meta.Client
	return r.eachProject(func(project string) error {
		rows, err := networksIn(ctx, r, project)
		if err != nil {
			return err
		}
		return sweepEach(rows, func(id string) error {
			return r.sweepOne(ctx, "network", id,
				func(ctx context.Context) (int, error) {
					return status(c.DeleteNetworkWithResponse(ctx, r.region, project, id))
				},
				func(ctx context.Context) (int, error) {
					return status(c.GetNetworkWithResponse(ctx, r.region, project, id))
				})
		})
	})
}

func sweepSecurityGroups(ctx context.Context, r *sweepRun) error {
	c := r.meta.Client
	return r.eachProject(func(project string) error {
		rows, err := r.rows("securityGroups", byOffset, func(offset int32, _ *string, limit int32) (int, []byte, error) {
			got, err := c.ListSecurityGroupsWithResponse(ctx, r.region, project,
				&k0rdent.ListSecurityGroupsParams{Offset: &offset, Limit: &limit})
			if err != nil {
				return 0, nil, err
			}
			return got.StatusCode(), got.Body, nil
		})
		if err != nil {
			return err
		}
		return sweepEach(rows, func(id string) error {
			return r.sweepOne(ctx, "security group", id,
				func(ctx context.Context) (int, error) {
					return status(c.DeleteSecurityGroupWithResponse(ctx, r.region, project, id))
				},
				func(ctx context.Context) (int, error) {
					return status(c.GetSecurityGroupWithResponse(ctx, r.region, project, id))
				})
		})
	})
}

// sweepProjects deletes the projects this suite made, once each is empty.
//
// It can't leave that to the platform. On v1.3.0-dev.4 a project holding a
// network answered its DELETE with 204, and the network, a subnet and a
// security group made before it were still there and active two minutes
// after the project read 404, orphaned (out/live-shapes-dev4.txt, section
// F). So a project that still holds anything after the other sweepers ran,
// in any region, is refused and what it holds is named. That covers an
// object without the prefix, which no sweeper touches, and one a sweeper
// failed to remove.
func sweepProjects(ctx context.Context, r *sweepRun) error {
	c := r.meta.Client
	var ids []string
	for id := range r.orgOf {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var errs []error
	for _, id := range ids {
		org := r.orgOf[id]
		held, err := r.projectHolds(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("refusing to delete project %q: what it holds couldn't be listed: %w", id, err))
			continue
		}
		if len(held) > 0 {
			errs = append(errs, fmt.Errorf("refusing to delete project %q: it still holds %s, and the platform "+
				"would leave those behind, still running, with no project to reach them through",
				id, strings.Join(held, ", ")))
			continue
		}
		errs = append(errs, r.sweepOne(ctx, "project", id,
			func(ctx context.Context) (int, error) {
				return status(c.DeleteProjectInOrgWithResponse(ctx, org, id))
			},
			func(ctx context.Context) (int, error) {
				return status(c.GetProjectInOrgWithResponse(ctx, org, id))
			}))
	}
	return errors.Join(errs...)
}

// sweepWebhookSubscriptions deletes every subscription whose endpoint's host
// starts with the prefix. The platform picks a subscription's id, so the id
// can't carry the prefix, and the endpoint is the one thing this suite names.
// The guard reads the host, followed by the id so each deletion says which.
// Subscriptions live on the global tier, so the region changes nothing.
func sweepWebhookSubscriptions(ctx context.Context, r *sweepRun) error {
	c := r.meta.Client
	listed := func(ctx context.Context) (map[string]string, error) {
		got, err := c.ListWebhookSubscriptionsWithResponse(ctx)
		if err != nil {
			return nil, err
		}
		if got.JSON200 == nil {
			return nil, fmt.Errorf("the list answered %d: %s", got.StatusCode(), got.Body)
		}
		hosts := map[string]string{}
		for _, s := range got.JSON200.Subscriptions {
			if s.Id == nil || s.Url == nil {
				continue
			}
			if u, err := url.Parse(*s.Url); err == nil {
				hosts[s.Id.String()] = u.Host
			}
		}
		return hosts, nil
	}
	hosts, err := listed(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for id, host := range hosts {
		if !ours(host) {
			continue
		}
		subscription, err := uuid.Parse(id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, r.sweepOne(ctx, "webhook subscription", host+"/"+id,
			func(ctx context.Context) (int, error) {
				return status(c.DeleteWebhookSubscriptionWithResponse(ctx, subscription))
			},
			// No read of one subscription exists, so it's gone when the
			// list no longer has it.
			func(ctx context.Context) (int, error) {
				now, err := listed(ctx)
				if err != nil {
					return 0, err
				}
				if _, still := now[id]; still {
					return http.StatusOK, nil
				}
				return http.StatusNotFound, nil
			}))
	}
	return errors.Join(errs...)
}

// projectHolds names everything a project still holds, in every regional
// region. VPCs and their peerings aren't listed: a VPC belongs to the cluster
// or instance group that made it and goes with its owner, which is listed.
// Neither is a security group the platform owns, for the same reason.
func (r *sweepRun) projectHolds(ctx context.Context, project string) ([]string, error) {
	c := r.meta.Client
	got, err := c.ListRegionsWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if got.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("listing regions answered %d", got.StatusCode())
	}
	var regions struct {
		Regions []sweepRow `json:"regions"`
	}
	if err = json.Unmarshal(got.Body, &regions); err != nil {
		return nil, err
	}

	var held []string
	for _, region := range regions.Regions {
		if region.Type != "regional" {
			continue
		}
		for _, kind := range projectKinds(ctx, c, region.ID, project) {
			rows, err := r.rows(kind.key, kind.paging, kind.fetch)
			if err != nil {
				return nil, fmt.Errorf("%s in %s: %w", kind.noun, region.ID, err)
			}
			for _, row := range rows {
				if row.OwnerKind == "" {
					held = append(held, fmt.Sprintf("%s %s in %s", kind.noun, row.ID, region.ID))
				}
			}
		}
	}
	return held, nil
}

// projectKind is one list projectHolds reads.
type projectKind struct {
	noun, key string
	paging    paging
	fetch     pageFetch
}

// projectKinds is every project-scoped list a project's contents show up in.
// A subnet sits in a network, so a network left over names both.
func projectKinds(ctx context.Context, c *k0rdent.ClientWithResponses, region, project string) []projectKind {
	byPage := func(token *string, limit int32) (*k0rdent.PageToken, *k0rdent.PageSize) {
		size := int(limit)
		return token, &size
	}
	return []projectKind{
		{noun: "file store", key: "fileStores", paging: byToken, fetch: func(_ int32, token *string, limit int32) (int, []byte, error) {
			tok, size := byPage(token, limit)
			got, err := c.ListFileStoresWithResponse(ctx, region, project, &k0rdent.ListFileStoresParams{PageToken: tok, PageSize: size})
			if err != nil {
				return 0, nil, err
			}
			return got.StatusCode(), got.Body, nil
		}},
		{noun: "cluster", key: "clusters", paging: byToken, fetch: func(_ int32, token *string, limit int32) (int, []byte, error) {
			tok, size := byPage(token, limit)
			got, err := c.ListClustersWithResponse(ctx, region, project, &k0rdent.ListClustersParams{PageToken: tok, PageSize: size})
			if err != nil {
				return 0, nil, err
			}
			return got.StatusCode(), got.Body, nil
		}},
		{noun: "instance group", key: "instanceGroups", paging: byToken, fetch: func(_ int32, token *string, limit int32) (int, []byte, error) {
			tok, size := byPage(token, limit)
			got, err := c.ListInstanceGroupsWithResponse(ctx, region, project, &k0rdent.ListInstanceGroupsParams{PageToken: tok, PageSize: size})
			if err != nil {
				return 0, nil, err
			}
			return got.StatusCode(), got.Body, nil
		}},
		{noun: "network", key: "networks", paging: byOffset, fetch: func(offset int32, _ *string, limit int32) (int, []byte, error) {
			got, err := c.ListNetworksWithResponse(ctx, region, project, &k0rdent.ListNetworksParams{Offset: &offset, Limit: &limit})
			if err != nil {
				return 0, nil, err
			}
			return got.StatusCode(), got.Body, nil
		}},
		{noun: "security group", key: "securityGroups", paging: byOffset, fetch: func(offset int32, _ *string, limit int32) (int, []byte, error) {
			got, err := c.ListSecurityGroupsWithResponse(ctx, region, project, &k0rdent.ListSecurityGroupsParams{Offset: &offset, Limit: &limit})
			if err != nil {
				return 0, nil, err
			}
			return got.StatusCode(), got.Body, nil
		}},
	}
}

// generated is what every generated response has in common.
type generated interface{ StatusCode() int }

// status is the HTTP status of a generated response.
func status[T generated](got T, err error) (int, error) {
	if err != nil {
		return 0, err
	}
	return got.StatusCode(), nil
}

// forgetSweepRuns drops the shared per-region state, so a test that changes
// the environment starts from it.
func forgetSweepRuns() {
	sweepRunsMu.Lock()
	defer sweepRunsMu.Unlock()
	sweepRuns = map[string]*sweepRun{}
}

func TestSweepGuardRefusesAnythingWithoutThePrefix(t *testing.T) {
	for _, id := range []string{"acc-cluster", "acc-x", "acc-to-inference-fake-vpc-lock-lost"} {
		if err := guardSweep("cluster", id); err != nil {
			t.Errorf("%q was refused: %v", id, err)
		}
	}
	// A generated id, a tenant's own name, the bare prefix, the prefix
	// somewhere other than the start, and nothing at all.
	for _, id := range []string{"tf-0123456789abcdef", "production", "acc-", "x-acc-cluster", "ACC-cluster", ""} {
		if err := guardSweep("cluster", id); err == nil {
			t.Errorf("%q was let through", id)
		}
	}
}

// The guard has to stop the write, not just report: an object without the
// prefix must never reach a delete, whatever a list said about it.
func TestSweepOneNeverWritesWithoutThePrefix(t *testing.T) {
	run := &sweepRun{region: "r"}
	wrote := false
	del := func(context.Context) (int, error) { wrote = true; return http.StatusNoContent, nil }
	get := func(context.Context) (int, error) { return http.StatusNotFound, nil }
	if err := run.sweepOne(context.Background(), "network", "keep-me", del, get); err == nil {
		t.Error("sweeping keep-me answered no error")
	}
	if wrote {
		t.Error("sweeping keep-me sent the delete")
	}
}

// runSweepers walks the list top to bottom, so the list has to be in an
// order the platform accepts deletes in.
func TestSweepersComeAfterWhatTheyNeed(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range sweepers {
		for _, dep := range s.after {
			if !seen[dep] {
				t.Errorf("%s is swept before %s, which it has to come after", s.kind, dep)
			}
		}
		seen[s.kind] = true
	}
}

// Every managed resource has a sweeper, or a reason it doesn't need one.
func TestSweepersCoverEveryResource(t *testing.T) {
	noSweeper := map[string]bool{
		// A property of a VPC, not an object, and the VPC goes with its owner.
		"vpc_security_groups": true,
	}
	var want, got []string
	for _, c := range perpetualDiffCases() {
		if !noSweeper[c.kind] {
			want = append(want, c.kind)
		}
	}
	for _, s := range sweepers {
		got = append(got, s.kind)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		t.Errorf("sweepers cover %v, and the managed resources needing one are %v", got, want)
	}
}

// Nothing is swept until SWEEP_CONFIRM names the endpoint's host.
func TestSweepRefusesWithoutConfirmation(t *testing.T) {
	forgetSweepRuns()
	t.Cleanup(forgetSweepRuns)
	t.Setenv(provider.EnvVar("ENDPOINT"), "https://api.dev02.example.com")
	t.Setenv(provider.EnvVar("CLIENT_ID"), "sa-terraform@vendor.iam")
	t.Setenv(provider.EnvVar("CLIENT_SECRET"), "k0r_secret")
	t.Setenv(provider.EnvVar("API_KEY"), "")
	// The acceptance run polls its loopback fake faster than a remote
	// endpoint is allowed to be polled.
	t.Setenv(provider.EnvVar("POLL_INTERVAL"), "")
	for _, confirm := range []string{"", "localhost", "api.dev01.example.com", "api.dev02.example.com:443"} {
		t.Setenv(sweepConfirm, confirm)
		_, err := sweepRunFor("us-texas-1")
		if err == nil || !strings.Contains(err.Error(), sweepConfirm) {
			t.Errorf("with %s=%q: %v", sweepConfirm, confirm, err)
			continue
		}
		// The refusal says what to set, and never hands over a line to
		// paste: typing the host is the confirmation.
		if strings.Contains(err.Error(), sweepConfirm+"=api.dev02.example.com") {
			t.Errorf("with %s=%q the refusal prints the value to paste: %v", sweepConfirm, confirm, err)
		}
	}
}

// With no endpoint in the environment the provider falls back to the
// production default. A sweep never does, whatever SWEEP_CONFIRM says.
func TestSweepRefusesAnEndpointThatWasNotSet(t *testing.T) {
	forgetSweepRuns()
	t.Cleanup(forgetSweepRuns)
	t.Setenv(provider.EnvVar("ENDPOINT"), "")
	t.Setenv(provider.EnvVar("CLIENT_ID"), "sa-terraform@vendor.iam")
	t.Setenv(provider.EnvVar("CLIENT_SECRET"), "k0r_secret")
	t.Setenv(provider.EnvVar("API_KEY"), "")
	t.Setenv(provider.EnvVar("POLL_INTERVAL"), "")
	prod, err := url.Parse(provider.DefaultEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(sweepConfirm, prod.Host)
	_, err = sweepRunFor("us-texas-1")
	if err == nil || !strings.Contains(err.Error(), provider.EnvVar("ENDPOINT")) {
		t.Errorf("a sweep with no endpoint set went ahead against %s: %v", provider.DefaultEndpoint, err)
	}
}

// sweepConfig is one of everything the sweepers remove, and beside it the
// same kinds under names without the prefix, which must survive. The subnet
// sits in a network that isn't the suite's, and the file store has an
// instance-group consumer, so its delete needs the detach first.
//
// The network inside the generated project isn't here. makeInnerNetwork
// adds it out of band, because once the sweep has deleted that project the
// gateway answers 403 for anything that was in it, as v1.3.0-dev.4 does, and
// a refresh of a managed network there would stop on it.
func sweepConfig() string {
	const evpn = `evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
  }`
	const egress = `egress {
    action             = "permit"
    protocol           = "any"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"
  }`
	remote := "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
	return fmt.Sprintf(`
resource %[1]q "acc" {
  id              = "acc-sweep-project"
  organization_id = %[9]q
}

resource %[1]q "keep" {
  id              = "keep-sweep-project"
  organization_id = %[9]q
}

resource %[2]q "acc" {
  id = "acc-sweep-net"
  %[10]s
}

resource %[2]q "keep" {
  id = "keep-sweep-net"
  %[10]s
}

resource %[3]q "acc" {
  id               = "acc-sweep-subnet"
  network          = %[2]s.keep.id
  address_pool     = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length    = 24
}

resource %[3]q "keep" {
  id               = "keep-sweep-subnet"
  network          = %[2]s.keep.id
  address_pool     = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length    = 24
}

resource %[4]q "acc" {
  id = "acc-sweep-sg"
  %[11]s
}

resource %[4]q "keep" {
  id = "keep-sweep-sg"
  %[11]s
}

resource %[5]q "acc" {
  id           = "acc-sweep-cluster"
  cluster_type = %[12]q
  node_pool    = { "gpu-workers" = { node_count = 1 } }
}

resource %[5]q "keep" {
  id           = "keep-sweep-cluster"
  cluster_type = %[12]q
  node_pool    = { "gpu-workers" = { node_count = 1 } }
}

resource %[6]q "acc" {
  id         = "acc-sweep-peering"
  vpc        = "vpc-acc-sweep-cluster"
  remote_vpc = %[13]q
  depends_on = [%[5]s.acc]
}

resource %[6]q "keep" {
  id         = "keep-sweep-peering"
  vpc        = "vpc-keep-sweep-cluster"
  remote_vpc = %[13]q
  depends_on = [%[5]s.keep]
}

resource %[7]q "acc" {
  id           = "acc-sweep-hosts"
  cluster_type = %[12]q
  node_pool    = { "gpu-workers" = { size = 2 } }
}

resource %[7]q "keep" {
  id           = "keep-sweep-hosts"
  cluster_type = %[12]q
  node_pool    = { "gpu-workers" = { size = 1 } }
}

resource %[8]q "acc" {
  id           = "acc-sweep-store"
  share_type   = "general"
  capacity_gib = 512
  consumers = {
    (%[7]s.acc.id) = {
      type    = "instanceGroup"
      network = { type = "ethernet" }
    }
  }
}

resource %[8]q "keep" {
  id           = "keep-sweep-store"
  share_type   = "general"
  capacity_gib = 512
  consumers    = %[14]s
}

resource %[15]q "acc" {
  url               = %[16]q
  secret_wo         = sha256("acc-sweep")
  secret_wo_version = 1
}

resource %[15]q "keep" {
  url               = %[17]q
  secret_wo         = sha256("keep-sweep")
  secret_wo_version = 1
}
`, projectType, networkType, subnetType, securityGroupType, clusterType, peeringType,
		instanceGroupType, fileStoreType, accOrg, evpn, egress, clusterTypeRef, remote, oneConsumer,
		webhookType, sweptWebhookURL, keptWebhookURL)
}

// The endpoints of the two webhook subscriptions sweepConfig makes. The
// platform picks their ids, so checkWebhooksSwept finds them by endpoint.
const (
	sweptWebhookURL = "https://acc-hooks.example.test/sweep"
	keptWebhookURL  = "https://keep-hooks.example.test/sweep"
)

// checkWebhooksSwept reads the list after the sweep: the suite's subscription
// has to be gone and the other one still there.
func checkWebhooksSwept(*terraform.State) error {
	status, body, err := readFake("/v1/regions/global/notifications/webhooks")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("listing subscriptions answered %d", status)
	}
	var list struct {
		Subscriptions []struct {
			URL string `json:"url"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return err
	}
	have := map[string]bool{}
	for _, s := range list.Subscriptions {
		have[s.URL] = true
	}
	switch {
	case have[sweptWebhookURL]:
		return fmt.Errorf("the subscription to %s survived the sweep", sweptWebhookURL)
	case !have[keptWebhookURL]:
		return fmt.Errorf("the sweep deleted the subscription to %s, which isn't the suite's", keptWebhookURL)
	}
	return nil
}

// sweepPaths are the objects sweepConfig and makeInnerNetwork make, by
// whether a sweep removes them.
var sweepPaths = map[bool][]string{
	true: {
		"/v1/regions/us-texas-1/projects/acc-sweep-project/networking/networks/acc-sweep-inner",
		"/v1/regions/us-texas-1/projects/demo/networking/networks/acc-sweep-net",
		"/v1/regions/us-texas-1/projects/demo/networking/networks/keep-sweep-net/subnets/acc-sweep-subnet",
		"/v1/regions/us-texas-1/projects/demo/networking/security-groups/acc-sweep-sg",
		"/v1/regions/us-texas-1/projects/demo/compute/clusters/acc-sweep-cluster",
		"/v1/regions/us-texas-1/projects/demo/compute/instance-groups/acc-sweep-hosts",
		"/v1/regions/us-texas-1/projects/demo/storage/file-stores/acc-sweep-store",
	},
	false: {
		// It still holds keep-sweep-inner, which isn't the suite's, so the
		// sweep has to leave the project rather than orphan what's in it.
		"/v1/regions/global/organizations/org-demo/projects/acc-sweep-project",
		"/v1/regions/us-texas-1/projects/acc-sweep-project/networking/networks/keep-sweep-inner",
		"/v1/regions/global/organizations/org-demo/projects/keep-sweep-project",
		"/v1/regions/us-texas-1/projects/demo/networking/networks/keep-sweep-net/subnets/keep-sweep-subnet",
		"/v1/regions/us-texas-1/projects/demo/compute/instance-groups/keep-sweep-hosts",
		"/v1/regions/us-texas-1/projects/demo/networking/networks/keep-sweep-net",
		"/v1/regions/us-texas-1/projects/demo/networking/security-groups/keep-sweep-sg",
		"/v1/regions/us-texas-1/projects/demo/compute/clusters/keep-sweep-cluster",
		"/v1/regions/us-texas-1/projects/demo/networking/vpcs/vpc-keep-sweep-cluster/peerings/keep-sweep-peering",
		"/v1/regions/us-texas-1/projects/demo/storage/file-stores/keep-sweep-store",
	},
}

// TestAccSweepers runs every sweeper against the fake with the suite's
// objects and somebody else's side by side: each of the first has to be
// gone, and each of the second untouched. The peering on the swept cluster's
// VPC goes first, and is checked through its cluster.
func TestAccSweepers(t *testing.T) {
	forgetSweepRuns()
	t.Cleanup(forgetSweepRuns)
	host, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(sweepConfirm, host.Host)
	// The sweepers wait within the provider's operation timeout, and its
	// default is sized for real hardware, not for a fake that settles in
	// two reads.
	t.Setenv(provider.EnvVar("OPERATION_TIMEOUT"), "2m")

	config := sweepConfig()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(projectType, projectPath),
			checkGone(networkType, networkPath),
			checkGone(securityGroupType, securityGroupPath),
			checkGone(clusterType, clusterPath),
			checkGone(peeringType, peeringPath),
			checkGone(fileStoreType, fileStorePath),
			checkWebhookGone,
		),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					for _, id := range []string{"acc-sweep-inner", "keep-sweep-inner"} {
						if err := makeInnerNetwork(id); err != nil {
							t.Fatalf("making %s inside the generated project: %v", id, err)
						}
					}
					// The one failure wanted: the project still holds a
					// network that isn't the suite's, so it is refused.
					err := runSweepers("us-texas-1")
					if err == nil || !strings.Contains(err.Error(), "acc-sweep-project") ||
						!strings.Contains(err.Error(), "keep-sweep-inner") {
						t.Fatalf("sweeping: want the project refused for holding keep-sweep-inner, got %v", err)
					}
					for _, sw := range sweepers {
						if sw.kind != "project" && strings.Contains(err.Error(), sw.kind+": ") {
							t.Fatalf("sweeping failed for more than the project: %v", err)
						}
					}
				},
				// The refresh finds the swept objects gone, so the plan after
				// it proposes making them again.
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check:              resource.ComposeAggregateTestCheckFunc(checkSwept(), checkWebhooksSwept),
			},
		},
	})
}

// innerNetwork is a network only reachable through the generated project,
// which the sweep has to find by looking in every acc- project.
const innerNetwork = "/v1/regions/us-texas-1/projects/acc-sweep-project/networking/networks"

// makeInnerNetwork creates a network in the generated project and waits for
// it to settle, as a run killed before its destroy would have left it.
func makeInnerNetwork(id string) error {
	token, err := mintFakeToken()
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint+innerNetwork,
		strings.NewReader(`{"id":"`+id+`","type":"evpn","evpn":{"type":"l3vpn"}}`))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := harnessHTTP.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("the fake answered %s", resp.Status)
	}
	for range 10 {
		code, body, err := readFake(innerNetwork + "/" + id)
		if err != nil {
			return err
		}
		var read struct {
			State string `json:"state"`
		}
		if code == http.StatusOK && json.Unmarshal(body, &read) == nil && read.State == "active" {
			return nil
		}
	}
	return errors.New("the network never settled active")
}

// checkSwept reads every object sweepConfig made, and the inner network,
// from the fake's store. A swept object's project may be gone, and the API
// answers 403 there whether the object went with it or not.
func checkSwept() resource.TestCheckFunc {
	return func(*terraform.State) error {
		var errs []error
		for swept, paths := range sweepPaths {
			for _, path := range paths {
				code, _, err := readFakeStore(path)
				switch {
				case err != nil:
					errs = append(errs, err)
				case swept && code != http.StatusNotFound:
					errs = append(errs, fmt.Errorf("%s answered %d after the sweep, want 404", path, code))
				case !swept && code != http.StatusOK:
					errs = append(errs, fmt.Errorf("%s answered %d after the sweep, want 200: it isn't the suite's", path, code))
				}
			}
		}
		return errors.Join(errs...)
	}
}
