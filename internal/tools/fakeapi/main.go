// Command fakeapi serves the slice of the k0rdent AI API that the provider's
// tenant-cluster path needs: authenticate, look up a cluster type, create
// a cluster, scale it, upgrade it, cancel what it is doing, read a kubeconfig,
// and destroy it.
//
// It exists because no tenant credential on a dev landscape exists yet and no
// landscape hostname is recorded anywhere. This is the honest substitute.
// Everything on the provider's side of the wire is real:
// real HCL, real plugin protocol, real provider process, real environment
// variables, real token mint over HTTP, real bearer header, real generated
// client. Only the control plane is fake.
//
// # What a green run proves, and what it does not
//
// It proves the provider's wiring: that a value written in HCL reaches the API
// as the generated client's request body, that the response decodes, that state
// round-trips, and that a second plan reports no changes **given the behaviour
// modelled here**. It proves nothing about the real server.
//
// # The rule this file is written to
//
// **The fake must not agree with itself.** A fake that echoes back exactly what
// it was sent makes the second plan clean by construction, which is a tautology
// dressed up as a measurement. So for each hazard the contract leaves open, this
// models the *suspected real* behaviour, the damaging reading where there is a
// damaging one, and says so in an ASSUMPTION comment at the point of code. The
// provider then has to pass against that, and a no-op plan becomes a bounded
// claim: "correct given these assumptions."
//
// Every ASSUMPTION below is a row in `wave2-noop-drift-checklist.md` and is
// unobserved. Grep for `ASSUMPTION` to read them all at once. When the
// credential lands and the real behaviour is recorded, each one is either
// confirmed or replaced, and the provider's handling of it is re-tested.
//
// # The routes are the only paths this repository writes out
//
// The provider itself assembles no URLs: every request comes from a generated
// client operation, which is the only safe arrangement given that the contract
// moves paths without a deprecation window. A fake server has
// to declare routes, and these fail closed: when the contract moves a path, the
// regenerated client asks for the new one, this fake answers 404, and the
// harness goes red. It cannot drift into silently agreeing with a stale
// provider.
//
// Run it with `scripts/demo.sh`, which prints the port it chose.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// settleAfter is how many reads a transient state survives before the fake
// reports it settled.
//
// Two, not zero, on purpose: a fake that answered `active` on the first read
// would let a provider with no waiter at all pass. Two reads means the waiter
// has to actually loop. It is not a claim about real timing: a GPU cluster takes
// minutes, which is why the provider's poll interval is configurable and the
// demo sets it low.
const settleAfter = 2

// Two markers a display name can carry to steer where an object settles.
//
// Nothing a client can legitimately send makes a real platform fail on demand,
// so the fake has to be told. Without these, the provider's `failed` and
// unknown-state waiter branches are unreachable from any test.
//
// A display name rather than a tag because every object that has a waiter has a
// display name, and the provider does not manage a subnet's tags.
const (
	settlesFailed  = "fake-settles-failed"
	settlesUnknown = "fake-settles-unknown"

	// Deliberately not a value any state enum in the contract declares. A
	// provider that recognised it would not be testing what this is for.
	unrecognisedState = "quiescing"

	// A cluster whose display name carries this is never issued a kubeconfig:
	// its one issue fails, and the list reports it `failed`.
	kubeconfigFails = "fake-kubeconfig-fails"

	// A cluster whose id carries this gets a VPC on the `ufo` fabric backend
	// rather than `nico`, so the refusal path for a binding is reachable.
	ufoBackendMarker = "on-ufo"

	// An upgrade on a cluster whose display name carries this never settles,
	// so a test can read one in flight across as many requests as it takes.
	holdsUpgrade = "fake-holds-upgrade"
)

// settle advances a transient state once it has been read settleAfter times,
// to `active` or to wherever the display name asks for instead.
func settle(state *string, reads *int, displayName *string) {
	*reads++

	target := "active"
	switch {
	case nameCarries(displayName, settlesFailed):
		target = "failed"
	case nameCarries(displayName, settlesUnknown):
		target = unrecognisedState
	}

	if *reads > settleAfter && *state != target {
		*state, *reads = target, 0
	}
}

func nameCarries(displayName *string, marker string) bool {
	return displayName != nil && strings.Contains(*displayName, marker)
}

// failureDetail is the message and reason a failed object reports. The provider
// formats both into its diagnostic; without them that formatting is untested.
func failureDetail(state string, out map[string]any) {
	if state != "failed" {
		return
	}
	out["stateMessage"] = "the fabric rejected the request"
	out["stateReason"] = "INVALID_CONFIGURATION"
}

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address; port 0 picks a free one")
	secretsFile := flag.String("secrets-file", "", "write this run's credentials here, 0600; must not exist yet")
	latency := flag.Duration("latency", 0, "hold every API request this long before answering it, as a real stack's round trip would")
	flag.Parse()

	// Loopback only. This process mints tokens and serves whatever a run made.
	host, _, hostErr := net.SplitHostPort(*addr)
	if hostErr != nil {
		log.Fatalf("fakeapi: -addr %q is not host:port: %v", *addr, hostErr)
	}
	if !isLoopback(host) {
		log.Fatalf("fakeapi: -addr %q is not a loopback address, and this process mints tokens.", *addr)
	}
	if *secretsFile == "" {
		log.Fatalf("fakeapi: -secrets-file is required. Each run makes its own credentials " +
			"and writes them there for the harness to read. See guard.go.")
	}
	secrets, err := newRunSecrets()
	if err != nil {
		log.Fatalf("fakeapi: %v", err)
	}
	if err = secrets.write(*secretsFile); err != nil {
		log.Fatalf("fakeapi: write the secrets file: %v", err)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("fakeapi: listen: %v", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !tcpAddr.IP.IsLoopback() {
		_ = ln.Close()
		log.Fatalf("fakeapi: refusing non-loopback listener %q", ln.Addr())
	}

	var srv *http.Server
	shutdown := func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				log.Printf("fakeapi: shutdown: %v", err)
			}
		}()
	}
	s := newState()
	api, apiErr := startClusterAPI()
	if apiErr != nil {
		log.Fatalf("fakeapi: start the cluster API: %v", apiErr)
	}
	s.clusterAPI = api
	handler := delayed(*latency, newHandler(s, secrets, strconv.Itoa(tcpAddr.Port), shutdown))
	srv = &http.Server{Handler: handler} //nolint:gosec // local verification harness, no timeouts wanted

	// The harness reads this line to learn the URL. os.Stdout is unbuffered, so
	// it is already written. An fsync here fails with EINVAL when stdout is a
	// pipe, which killed every caller that did not redirect to a file.
	fmt.Printf("http://%s\n", ln.Addr().String())

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("fakeapi: serve: %v", err)
	}
}

// newHandler is every route the fake serves, behind the guard and the request
// log. port is the listener's, which the guard requires the Host header to
// name. shutdown stops the server, for the owner's /_harness/shutdown.
func newHandler(s *state, secrets runSecrets, port string, shutdown func()) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/regions/global/auth/token", secrets.minting(s.serviceAccountMints))
	mux.HandleFunc("GET /_harness/ready", secrets.ownerControl(nil))
	mux.HandleFunc("POST /_harness/shutdown", secrets.ownerControl(shutdown))
	mux.HandleFunc("GET /_harness/store/{path...}", secrets.owner(s.storeRead()))
	counts := newRequestCounts()
	mux.HandleFunc("GET /_harness/requests", secrets.owner(counts.serve))
	s.routes(mux, secrets.auth)

	// Logged and counted before the guard, so a refused request is on the
	// record too.
	return logging(counts, guard(port, churning(mux)))
}

// routes declares every API route the fake serves. These are the only paths
// this repository writes out, and they fail closed: anything else is the
// gateway's no-route 404. auth is the run's bearer check, and every route
// goes through it before the gateway's authorization.
func (s *state) routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	s.routesWith(mux, func(next http.HandlerFunc) http.HandlerFunc { return auth(s.gateway(next)) })
}

// routesWith declares the API routes with each handler wrapped in authorized.
// The API wraps them in the bearer check and the gateway. The harness's store
// read wraps them in nothing.
func (s *state) routesWith(mux *http.ServeMux, authorized func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /v1/regions", authorized(s.listRegions))
	mux.HandleFunc("GET /v1/regions/global/projects", authorized(s.listProjects))
	mux.HandleFunc("GET /v1/regions/global/organizations", authorized(s.listOrganizations))
	mux.HandleFunc("GET /v1/regions/global/organizations/{orgId}", authorized(s.getOrganization))
	mux.HandleFunc("GET /v1/regions/global/organizations/{orgId}/projects", authorized(s.listProjectsInOrg))
	mux.HandleFunc("POST /v1/regions/global/organizations/{orgId}/projects", authorized(s.createProjectInOrg))
	mux.HandleFunc("GET /v1/regions/global/organizations/{orgId}/projects/{project}", authorized(s.getProjectInOrg))
	mux.HandleFunc("PATCH /v1/regions/global/organizations/{orgId}/projects/{project}", authorized(s.updateProjectInOrg))
	mux.HandleFunc("DELETE /v1/regions/global/organizations/{orgId}/projects/{project}", authorized(s.deleteProjectInOrg))
	mux.HandleFunc("GET /v1/regions/global/organizations/{orgId}/iam/roles", authorized(s.listRoles))
	sa := "/v1/regions/global/organizations/{orgId}/iam/service-accounts"
	mux.HandleFunc("GET "+sa, authorized(s.listServiceAccounts))
	mux.HandleFunc("POST "+sa, authorized(s.createServiceAccount))
	mux.HandleFunc("GET "+sa+"/{id}", authorized(s.getServiceAccount))
	mux.HandleFunc("PATCH "+sa+"/{id}", authorized(s.updateServiceAccount))
	mux.HandleFunc("DELETE "+sa+"/{id}", authorized(s.deleteServiceAccount))
	mux.HandleFunc("GET "+sa+"/{serviceAccountId}/credentials", authorized(s.listServiceAccountCredentials))
	mux.HandleFunc("POST "+sa+"/{serviceAccountId}/credentials", authorized(s.createServiceAccountCredential))
	mux.HandleFunc("GET "+sa+"/{serviceAccountId}/credentials/{id}", authorized(s.getServiceAccountCredential))
	mux.HandleFunc("DELETE "+sa+"/{serviceAccountId}/credentials/{id}", authorized(s.deleteServiceAccountCredential))
	mux.HandleFunc("GET /v1/regions/{region}/compute/cluster-types", authorized(s.listClusterTypes))
	mux.HandleFunc("GET /v1/regions/{region}/compute/cluster-types/{id}", authorized(s.getClusterType))
	mux.HandleFunc("GET /v1/regions/{region}/compute/cluster-types/{id}/versions", authorized(s.listClusterTypeVersions))
	mux.HandleFunc("GET /v1/regions/{region}/compute/cluster-types/{id}/versions/{versionId}", authorized(s.getClusterTypeVersion))
	mux.HandleFunc("GET /v1/regions/{region}/networking/address-pools", authorized(s.listAddressPools))
	mux.HandleFunc("GET /v1/regions/{region}/networking/address-pools/{id}", authorized(s.getAddressPool))

	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/compute/clusters", authorized(s.listClusters))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/compute/clusters", authorized(s.createCluster))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/compute/clusters/{id}", authorized(s.getCluster))
	mux.HandleFunc("PATCH /v1/regions/{region}/projects/{project}/compute/clusters/{id}", authorized(s.updateCluster))
	mux.HandleFunc("DELETE /v1/regions/{region}/projects/{project}/compute/clusters/{id}", authorized(s.deleteCluster))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/compute/clusters/{id}/scale", authorized(s.scaleCluster))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/compute/clusters/{id}/upgrade", authorized(s.upgradeCluster))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/compute/clusters/{id}/cancel", authorized(s.cancelCluster))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/compute/clusters/{id}/runtime/events", authorized(s.listClusterRuntimeEvents))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/compute/clusters/{id}/kubeconfigs", authorized(s.listKubeconfigs))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/compute/clusters/{id}/kubeconfigs/{kid}/download", authorized(s.downloadKubeconfig))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/compute/clusters/{id}/kubeconfig", authorized(s.getClusterKubeconfig))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/compute/clusters/{id}/ssh-access", authorized(s.listClusterSSHAccess))

	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/compute/instance-groups", authorized(s.listInstanceGroups))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/compute/instance-groups", authorized(s.createInstanceGroup))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/compute/instance-groups/{id}", authorized(s.getInstanceGroup))
	mux.HandleFunc("PATCH /v1/regions/{region}/projects/{project}/compute/instance-groups/{id}", authorized(s.updateInstanceGroup))
	mux.HandleFunc("DELETE /v1/regions/{region}/projects/{project}/compute/instance-groups/{id}", authorized(s.deleteInstanceGroup))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/compute/instance-groups/{id}/scale", authorized(s.scaleInstanceGroup))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/compute/instances", authorized(s.listInstances))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/compute/instances/{id}", authorized(s.getInstance))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/compute/instances/{id}/{action}", authorized(s.executeInstancePowerAction))

	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/networking/networks", authorized(s.listNetworks))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/networking/networks", authorized(s.createNetwork))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/networking/networks/{id}", authorized(s.getNetwork))
	mux.HandleFunc("PATCH /v1/regions/{region}/projects/{project}/networking/networks/{id}", authorized(s.updateNetwork))
	mux.HandleFunc("DELETE /v1/regions/{region}/projects/{project}/networking/networks/{id}", authorized(s.deleteNetwork))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/networking/networks/{networkId}/subnets", authorized(s.listSubnets))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/networking/networks/{networkId}/subnets", authorized(s.createSubnet))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/networking/networks/{networkId}/subnets/{id}", authorized(s.getSubnet))
	mux.HandleFunc("DELETE /v1/regions/{region}/projects/{project}/networking/networks/{networkId}/subnets/{id}", authorized(s.deleteSubnet))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/networking/vpcs", authorized(s.listVPCs))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/networking/vpcs/{id}", authorized(s.getVPC))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/networking/vpcs/{id}", authorized(s.setVPCSecurityGroups))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/networking/vpcs/{vpcId}/peerings", authorized(s.listVPCPeerings))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/networking/vpcs/{vpcId}/peerings", authorized(s.createVPCPeering))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/networking/vpcs/{vpcId}/peerings/{id}", authorized(s.getVPCPeering))
	mux.HandleFunc("DELETE /v1/regions/{region}/projects/{project}/networking/vpcs/{vpcId}/peerings/{id}", authorized(s.deleteVPCPeering))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/networking/security-groups", authorized(s.listSecurityGroups))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/networking/security-groups-effective", authorized(s.listSecurityGroupsEffective))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/networking/security-groups/{id}/attachments", authorized(s.listSecurityGroupAttachments))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/networking/security-groups", authorized(s.createSecurityGroup))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/networking/security-groups/{id}", authorized(s.getSecurityGroup))
	mux.HandleFunc("PATCH /v1/regions/{region}/projects/{project}/networking/security-groups/{id}", authorized(s.updateSecurityGroup))
	mux.HandleFunc("DELETE /v1/regions/{region}/projects/{project}/networking/security-groups/{id}", authorized(s.deleteSecurityGroup))

	mux.HandleFunc("GET /v1/regions/{region}/storage/share-types", authorized(s.listShareTypes))
	mux.HandleFunc("GET /v1/regions/{region}/storage/share-types/{id}", authorized(s.getShareType))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/storage/file-stores", authorized(s.listFileStores))
	mux.HandleFunc("POST /v1/regions/{region}/projects/{project}/storage/file-stores", authorized(s.createFileStore))
	mux.HandleFunc("GET /v1/regions/{region}/projects/{project}/storage/file-stores/{id}", authorized(s.getFileStore))
	mux.HandleFunc("PATCH /v1/regions/{region}/projects/{project}/storage/file-stores/{id}", authorized(s.updateFileStore))
	mux.HandleFunc("DELETE /v1/regions/{region}/projects/{project}/storage/file-stores/{id}", authorized(s.deleteFileStore))

	mux.HandleFunc("GET /v1/regions/global/notifications/webhooks", authorized(s.listWebhooks))
	mux.HandleFunc("POST /v1/regions/global/notifications/webhooks", authorized(s.createWebhook))
	mux.HandleFunc("DELETE /v1/regions/global/notifications/webhooks/{id}", authorized(s.deleteWebhook))
	mux.HandleFunc("POST /v1/regions/global/notifications/webhooks/{id}/rotate", authorized(s.rotateWebhook))
	mux.HandleFunc("GET /v1/regions/global/notifications/webhooks/{id}/deliveries", authorized(s.listWebhookDeliveries))

	mux.HandleFunc("GET /v1/regions/global/organizations/{orgId}/quota", authorized(s.getTenantQuota))
	mux.HandleFunc("GET /v1/regions/global/organizations/{orgId}/projects/{id}/quota", authorized(s.getProjectQuota))

	mux.HandleFunc("/", notFound)
}

// logging records whether a bearer was sent, never its value. This log lands in
// .demo/fake.log, an ordinary file nothing cleans up,
// and the provider promises no token reaches disk. The checks that read it,
// isMutation, MintCount and requestsScopedTo, parse only the method and path.
func logging(counts *requestCounts, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counts.add(r.Method, r.URL.EscapedPath())
		auth := ""
		if r.Header.Get("Authorization") != "" {
			auth = " auth=present"
		}
		// The escaped path, never the decoded one: a %0A or %20 decoded would
		// split the line or blur where the path ends, and the request audit
		// reads every line it can.
		log.Printf("%s %s ua=%q%s", r.Method, r.URL.EscapedPath(), r.Header.Get("User-Agent"), auth)
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

type cluster struct {
	id          string
	uid         string
	project     string
	ct          fakeClusterType
	version     fakeVersion // the bound version
	upgradeTo   *fakeVersion
	clusterType string // as resolved by the fake, not as sent
	displayName *string
	description *string
	tags        map[string]string
	tagsSent    bool // whether the create or update carried a `tags` key at all
	// securityGroups is the cluster's own binding, in the order sent.
	securityGroups []string
	pools          map[string]int
	targets        map[string]int // in-flight scale targets
	state          string
	stateMessage   string
	stateReason    string
	reads          int // reads since the last transition
	op             *fakeOperation
	opSeq          int
	// events is the runtime event log of the current or last operation.
	events []fakeEvent

	kubeconfigReads  int // kubeconfig reads since the cluster became active
	kubeconfigIssued bool
	kubeconfigServed int // for kubeconfigReissueDenied

	// ceiling is the storage ceiling in GiB the cluster declares, 0 for none
	// (v1.3.0-dev.4).
	ceiling int64
	// sshAccess is the users the create carried, served by
	// listClusterSshAccess and never changed after.
	sshAccess []sshUser

	// released says a delete started, which releases the node pools and the
	// SSH users. A teardown that fails leaves them gone: on a dev.4 landscape
	// a cluster left `failed` that way answered no node pools and
	// `[]` for its SSH users.
	released bool
	// teardownLost says a teardownFails cluster already failed its one
	// teardown.
	teardownLost bool

	// refusedBusy says a delete was refused while the cluster was busy. See
	// busyUntilRefused.
	refusedBusy bool
	// busiedForUpgrade says another run's operation already met an upgrade.
	// See busyAtFirstUpgrade.
	busiedForUpgrade bool
}

// The contract's bounds on a cluster's storage.ceilingGiB.
const (
	minCeilingGiB = 1
	maxCeilingGiB = 1048576
)

// clusterStorage is a create or update's `storage`.
type clusterStorage struct {
	CeilingGiB *int64 `json:"ceilingGiB"`
}

// ceilingRefusal is the 422 a ceiling outside the contract's bounds gets, or
// "" for one inside them.
func ceilingRefusal(st *clusterStorage) string {
	if st == nil || st.CeilingGiB == nil {
		return "storage.ceilingGiB is required"
	}
	if g := *st.CeilingGiB; g < minCeilingGiB || g > maxCeilingGiB {
		return fmt.Sprintf("storage.ceilingGiB %d is outside %d..%d", g, minCeilingGiB, maxCeilingGiB)
	}
	return ""
}

// fakeOperation is a cluster's current or last lifecycle operation.
type fakeOperation struct {
	executionID string
	status      string
	reason      string
	message     string
	startedAt   string
	completedAt string
	stages      []string
	done        int // stages finished so far
}

type instanceGroup struct {
	id           string
	uid          string
	project      string
	clusterType  string
	displayName  *string
	description  *string
	pools        map[string]int32 // desiredCount
	sshAccess    []sshUser        // as sent, echoed sorted by username
	held         map[string]int32 // readyCount + notReadyCount
	provisioning []string         // pools that change what they hold on settling
	state        string
	reads        int
	refusals     int  // scales refused by a transient marker since the last admitted one
	cancelled    bool // deleted while creating
	released     bool // a delete started, so the SSH users are gone
	teardownLost bool // a teardownFails group has already failed its one teardown
}

type state struct {
	mu        sync.Mutex
	scopes    map[scope]*objects
	seq       int
	subnetSeq int

	// lockLost holds each peering create already answered with the lost-lock
	// 503, so the next attempt goes through. See vpcLockLostMarker.
	lockLost map[string]bool

	// organizations are the ones a project can be created in. See
	// projects.go.
	organizations map[string]bool

	// Serves expiring kubeconfigs (clusterapi.go). Nil turns them off.
	clusterAPI *clusterAPI
}

// scope is where a project-owned object lives: the region and the project its
// path names. Those two are the whole of it, because the path names no
// organization, so the fake takes no view on whether one project id can exist
// in two organizations. Projects themselves stay keyed by id, in the global
// tier's objects.
type scope struct{ region, project string }

// objects is everything a project owns in one region: what lives under a
// /regions/{region}/projects/{project}/ path. Each table belongs to one entry
// in the object table in kinds.go, which is the only thing that keys into it.
type objects struct {
	scope      scope
	clusters   table[*cluster]
	groups     table[*instanceGroup]
	networks   table[*fakeNetwork]
	subnets    table[*fakeSubnet] // under their network
	vpcs       table[*fakeVPC]
	groupsSec  table[*fakeSecurityGroup]
	peerings   table[*fakePeering] // under their VPC
	fileStores table[*fakeFileStore]
	// released are the records of instances whose group was deleted, which
	// the contract keeps and never reaps. See instances.go.
	released []fakeInstance
	// power is the last power action accepted for each host, by instance id.
	power map[string]powered

	// Only the global tier's objects, the scope with no region and no
	// project, hold these. See projects.go, webhooks.go and
	// serviceaccounts.go.
	projects        table[*fakeProject]
	webhooks        table[*fakeWebhook]
	serviceAccounts table[*fakeServiceAccount]
	credentials     table[*fakeCredential] // under their service account
}

// newState is a fake with nothing in it but its seeds, which are its own: no
// two fakes share a row, so a test that changes one leaves the next alone.
func newState() *state {
	global := newObjects(scope{})
	for _, seed := range projectSeeds {
		p := seed
		global.projects[at{id: p.id}] = &p
	}
	return &state{scopes: map[scope]*objects{{}: global}, organizations: seededOrganizations()}
}

// global is the global tier's objects, where the projects, the webhook
// subscriptions and the service accounts are, whatever scope a request names.
// The caller holds s.mu.
func (s *state) global() *objects {
	return s.scopes[scope{}]
}

func newObjects(sc scope) *objects {
	return &objects{
		scope:      sc,
		clusters:   table[*cluster]{},
		groups:     table[*instanceGroup]{},
		networks:   table[*fakeNetwork]{},
		subnets:    table[*fakeSubnet]{},
		vpcs:       table[*fakeVPC]{},
		groupsSec:  table[*fakeSecurityGroup]{},
		peerings:   table[*fakePeering]{},
		fileStores: table[*fakeFileStore]{},
		power:      map[string]powered{},

		projects:        table[*fakeProject]{},
		webhooks:        table[*fakeWebhook]{},
		serviceAccounts: table[*fakeServiceAccount]{},
		credentials:     table[*fakeCredential]{},
	}
}

// in is the objects of the scope a request's path names, and nothing else.
// Every handler for a project-owned object goes through it, so an object in
// another project answers exactly as one that does not exist: the same 404 on
// a read or a delete, and no row in a list. Without this, a provider that sent
// a request to the wrong project would find what it was looking for and pass.
// The caller holds s.mu.
func (s *state) in(r *http.Request) *objects {
	sc := scope{r.PathValue("region"), r.PathValue("project")}
	o, ok := s.scopes[sc]
	if !ok {
		o = newObjects(sc)
		s.scopes[sc] = o
	}
	return o
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// The catalog
// ---------------------------------------------------------------------------

type fakePool struct {
	id          string
	role        string
	def         *int32
	min         *int32
	max         *int32
	displayName string
	// since is the first version of the ClusterType that has this pool, and
	// 0 means every version does. A pool a later version brings materializes
	// when an upgrade into that version lands, which is what moves a
	// cluster's node total during an upgrade.
	since int32
}

// at reports whether the pool exists at ClusterType version v.
func (p fakePool) at(v int32) bool { return p.since == 0 || v >= p.since }

type fakeClusterType struct {
	id          string
	displayName string
	description string
	status      string
	version     int32
	pools       []fakePool
	versions    []fakeVersion
	// noGroupInterface marks a type whose node pools have no interface on a
	// network in a NICo-backed VPC, like the kind stack's seed type, so a
	// cluster of it can carry no security group.
	noGroupInterface bool
}

// fakeVersion is one entry in a ClusterType's history.
//
// Each carries its own k0s version, and two of them deliberately carry the
// same one: a new ClusterType version may change node pools or CIDRs and leave
// Kubernetes where it was, so a provider that treated this list as a list of
// distinct Kubernetes versions would be wrong. A fake where every version had a
// different k0s version would not catch that.
type fakeVersion struct {
	versionID   string
	version     int32
	displayName string
	description string
	status      string
	createdAt   string
	k0sVersion  string
	// poolMin and poolMax move a pool's bounds at this version, which is what
	// makes an upgrade's bounds check, and the order of a scale around it,
	// reachable.
	poolMin, poolMax map[string]int32
	// upgradeFails makes an upgrade into this version settle `failed`.
	upgradeFails bool
	// bmaasOnly drops the `k8s` block, so no Kubernetes cluster can be
	// upgraded into this version.
	bmaasOnly bool
}

func i32(v int32) *int32 { return &v }

// The catalog. `gpu-training` deliberately has two pools, one of them a
// control-plane pool that cannot be scaled, so that a cluster created with a
// single-pool override reads back with two materialized pools: hazard H2, the
// node-pool materialization asymmetry. A one-pool catalog would hide it.
var catalog = []fakeClusterType{
	{
		id:          "gpu-training",
		displayName: "GPU training",
		description: "Two node pools: a control plane and a scalable GPU worker pool.",
		status:      "available",
		version:     3,
		pools: []fakePool{
			{id: "control-plane", role: "control-plane", displayName: "Control plane", def: i32(1), min: i32(1), max: i32(1)},
			{id: "gpu-workers", role: "worker", displayName: "GPU workers", def: i32(2), min: i32(0), max: i32(16)},
		},
		// Oldest first, as the contract states, and covering all three
		// statuses. Versions 2 and 3 share a k0s version on purpose.
		versions: []fakeVersion{
			{
				versionID: "11111111-1111-1111-1111-111111111111", version: 1,
				displayName: "GPU training v1", description: "First cut.",
				status: "deprecated", createdAt: "2026-01-04T09:00:00Z", k0sVersion: "v1.34.0+k0s.0",
			},
			{
				versionID: "22222222-2222-2222-2222-222222222222", version: 2,
				displayName: "GPU training v2", description: "Adds the control-plane pool.",
				status: "available", createdAt: "2026-04-18T09:00:00Z", k0sVersion: "v1.35.1+k0s.0",
			},
			{
				versionID: "33333333-3333-3333-3333-333333333333", version: 3,
				displayName: "GPU training v3", description: "Wider GPU worker bounds, same Kubernetes.",
				status: "available", createdAt: "2026-08-02T09:00:00Z", k0sVersion: "v1.35.1+k0s.0",
			},
			{
				versionID: "44444444-4444-4444-4444-444444444444", version: 4,
				displayName: "GPU training v4", description: "Not published yet.",
				status: "draft", createdAt: "2026-09-15T09:00:00Z", k0sVersion: "v1.36.0+k0s.0",
			},
		},
	},
	{
		id:          "cpu-general",
		displayName: "CPU general purpose",
		description: "A single worker pool, for the case where nothing is materialized implicitly.",
		status:      "available",
		version:     1,
		// The one type whose pools have no interface a security group can go
		// on, so the 422 for that is reachable.
		noGroupInterface: true,
		pools: []fakePool{
			{id: "workers", role: "worker", displayName: "Workers", def: i32(1), min: i32(1), max: i32(8)},
		},
		versions: []fakeVersion{
			{
				versionID: "55555555-5555-5555-5555-555555555555", version: 1,
				displayName: "CPU general v1", description: "The only version.",
				status: "available", createdAt: "2026-02-11T09:00:00Z", k0sVersion: "v1.34.0+k0s.0",
			},
		},
	},
	{
		// The upgrade chain: a draft an upgrade skips, a version that narrows
		// a pool's bounds, and one an upgrade into fails.
		id:          "gpu-inference",
		displayName: "GPU inference",
		description: "A version history built for upgrades.",
		status:      "available",
		version:     5,
		pools: []fakePool{
			{id: "control-plane", role: "control-plane", displayName: "Control plane", def: i32(1), min: i32(1), max: i32(1)},
			{id: "workers", role: "worker", displayName: "Workers", def: i32(2), min: i32(1), max: i32(8)},
		},
		versions: []fakeVersion{
			{
				versionID: "aaaaaaaa-0000-4000-8000-000000000001", version: 1,
				displayName: "GPU inference v1", status: "available",
				createdAt: "2026-03-01T09:00:00Z", k0sVersion: "v1.34.0+k0s.0",
			},
			{
				versionID: "aaaaaaaa-0000-4000-8000-000000000002", version: 2,
				displayName: "GPU inference v2", description: "At most four workers.", status: "available",
				createdAt: "2026-04-01T09:00:00Z", k0sVersion: "v1.34.0+k0s.0",
				poolMax: map[string]int32{"workers": 4},
			},
			{
				versionID: "aaaaaaaa-0000-4000-8000-000000000003", version: 3,
				displayName: "GPU inference v3", description: "Never published.", status: "draft",
				createdAt: "2026-05-01T09:00:00Z", k0sVersion: "v1.35.1+k0s.0",
			},
			{
				versionID: "aaaaaaaa-0000-4000-8000-000000000004", version: 4,
				displayName: "GPU inference v4", status: "available",
				createdAt: "2026-06-01T09:00:00Z", k0sVersion: "v1.35.1+k0s.0",
			},
			{
				versionID: "aaaaaaaa-0000-4000-8000-000000000005", version: 5,
				displayName: "GPU inference v5", description: "Its upgrade does not reconcile.", status: "available",
				createdAt: "2026-07-01T09:00:00Z", k0sVersion: "v1.36.0+k0s.0",
				upgradeFails: true,
			},
		},
	},
	boundsType("gpu-minraise", "bbbbbbbb", "A later version raises the workers' minimum to 3.",
		fakeVersion{}, fakeVersion{poolMin: map[string]int32{"workers": 3}}),
	boundsType("gpu-minlower", "cccccccc", "A later version lowers the workers' minimum from 2 to 1.",
		fakeVersion{poolMin: map[string]int32{"workers": 2}}, fakeVersion{}),
	boundsType("gpu-chain", "dddddddd", "The third version allows one worker; the fourth is BMaaS-only.",
		fakeVersion{}, fakeVersion{}, fakeVersion{poolMax: map[string]int32{"workers": 1}},
		fakeVersion{bmaasOnly: true}, fakeVersion{}),
	poolGrowType(),
	{
		// No `available` version, so the list leaves it out unless asked, and
		// a create can't bind it.
		id:          "gpu-preview",
		displayName: "GPU preview",
		description: "Only a draft so far.",
		status:      "draft",
		version:     1,
		pools: []fakePool{
			{id: "workers", role: "worker", displayName: "Workers", def: i32(1), min: i32(1), max: i32(8)},
		},
		versions: []fakeVersion{
			{
				versionID: "ffffffff-0000-4000-8000-000000000001", version: 1,
				displayName: "GPU preview v1", status: "draft",
				createdAt: "2026-10-01T09:00:00Z", k0sVersion: "v1.36.0+k0s.0",
			},
		},
	},
}

// poolGrowType is a ClusterType whose second version brings a pool the first
// doesn't have, so an upgrade into it changes the cluster's node total though
// no pool the configuration names was scaled.
func poolGrowType() fakeClusterType {
	ct := boundsType("gpu-poolgrow", "eeeeeeee", "The second version brings a one-node system pool.",
		fakeVersion{}, fakeVersion{})
	ct.pools = append(ct.pools,
		fakePool{id: "system", role: "worker", displayName: "System", def: i32(1), min: i32(1), max: i32(1), since: 2})
	return ct
}

// boundsType builds a ClusterType whose versions differ only in pool bounds,
// all available, numbered from 1 in the order given. Version n's id is
// prefix-0000-4000-8000-00000000000n.
func boundsType(id, prefix, description string, versions ...fakeVersion) fakeClusterType {
	ct := fakeClusterType{
		id: id, displayName: id, description: description, status: "available",
		version: int32(len(versions)),
		pools: []fakePool{
			{id: "control-plane", role: "control-plane", displayName: "Control plane", def: i32(1), min: i32(1), max: i32(1)},
			{id: "workers", role: "worker", displayName: "Workers", def: i32(2), min: i32(1), max: i32(8)},
		},
	}
	for i, v := range versions {
		n := int32(i + 1)
		v.versionID = fmt.Sprintf("%s-0000-4000-8000-%012d", prefix, n)
		v.version, v.status = n, "available"
		v.displayName = fmt.Sprintf("%s v%d", id, n)
		v.createdAt, v.k0sVersion = "2026-08-01T09:00:00Z", "v1.35.1+k0s.0"
		ct.versions = append(ct.versions, v)
	}
	return ct
}

// isLoopback reports whether a listen host is the local machine. An empty host
// means "every interface", which is exactly what this refuses.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func findClusterType(id string) (fakeClusterType, bool) {
	for _, ct := range catalog {
		if ct.id == id {
			return ct, true
		}
	}
	return fakeClusterType{}, false
}

// clusterTypePath is the head-scoped reference for a ClusterType.
//
// ASSUMPTION: the live API returns a **relative
// path** for `clusterType` and `selfLink`, despite both being declared
// `format: uri`. This is the one assumption in this file with live evidence
// behind it rather than none: the console's client carries a standing overlay
// rewriting the `InstanceGroup` twin to `uri-reference` because the declared
// format "rejects real responses". So the fake never returns an absolute URL,
// and nothing in the provider's path may validate strictly on `format: uri`.
func clusterTypePath(region, id string) string {
	return "/v1/regions/" + region + "/compute/cluster-types/" + id
}

// resolveClusterTypeRef models what the platform does to a `clusterType` on
// write, and it models the damaging reading.
//
// ASSUMPTION: the server **resolves an unversioned
// reference to a pinned one** on write. The contract contradicts itself here:
// the create schema says the `/versions/{id}` segment is optional, the response
// schema says the value "Reflects the versioned URI supplied at creation time",
// and both cannot hold for an unversioned write. Of the two readings this is the
// one that breaks a provider: if the server resolves, then a user who wrote the
// head form sees a different value read back, on the only required argument of
// `createCluster`, which is also immutable. So the fake resolves. A fake that
// echoed would let a provider that simply compares strings pass, and that
// provider would then diff forever against a real server.
//
// A pinned reference binds that version, an unpinned one the head, and either
// must be `available`. It returns the resolved reference, and the ClusterType and version it bound.
func resolveClusterTypeRef(region, ref string) (string, fakeClusterType, fakeVersion, bool) {
	path := ref
	if i := strings.Index(path, "://"); i >= 0 {
		// Tolerate an absolute reference on the way in. Real gateways accept
		// what they issue, and the provider is documented as accepting both.
		if j := strings.Index(path[i+3:], "/"); j >= 0 {
			path = path[i+3+j:]
		}
	}
	path = strings.TrimSuffix(path, "/")

	prefix := "/v1/regions/" + region + "/compute/cluster-types/"
	rest, ok := strings.CutPrefix(path, prefix)
	if !ok || rest == "" {
		return "", fakeClusterType{}, fakeVersion{}, false
	}

	slug, pin, pinned := strings.Cut(rest, "/versions/")
	ct, found := findClusterType(slug)
	if !found {
		return "", fakeClusterType{}, fakeVersion{}, false
	}
	v, found := headVersion(ct)
	if pinned {
		v, found = findVersion(ct, pin)
	}
	if !found || v.status != "available" {
		return "", fakeClusterType{}, fakeVersion{}, false
	}
	// Always answer with the pinned form, whichever form arrived.
	return versionRef(region, ct, v), ct, v, true
}

func (s *state) listRegions(w http.ResponseWriter, r *http.Request) {
	rows := make([]any, 0, len(fakeRegions))
	for _, region := range fakeRegions {
		rows = append(rows, region)
	}
	regionList.serve(w, r, rows)
}

// listProjects reads the same map the org-scoped operations write, so a project
// created in one apply is findable in the next. It used to answer with a fixed
// literal, which meant the plural data source and the resource could disagree
// about what exists and nothing would notice.
func (s *state) listProjects(w http.ResponseWriter, r *http.Request) {
	org := r.URL.Query().Get("organizationId")
	// A case-insensitive substring match on the id and the display name.
	search := strings.ToLower(r.URL.Query().Get("search"))
	status := r.URL.Query().Get("status")

	s.mu.Lock()
	defer s.mu.Unlock()

	matching := []any{}
	caller := scopedTo(r)
	for _, p := range projectKind.under(s.global(), "") {
		if (org != "" && p.org != org) || !caller.sees(p.org, p.id) {
			continue
		}
		if status != "" && p.status != status {
			continue
		}
		if search != "" && !strings.Contains(p.id, search) &&
			!strings.Contains(strings.ToLower(p.displayName), search) {
			continue
		}
		matching = append(matching, p.body())
	}
	projectList.serve(w, r, matching)
}

func (s *state) listClusterTypes(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")
	if region == "global" {
		// The global tier's gateway disables the compute routes, so the real
		// answer is the gateway's own no-route 404.
		gatewayNoRoute(w)
		return
	}
	// A type with no `available` version is left out unless asked for, as
	// the contract after this provider's pinned one has it. That contract
	// reads `includeDeprecated` as an alias of `includeUnavailable`.
	include := r.URL.Query().Get("includeDeprecated") == "true"
	items := make([]any, 0, len(catalog))
	for _, ct := range catalog {
		if ct.status != "available" && !include {
			continue
		}
		items = append(items, map[string]any{
			"id":          ct.id,
			"displayName": ct.displayName,
			"description": ct.description,
			"status":      ct.status,
			"version":     ct.version,
			"selfLink":    clusterTypePath(region, ct.id),
		})
	}
	clusterTypeList.serve(w, r, items)
}

func (s *state) getClusterType(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")
	ct, ok := findClusterType(r.PathValue("id"))
	if !ok {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "no such cluster type")
		return
	}
	writeJSON(w, http.StatusOK, clusterTypeJSON(region, ct, nil))
}

// listClusterTypeVersions answers the history, oldest first, unpaged.
func (s *state) listClusterTypeVersions(w http.ResponseWriter, r *http.Request) {
	ct, ok := findClusterType(r.PathValue("id"))
	if !ok {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "no such cluster type")
		return
	}

	want := r.URL.Query().Get("status")
	switch want {
	case "", "available", "draft", "deprecated":
	default:
		// 422, not an empty list. An unrecognised filter value is a rejected
		// request, and a fake that answered "no matches" would let a provider
		// report an empty catalogue where the server reports an error.
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"status "+want+" is not one of available, draft, deprecated")
		return
	}

	items := []any{}
	for _, v := range ct.versions {
		if want != "" && v.status != want {
			continue
		}
		items = append(items, map[string]any{
			"versionId":   v.versionID,
			"version":     v.version,
			"displayName": v.displayName,
			"description": v.description,
			"status":      v.status,
			"createdAt":   v.createdAt,
			// No k0sVersion, deliberately: the contract does not carry one on
			// the list record, which is what makes the provider fan out. A fake
			// that added it would hide the round trip it costs.
		})
	}
	clusterTypeVersionList.serve(w, r, items)
}

// getClusterTypeVersion answers the whole ClusterType as it was at a version,
// with a version-scoped selfLink rather than the head-scoped one.
func (s *state) getClusterTypeVersion(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")
	ct, ok := findClusterType(r.PathValue("id"))
	if !ok {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "no such cluster type")
		return
	}

	id := r.PathValue("versionId")
	if !looksLikeUUID(id) {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "versionId is not a UUID")
		return
	}
	for _, v := range ct.versions {
		if v.versionID == id {
			writeJSON(w, http.StatusOK, clusterTypeJSON(region, ct, &v))
			return
		}
	}
	apiError(w, http.StatusNotFound, "NOT_FOUND", "no such cluster type version")
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// clusterTypeJSON renders a ClusterType, at its head when version is nil and at
// the named version otherwise. The k0s version and the selfLink are what differ;
// the pools do not, apart from a pool that only a later version has.
func clusterTypeJSON(region string, ct fakeClusterType, version *fakeVersion) map[string]any {
	pools := make([]any, 0, len(ct.pools))
	at := ct.version
	if version != nil {
		at = version.version
	}
	for _, p := range ct.pools {
		if !p.at(at) {
			continue
		}
		pool := map[string]any{
			"id":          p.id,
			"role":        p.role,
			"displayName": p.displayName,
			"machineType": "/v1/regions/" + region + "/compute/machine-types/fake-" + p.id,
			// networkAttachment and networkSchema are required by the contract
			// and empty here: the provider reads neither, and inventing a shape
			// for them would be inventing a contract.
			"networkAttachment": map[string]any{},
		}
		if p.def != nil {
			pool["nodeCountDefault"] = *p.def
		}
		if p.min != nil {
			pool["nodeCountMin"] = *p.min
		}
		if p.max != nil {
			pool["nodeCountMax"] = *p.max
		}
		if version != nil {
			if min, ok := version.poolMin[p.id]; ok {
				pool["nodeCountMin"] = min
			}
			if max, ok := version.poolMax[p.id]; ok {
				pool["nodeCountMax"] = max
			}
		}
		pools = append(pools, pool)
	}

	// The contract's floor is v1.34.0, so anything lower would be a value the
	// real API rejects.
	status, seq, selfLink, k0s := ct.status, ct.version, clusterTypePath(region, ct.id), "v1.34.0+k0s.0"
	if version != nil {
		status, seq, k0s = version.status, version.version, version.k0sVersion
		selfLink = clusterTypePath(region, ct.id) + "/versions/" + version.versionID
	}

	out := map[string]any{
		"id":          ct.id,
		"displayName": ct.displayName,
		"description": ct.description,
		"status":      status,
		"version":     seq,
		"versionId":   versionIDOf(ct, version),
		"selfLink":    selfLink,
		"nodePools":   pools,
		"k8s": map[string]any{
			"clusterTemplateName":    "fake-template",
			"clusterTemplateVersion": "v1-34-0",
			"k0sVersion":             k0s,
		},
		"networkSchema": map[string]any{},
	}
	if version != nil && version.bmaasOnly {
		delete(out, "k8s")
	}
	return out
}

// boundVersion is the catalog entry a resolved `clusterType` reference names,
// in either of the two pinned forms resolveClusterTypeRef produces.
func boundVersion(ref string) (fakeClusterType, fakeVersion, bool) {
	rest := ref[strings.Index(ref, "/cluster-types/")+len("/cluster-types/"):]
	slug, pin, _ := strings.Cut(rest, "/versions/")
	ct, ok := findClusterType(slug)
	if !ok {
		return fakeClusterType{}, fakeVersion{}, false
	}
	for _, v := range ct.versions {
		if v.versionID == pin || fmt.Sprintf("%d", v.version) == pin {
			return ct, v, true
		}
	}
	return ct, fakeVersion{}, false
}

func versionIDOf(ct fakeClusterType, version *fakeVersion) any {
	if version != nil {
		return version.versionID
	}
	if len(ct.versions) == 0 {
		return nil
	}
	// The head is the newest, and the history is oldest first. The contract
	// says getClusterType "always resolves to the head's literal latest
	// version, whatever its status", which isn't what an unversioned create
	// binds: that's the newest `available` one.
	return ct.versions[len(ct.versions)-1].versionID
}

// ---------------------------------------------------------------------------
// Clusters
// ---------------------------------------------------------------------------

func (s *state) createCluster(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")

	var body struct {
		ID          *string            `json:"id"`
		ClusterType string             `json:"clusterType"`
		DisplayName *string            `json:"displayName"`
		Description *string            `json:"description"`
		Tags        *map[string]string `json:"tags"`
		NodePools   *[]struct {
			ID        string `json:"id"`
			NodeCount *int   `json:"nodeCount"`
		} `json:"nodePools"`
		SecurityGroups *[]string       `json:"securityGroups"`
		Storage        *clusterStorage `json:"storage"`
		// Not echoed, since a Cluster has no sshAccess: listClusterSshAccess
		// reports it.
		SSHAccess []sshUser `json:"sshAccess"`
	}
	if !decode(w, r, &body) {
		return
	}
	if msg := invalidSSHAccess(body.SSHAccess); msg != "" {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", msg)
		return
	}
	if body.Storage != nil {
		if msg := ceilingRefusal(body.Storage); msg != "" {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "validation: "+msg)
			return
		}
	}
	if body.ClusterType == "" {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "clusterType is required")
		return
	}
	resolved, ct, version, ok := resolveClusterTypeRef(region, body.ClusterType)
	if !ok {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"clusterType does not resolve to an available cluster type version in this region: "+body.ClusterType)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o, _, ok := clusterKind.admit(s, w, r)
	if !ok {
		return
	}
	id, ok := clusterKind.claim(s, w, o, "", body.ID)
	if !ok {
		return
	}

	// Materialize every pool on the bound ClusterType, using the override where
	// one was given.
	//
	// ASSUMPTION (hazard H2): a sparse override list materializes the full pool
	// set. This one is documented rather than guessed: "The server materializes
	// one node pool for every entry in `ClusterType.nodePools[]`, using each
	// pool's `nodeCountDefault` when no override is supplied", and it is
	// modelled because a fake that returned only the pools it was sent would let
	// a naive list-to-list provider pass.
	pools := map[string]int{}
	for _, p := range ct.pools {
		if !p.at(version.version) {
			continue
		}
		if p.def != nil {
			pools[p.id] = int(*p.def)
		} else {
			pools[p.id] = 0
		}
	}
	if body.NodePools != nil {
		for _, override := range *body.NodePools {
			if _, known := pools[override.ID]; !known {
				apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
					"no node pool "+override.ID+" on the bound cluster type")
				return
			}
			if override.NodeCount != nil {
				pools[override.ID] = *override.NodeCount
			}
		}
	}
	if msg := outOfBounds(ct, version, pools); msg != "" {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", msg)
		return
	}
	var groups []string
	if body.SecurityGroups != nil {
		groups = *body.SecurityGroups
		if !o.admitsGroups(w, ct, groups) {
			return
		}
	}

	c := &cluster{
		id:          id,
		uid:         uidFor(o.scope, "cluster", id),
		project:     r.PathValue("project"),
		ct:          ct,
		version:     version,
		clusterType: resolved,
		displayName: body.DisplayName,
		description: body.Description,
		pools:       pools,
		state:       "creating",

		securityGroups: groups,
		sshAccess:      trimmedSSHKeys(body.SSHAccess),
	}
	if body.Storage != nil {
		c.ceiling = *body.Storage.CeilingGiB
	}
	if body.Tags != nil {
		c.tagsSent = true
		c.tags = *body.Tags
	}
	c.start("provisioning the cluster", "reserving-hardware", "provisioning-nodes", "bootstrapping-control-plane", "joining-workers")
	clusterKind.put(o, "", id, c)
	// A VPC is materialized by the cluster workflow, never created directly.
	o.vpcForOwner("cluster", id)

	// ASSUMPTION: a mutation response is a different
	// shape from a read. `nextAvailableClusterTypeVersion` is "Populated only on
	// the read paths" and mutation responses "never populate it", and a create
	// reports the transient state rather than the settled one. Modelled because
	// a provider that stores the create body and then refreshes sees a change
	// nobody made, which is exactly a first-plan-after-create diff.
	writeJSON(w, http.StatusCreated, s.clusterBody(region, c, false))
}

func (s *state) getCluster(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")

	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := clusterKind.read(w, r, s.in(r))
	if !ok {
		return
	}
	body := s.clusterBody(region, c, true)
	// Progress and stages are "present only on get-by-id with
	// `includeStages=true`", so a provider that never asks never sees them.
	if r.URL.Query().Get("includeStages") == "true" && c.op != nil {
		op := c.op.json()
		op["progress"], op["stages"] = c.op.progress()
		body["operation"] = op
	}
	writeJSON(w, http.StatusOK, body)
}

// listClusterSSHAccess answers the users the create carried, `[]` for none.
// Sorted by username, as a k0rdent API build from main answers an instance group's, with
// each user's keys in the order sent: no landscape has answered this read
// yet, so the fake answers it the way the platform answers the same field
// elsewhere.
func (s *state) listClusterSSHAccess(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _, ok := clusterKind.find(w, r, s.in(r))
	if !ok {
		return
	}
	users := slices.Clone(c.sshAccess)
	if c.released {
		users = nil
	}
	if users == nil {
		users = []sshUser{}
	}
	slices.SortFunc(users, func(a, b sshUser) int { return strings.Compare(a.Username, b.Username) })
	writeBody(w, http.StatusOK, users)
}

func (s *state) listClusters(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")

	s.mu.Lock()
	defer s.mu.Unlock()
	clusters, _ := clusterKind.list(w, r, s.in(r))

	items := make([]any, 0, len(clusters))
	for _, c := range clusters {
		items = append(items, s.clusterBody(region, c, true))
	}
	clusterList.serve(w, r, items)
}

func (s *state) updateCluster(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")

	var body struct {
		DisplayName    *string            `json:"displayName"`
		Description    *string            `json:"description"`
		Tags           *map[string]string `json:"tags"`
		SecurityGroups *[]string          `json:"securityGroups"`
		Storage        *clusterStorage    `json:"storage"`
	}
	if !decode(w, r, &body) {
		return
	}
	// Measured on the k0rdent API v1.3.0-dev.3: securityGroups combined with
	// any other field answers 422, as the contract says it does, so a provider
	// that folded it into the metadata PATCH fails here as it would there.
	if body.SecurityGroups != nil && (body.DisplayName != nil || body.Description != nil || body.Tags != nil) {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"securityGroups cannot be combined with other fields in one PATCH; send it on its own")
		return
	}
	if body.Storage != nil {
		msg := ceilingRefusal(body.Storage)
		if msg == "" && body.SecurityGroups != nil {
			msg = "storage cannot be combined with securityGroups"
		}
		if msg != "" {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "validation: "+msg)
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	c, _, ok := clusterKind.find(w, r, o)
	if !ok {
		return
	}
	// v1.3.0-rc.3 admits securityGroups from `active` or `failed` until a
	// delete is accepted, tags, displayName and description in any state but
	// `deleting`, and a storage raise only from `active`.
	if body.SecurityGroups != nil {
		if c.released {
			msg := "cluster \"" + c.id + "\" has a delete that did not complete; retry the delete"
			if c.state == "deleting" {
				msg = "cluster \"" + c.id + "\" is being deleted"
			}
			apiError(w, http.StatusConflict, "CONFLICT", msg)
			return
		}
		groups := *body.SecurityGroups
		// `[]` detaches everything and needs no interface to go on.
		if len(groups) > 0 && !o.admitsGroups(w, c.ct, groups) {
			return
		}
		if c.state != "active" && c.state != "failed" {
			apiError(w, http.StatusConflict, "CONFLICT",
				"cluster \""+c.id+"\" cannot change its security groups in state \""+c.state+"\"")
			return
		}
		// Asynchronous: 202, and the cluster is `updating` while the
		// bundles are re-rendered.
		c.securityGroups = groups
		c.state, c.reads = "updating", 0
		c.stateReason, c.stateMessage = "", ""
		c.start("applying security groups", "rendering-network-bundles")
		writeJSON(w, http.StatusAccepted, s.clusterBody(region, c, false))
		return
	}
	if c.state == "deleting" {
		apiError(w, http.StatusConflict, "CONFLICT", "cluster \""+c.id+"\" is being deleted")
		return
	}
	if body.Storage != nil {
		want := *body.Storage.CeilingGiB
		if want < c.ceiling {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				fmt.Sprintf("storage.ceilingGiB cannot be lowered from %d to %d", c.ceiling, want))
			return
		}
		// An unchanged ceiling is no raise, so any state takes it.
		if want != c.ceiling && c.state != "active" {
			// v1.3.0-dev.4: a raise on a cluster that isn't ACTIVE answers 409
			// with no reason, and can be retried once it is.
			errorEnvelope(w, http.StatusConflict, "", "conflict: cluster \""+c.id+"\" is "+c.state+
				"; a storage raise needs it active", nil)
			return
		}
		c.ceiling = want
	}

	// A PATCH: an absent field is left alone and an empty one clears it.
	c.displayName = patchString(c.displayName, body.DisplayName)
	c.description = patchString(c.description, body.Description)
	if body.Tags != nil {
		c.tagsSent = true
		c.tags = *body.Tags
	}

	writeJSON(w, http.StatusOK, s.clusterBody(region, c, false))
}

func (s *state) scaleCluster(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")

	var body struct {
		NodePools []struct {
			ID        string `json:"id"`
			NodeCount int    `json:"nodeCount"`
		} `json:"nodePools"`
	}
	if !decode(w, r, &body) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	c, _, ok := clusterKind.find(w, r, o)
	if !ok {
		return
	}
	if c.state != "active" {
		apiError(w, http.StatusConflict, "CONFLICT", "the cluster is "+c.state)
		return
	}
	if len(body.NodePools) == 0 {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "nodePools is required")
		return
	}

	// The three validations the contract states, modelled because each one is a
	// way a provider can be wrong: a no-op target, an unknown pool, and a
	// request mixing increases with decreases.
	up, down := 0, 0
	for _, want := range body.NodePools {
		current, known := c.pools[want.ID]
		if !known {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "no node pool "+want.ID+" on this cluster")
			return
		}
		if want.NodeCount == current {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				fmt.Sprintf("pool %s is already at %d; there is nothing to scale", want.ID, current))
			return
		}
		if want.NodeCount > current {
			up++
		} else {
			down++
		}
	}
	if up > 0 && down > 0 {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"a single scale request may not mix increases with decreases")
		return
	}
	// Checked against the bound version's bounds, as the platform does.
	wanted := map[string]int{}
	for _, want := range body.NodePools {
		wanted[want.ID] = want.NodeCount
	}
	if msg := outOfBounds(c.ct, c.version, wanted); msg != "" {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", msg)
		return
	}

	c.targets = map[string]int{}
	for _, want := range body.NodePools {
		c.targets[want.ID] = want.NodeCount
	}
	c.state = "updating"
	c.reads = 0
	c.start("scaling node pools", "reserving-hardware", "provisioning-nodes")

	writeJSON(w, http.StatusOK, s.clusterBody(region, c, false))
}

func (c *cluster) start(message string, stages ...string) {
	c.opSeq++
	c.op = &fakeOperation{
		executionID: fmt.Sprintf("%s-op-%d", c.id, c.opSeq),
		status:      "running",
		message:     message,
		startedAt:   fixedTime,
		stages:      stages,
	}
	c.events = nil
	c.record("info", "cluster", "", message)
}

// finish ends the current operation. One that failed or was cancelled carries
// the cluster's own stateReason and stateMessage, as the contract's Operation does.
func (c *cluster) finish(status string) {
	c.op.status, c.op.completedAt = status, fixedTimeDone
	if status != "succeeded" {
		c.op.reason, c.op.message = c.stateReason, c.stateMessage
	}
}

func (o *fakeOperation) json() map[string]any {
	out := map[string]any{
		"executionId": o.executionID,
		"status":      o.status,
		"message":     o.message,
		"startedAt":   o.startedAt,
	}
	if o.completedAt != "" {
		out["completedAt"] = o.completedAt
	}
	if o.reason != "" {
		out["reason"] = o.reason
	}
	return out
}

func (o *fakeOperation) progress() (map[string]any, []any) {
	stages := make([]any, 0, len(o.stages))
	current := ""
	for i, key := range o.stages {
		status := "pending"
		switch {
		case i < o.done:
			status = "done"
		case i == o.done && o.status == "running":
			status, current = "inProgress", key
		}
		stages = append(stages, map[string]any{"key": key, "label": key, "status": status})
	}
	total := len(o.stages)
	inProgress := 0
	if current != "" {
		inProgress = 1
	}
	progress := map[string]any{
		"completed":       o.done,
		"total":           total,
		"percentComplete": o.done * 100 / max(total, 1),
		"counts": map[string]any{
			"done": o.done, "inProgress": inProgress, "pending": total - o.done - inProgress,
			"failed": 0, "skipped": 0,
		},
	}
	if current != "" {
		progress["current"] = current
	}
	return progress, stages
}

func (s *state) deleteCluster(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	c, where, ok := clusterKind.find(w, r, o)
	if !ok {
		return
	}
	switch c.state {
	case "creating", "updating":
		c.refusedBusy = true
		apiError(w, http.StatusConflict, "CONFLICT", fmt.Sprintf(
			"cluster %q is %s; delete requires active or failed. Cancel the in-flight operation first "+
				"(POST .../clusters/%s/cancel)", c.id, c.state, c.id))
		return
	case "deleting":
		// Recorded on v1.3.0-dev.4: no reason, no details.
		recordedConflict(w, fmt.Sprintf(
			"conflict: cluster %q is already being deleted; wait for the delete to settle", c.id), "")
		return
	}
	c.released = true
	clusterKind.tearDown(o, where, c)
	c.stateReason, c.stateMessage = "", ""
	c.start("deleting the cluster", "draining", "releasing-hardware")
	w.WriteHeader(http.StatusNoContent)
}

// advance moves a cluster one step through whatever transition it is in.
//
// ASSUMPTION (hazard H6): create, scale and delete are asynchronous, and a
// cluster reports a transient state until it settles. That much the contract
// says. What it does not say is the sequence, and the fake models the part that
// matters for a provider: while a scale is in flight the pool carries
// `targetNodeCount` alongside the old `nodeCount`, and the key disappears once
// the pool settles. A provider that waits on `state != updating` but reads
// `nodeCount` too early gets the old number, which is the bug this models.
//
// A delete is the object table's to finish: see clusterKind. This never
// finds a cluster gone.
func (o *objects) advance(c *cluster) bool {
	if c.settleCancel() {
		return false
	}
	if c.state == "active" || (c.state == "updating" && c.upgradeTo != nil && nameCarries(c.displayName, holdsUpgrade)) {
		return false
	}
	if (c.state == "creating" || c.state == "updating") && nameCarries(c.displayName, neverSettles) {
		return false
	}
	if holdsUntilRefused(c.state, c.displayName, c.refusedBusy) {
		return false
	}
	c.reads++
	lasts := settleAfter
	if c.state == "creating" && nameCarries(c.displayName, createsSlowly) {
		lasts = slowReads
	}
	if c.reads < lasts {
		return false
	}
	c.reads = 0
	if c.state == "creating" && nameCarries(c.displayName, createFails) {
		c.state = "failed"
		c.stateReason, c.stateMessage = "RESERVATION_FAILED", "no hosts could be reserved for the node pools"
		c.finish("failed")
		return false
	}
	switch c.state {
	case "creating", "updating":
		c.complete()
	}
	return false
}

// complete lands the operation in flight: the pools reach their targets, an
// upgrade moves the binding, and the cluster settles `active`, or `failed`
// when the upgrade does.
func (c *cluster) complete() {
	for id, target := range c.targets {
		c.pools[id] = target
	}
	c.targets = nil
	c.state = "active"
	if c.upgradeTo != nil {
		settleUpgrade(c)
	}
	c.op.done = len(c.op.stages)
	if c.state == "failed" {
		c.finish("failed")
	} else {
		c.finish("succeeded")
	}
}

// clusterBody renders a Cluster. read selects the read shape; a mutation
// response is deliberately narrower.
func (s *state) clusterBody(region string, c *cluster, read bool) map[string]any {
	pools := make([]any, 0, len(c.pools))
	total := 0
	held := c.pools
	if c.released {
		// The platform soft-deletes the pools as it starts the delete, and
		// dev.4 answered `"nodePools":[]` and `"nodeCount":0` from then on
		// (live-shapes-dev4.txt, section E).
		held = nil
	}
	// In id order, so two reads of one cluster agree.
	for _, id := range sortedMapKeys(held) {
		count := held[id]
		pool := map[string]any{
			"id":                    id,
			"clusterTypeNodePoolId": id,
			"nodeCount":             count,
			"createdAt":             fixedTime,
			"updatedAt":             fixedTime,
		}
		if target, pending := c.targets[id]; pending {
			pool["targetNodeCount"] = target
		}
		pools = append(pools, pool)
		total += count
	}

	body := map[string]any{
		"id":          c.id,
		"clusterType": c.clusterType,
		"nodePools":   pools,
		"nodeCount":   total,
		"state":       c.state,
		// ASSUMPTION (hazard H4): the response is required and "Always present;
		// `[]` when none", so a cluster created without security groups reads
		// back with an empty list against a configuration that wrote nothing,
		// never with the field missing.
		"securityGroups": clusterGroups(c),
		"selfLink":       "/v1/regions/" + region + "/projects/" + c.project + "/compute/clusters/" + c.id,
		"uid":            c.uid,
		"createdAt":      fixedTime,
		"updatedAt":      fixedTime,
		"health":         map[string]any{"state": "unknown"},
	}
	if c.op != nil {
		body["operation"] = c.op.json()
	}
	if hosted := hostedControlPlane(c); hosted != nil {
		body["controlPlane"] = map[string]any{"hosted": hosted}
	}
	if c.displayName != nil {
		body["displayName"] = *c.displayName
	}
	if c.description != nil {
		body["description"] = *c.description
	}
	// Hazard H9 was an empty `tags` map read back as the field being absent,
	// which cost a provider a permanent diff against `tags = {}`. v1.3.0-dev.4
	// settles it: `tags` and `systemTags` are required, "{} when there are
	// none", so an empty map is answered as {} and never left out.
	body["tags"] = map[string]string{}
	if len(c.tags) > 0 {
		body["tags"] = c.tags
	}
	body["systemTags"] = map[string]string{}
	if c.ceiling > 0 {
		body["storage"] = map[string]any{"ceilingGiB": c.ceiling}
	}
	if c.upgradeTo != nil {
		body["targetClusterTypeVersionId"] = c.upgradeTo.versionID
	}
	if c.stateMessage != "" {
		body["stateMessage"] = c.stateMessage
	}
	if c.stateReason != "" {
		body["stateReason"] = c.stateReason
	}
	if read {
		body["clusterTypeName"] = c.ct.displayName
		// Populated on reads only, and omitted when nothing newer is
		// available. This is the field the checklist names as the clearest
		// case of one resource having several truthful shapes.
		if next, ok := nextAvailable(c.ct, c.version.version); ok {
			body["nextAvailableClusterTypeVersion"] = versionRef(region, c.ct, next)
		}
	}
	return body
}

const (
	fixedTime     = "2026-09-21T00:00:00Z"
	fixedTimeDone = "2026-09-21T00:20:00Z"
)

// hostedControlPlane is the observed control plane, or nil while there is
// nothing observed yet. The contract never publishes the block partially, and
// its deprecated counters are still always present beside `state`.
func hostedControlPlane(c *cluster) map[string]any {
	if c.state == "creating" {
		return nil
	}
	_, v, _ := boundVersion(c.clusterType)
	hosted := map[string]any{
		"state":               "ready",
		"message":             "3 of 3 control-plane replicas are ready",
		"version":             v.k0sVersion,
		"desiredReplicas":     3,
		"readyReplicas":       3,
		"observedAt":          fixedTime,
		"ready":               true,
		"initialized":         true,
		"replicas":            3,
		"updatedReplicas":     3,
		"unavailableReplicas": 0,
	}
	if c.upgradeTo != nil {
		hosted["state"] = "progressing"
		hosted["message"] = "1 of 3 control-plane replicas run the requested version"
		hosted["updatedReplicas"] = 1
	}
	return hosted
}

// ---------------------------------------------------------------------------
// Kubeconfigs
// ---------------------------------------------------------------------------

// There is no create operation: the platform issues one kubeconfig per cluster
// when the cluster becomes active, and the fake does the same, a few reads late.
// See kubeconfigReady.
func (s *state) listKubeconfigs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _, ok := clusterKind.find(w, r, s.in(r))
	if !ok {
		return
	}
	switch {
	case c.state == "active" && nameCarries(c.displayName, kubeconfigFails):
		kubeconfigList.serve(w, r, []any{map[string]any{
			"id":            "kc-" + c.id,
			"status":        "failed",
			"failureReason": "the fake refused to issue this kubeconfig",
			"createdAt":     fixedTime,
			"selfLink":      "/v1/regions/" + r.PathValue("region") + "/projects/" + c.project + "/compute/clusters/" + c.id + "/kubeconfig",
		}})
	case !c.kubeconfigReady():
		// The list excludes a `pending` issuance, so it is empty until then.
		kubeconfigList.serve(w, r, nil)
	default:
		kubeconfigList.serve(w, r, []any{map[string]any{
			"id":        "kc-" + c.id,
			"status":    "active",
			"issuedFor": "cluster-admin",
			"createdAt": fixedTime,
			"selfLink":  "/v1/regions/" + r.PathValue("region") + "/projects/" + c.project + "/compute/clusters/" + c.id + "/kubeconfigs/kc-" + c.id,
		}})
	}
}

// kubeconfigReady reports whether the cluster's kubeconfig has been issued,
// counting this read toward issuing it.
//
// The contract calls the 404 transient: the platform creates a kubeconfig
// automatically once a cluster becomes active. Issuance runs after activation,
// so the fake holds the kubeconfig back for settleAfter reads after the
// cluster is active. A client that reads once on activation and gives up fails
// here, as it would against the real platform.
func (c *cluster) kubeconfigReady() bool {
	if c.kubeconfigIssued {
		return true
	}
	if c.state != "active" || nameCarries(c.displayName, kubeconfigFails) {
		return false
	}
	c.kubeconfigReads++
	if c.kubeconfigReads > settleAfter {
		c.kubeconfigIssued = true
	}
	return c.kubeconfigIssued
}

func (s *state) downloadKubeconfig(w http.ResponseWriter, r *http.Request) {
	s.serveKubeconfig(w, r, r.PathValue("kid"))
}

// getClusterKubeconfig is the canonical read: one kubeconfig per cluster, so
// no kubeconfig id.
func (s *state) getClusterKubeconfig(w http.ResponseWriter, r *http.Request) {
	s.serveKubeconfig(w, r, "kc-"+r.PathValue("id"))
}

func (s *state) serveKubeconfig(w http.ResponseWriter, r *http.Request, kid string) {
	id := r.PathValue("id")

	s.mu.Lock()
	defer s.mu.Unlock()
	c, _, ok := clusterKind.find(w, r, s.in(r))
	if !ok {
		return
	}
	if kid != "kc-"+c.id {
		apiError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("kubeconfig %q", kid))
		return
	}
	if !c.kubeconfigReady() {
		apiError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("cluster %q has no kubeconfig", id))
		return
	}
	c.kubeconfigServed++
	if nameCarries(c.displayName, kubeconfigReissueDenied) && c.kubeconfigServed == 2 {
		apiError(w, http.StatusForbidden, "PERMISSION_DENIED",
			fmt.Sprintf("the caller may no longer read the kubeconfig of cluster %q", id))
		return
	}
	if body, ok := s.expiringKubeconfig(c); ok {
		w.Header().Set("Content-Type", "application/yaml")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, body); err != nil {
			log.Printf("fakeapi: write response: %v", err)
		}
		return
	}

	// Declared `application/yaml`. Served as such so the generated client's
	// YAML200 field is the one that decodes; a different content type would be a
	// finding, not something for the fake to paper over.
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `apiVersion: v1
kind: Config
# NOT A REAL CREDENTIAL. Served by internal/tools/fakeapi for local
# verification. It authenticates against nothing.
clusters:
  - name: %s
    cluster:
      server: https://%s.fake.invalid:6443
contexts:
  - name: %s
    context:
      cluster: %s
      user: fake-admin
current-context: %s
users:
  - name: fake-admin
    user:
      token: not-a-real-token
`, c.id, c.id, c.id, c.id, c.id)
}

// ---------------------------------------------------------------------------
// Instance groups
// ---------------------------------------------------------------------------

func (s *state) createInstanceGroup(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")

	var body struct {
		ID          *string `json:"id"`
		ClusterType string  `json:"clusterType"`
		DisplayName *string `json:"displayName"`
		Description *string `json:"description"`
		NodePools   []struct {
			NodePool string `json:"nodePool"`
			Size     *int32 `json:"size"`
		} `json:"nodePools"`
		SSHAccess []sshUser `json:"sshAccess"`
	}
	if !decode(w, r, &body) {
		return
	}
	if msg := invalidSSHAccess(body.SSHAccess); msg != "" {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", msg)
		return
	}
	resolved, ct, _, ok := resolveClusterTypeRef(region, body.ClusterType)
	if !ok {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"clusterType does not resolve to an available cluster type version in this region: "+body.ClusterType)
		return
	}
	if len(body.NodePools) == 0 {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "nodePools is required")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o, _, ok := instanceGroupKind.admit(s, w, r)
	if !ok {
		return
	}
	id, ok := instanceGroupKind.claim(s, w, o, "", body.ID)
	if !ok {
		return
	}

	// Unlike a cluster, an instance group provisions exactly the pools it is
	// given, with no materialization.
	pools := map[string]int32{}
	for _, p := range body.NodePools {
		known := false
		for _, cp := range ct.pools {
			if cp.id == p.NodePool {
				known = true
				if p.Size != nil {
					pools[p.NodePool] = *p.Size
				} else if cp.def != nil {
					pools[p.NodePool] = *cp.def
				}
			}
		}
		if !known {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				"no node pool "+p.NodePool+" on the bound cluster type")
			return
		}
	}

	g := &instanceGroup{
		id:          id,
		uid:         uidFor(o.scope, "instance_group", id),
		project:     r.PathValue("project"),
		clusterType: resolved,
		displayName: body.DisplayName,
		description: body.Description,
		pools:       pools,
		sshAccess:   trimmedSSHKeys(body.SSHAccess),
		state:       "creating",
	}
	g.provisioning = sortedMapKeys(pools)
	instanceGroupKind.put(o, "", id, g)
	o.vpcForOwner("instance_group", id)
	writeJSON(w, http.StatusCreated, s.groupBody(region, g))
}

func (s *state) getInstanceGroup(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := instanceGroupKind.read(w, r, s.in(r))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.groupBody(r.PathValue("region"), g))
}

// advanceGroup is one read's step for an instance group. A delete is the
// object table's to finish, and so is a cancelled create once it has read
// back `failed`: "a follow-up GET shows creating, then briefly failed, then
// 404".
func advanceGroup(_ *objects, g *instanceGroup) bool {
	if g.state == "active" {
		return false
	}
	g.reads++
	lasts := settleAfter
	if g.state == "creating" && nameCarries(g.displayName, createsSlowly) {
		lasts = slowReads
	}
	if g.reads < lasts {
		return false
	}
	g.reads = 0
	switch {
	case g.state == "failed" && !g.cancelled:
		// A failed create or teardown stays failed until the next DELETE.
	case g.cancelled && g.state == "failed":
		return true
	case g.cancelled:
		g.state = "failed"
	case g.state == "creating" && nameCarries(g.displayName, createsForever):
	case g.state == "creating" && nameCarries(g.displayName, createFails):
		g.state = "failed"
	default:
		g.state = "active"
		g.provision()
	}
	return false
}

// groupFailsTeardown is the instance group's teardown outcome: a group named
// teardownFails fails its first teardown and settles `failed`, with the users
// the delete released still gone. The next DELETE succeeds.
func groupFailsTeardown(g *instanceGroup) bool {
	if !nameCarries(g.displayName, teardownFails) || g.teardownLost {
		return false
	}
	g.state, g.teardownLost = "failed", true
	return true
}

// clusterFailsTeardown is the cluster's teardown outcome: a cluster named
// teardownFails fails its first teardown and settles `failed`, with what the
// delete released still gone. The next DELETE succeeds.
func clusterFailsTeardown(c *cluster) bool {
	if !nameCarries(c.displayName, teardownFails) || c.teardownLost {
		return false
	}
	c.state, c.teardownLost = "failed", true
	return true
}

func (s *state) listInstanceGroups(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")

	s.mu.Lock()
	defer s.mu.Unlock()
	groups, _ := instanceGroupKind.list(w, r, s.in(r))

	items := make([]any, 0, len(groups))
	for _, g := range groups {
		items = append(items, s.groupBody(region, g))
	}
	instanceGroupList.serve(w, r, items)
}

func (s *state) updateInstanceGroup(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")

	var body struct {
		DisplayName    *string   `json:"displayName"`
		Description    *string   `json:"description"`
		SecurityGroups *[]string `json:"securityGroups"`
	}
	if !decode(w, r, &body) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	g, _, ok := instanceGroupKind.find(w, r, s.in(r))
	if !ok {
		return
	}
	if body.SecurityGroups != nil {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"this fake does not model securityGroups")
		return
	}
	g.displayName = patchString(g.displayName, body.DisplayName)
	g.description = patchString(g.description, body.Description)
	writeJSON(w, http.StatusOK, s.groupBody(region, g))
}

// inUse is the scale 409 the contract calls reason-less, in the shape the
// service writes it: no top-level reason and one CONFLICT_IN_USE detail.
func inUse(w http.ResponseWriter, message string) {
	errorEnvelope(w, http.StatusConflict, "", message,
		[]any{map[string]any{"reason": "CONFLICT_IN_USE", "message": message}})
}

// Markers that steer an instance group's scale into the refusals the contract
// documents. Each depends on platform state a client cannot create, so a test
// asks for it by display name, like settlesFailed.
const (
	// The first scaleRefusalsBeforeAdmit scales answer the reason-less 409 of
	// quota accounting still settling, and then the same request is admitted.
	scaleSettling = "fake-scale-settling"
	// The same, with the 503 of a pool size that cannot be confirmed.
	scaleUnconfirmed = "fake-scale-unconfirmed"
	// Every scale is refused QUOTA_EXCEEDED, with one host of project headroom.
	scaleOverQuota = "fake-scale-over-quota"
	// Every pool provisions one host fewer than it asks for, which is what a
	// scale that found no candidate hosts leaves behind.
	poolShort = "fake-pool-short"
	// Every scale is refused 409 with the details[].reason that follows, for
	// example fake-scale-refuses-RESERVATION_DRIFTED.
	scaleRefuses = "fake-scale-refuses-"
	// The create never finishes, so a create timeout can be reached.
	createsForever = "fake-creates-forever"
	// The teardown lasts slowTeardownReads reads rather than settleAfter, so a
	// test can plan against a group mid-delete and its destroy still finishes.
	// A cluster's lasts slowReads, so a destroy's timeout can be reached.
	deletesSlowly = "fake-deletes-slowly"
	// A cluster's or an instance group's create settles only after slowReads
	// reads, so a create timeout can be reached and the next apply can still
	// see it finish.
	createsSlowly = "fake-creates-slowly"
	// The first teardown fails and leaves the group `failed`, the way a
	// teardown the platform gives up on does. The next DELETE succeeds.
	teardownFails = "fake-teardown-fails"
	// A cluster's or an instance group's create settles `failed`, the way a
	// create the platform can't reserve hosts for does. A cluster's operation
	// finishes `failed` with reason RESERVATION_FAILED.
	createFails = "fake-create-fails"
)

// slowTeardownReads is how many reads a group marked deletesSlowly answers
// `deleting` for: more than a plan step's refreshes, few enough that the
// destroy's wait isn't noticed.
const slowTeardownReads = 8

// slowReads is how many reads a create marked createsSlowly, or a cluster's
// teardown marked deletesSlowly, lasts: at the acceptance suite's 100ms poll,
// about 3s, which outlasts a 1s timeout with room to spare.
const slowReads = 30

const scaleRefusalsBeforeAdmit = 2

var refusedReason = regexp.MustCompile(regexp.QuoteMeta(scaleRefuses) + `([A-Z_]+)`)

// provision records what the pools being provisioned now hold.
func (g *instanceGroup) provision() {
	if g.held == nil {
		g.held = map[string]int32{}
	}
	for _, id := range g.provisioning {
		held := g.pools[id]
		if nameCarries(g.displayName, poolShort) && held > 0 {
			held--
		}
		g.held[id] = held
	}
	g.provisioning = nil
}

func (s *state) scaleInstanceGroup(w http.ResponseWriter, r *http.Request) {
	region, project, id := r.PathValue("region"), r.PathValue("project"), r.PathValue("id")

	var body struct {
		NodePools []struct {
			NodePool string `json:"nodePool"`
			Size     int32  `json:"size"`
		} `json:"nodePools"`
	}
	if !decode(w, r, &body) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	g, _, ok := instanceGroupKind.find(w, r, s.in(r))
	if !ok {
		return
	}
	// "Another lifecycle operation ... already in flight rejects the scale with
	// no `reason`."
	if g.state != "active" {
		inUse(w, fmt.Sprintf("conflict: instance group %q is in state %s; scale requires the settled active state",
			id, g.state))
		return
	}
	if nameCarries(g.displayName, scaleUnconfirmed) && g.refusals < scaleRefusalsBeforeAdmit {
		g.refusals++
		message := fmt.Sprintf("instance group %q pool has an unconfirmed size and cannot be scaled right now", id)
		errorEnvelope(w, http.StatusServiceUnavailable, "", message,
			[]any{map[string]any{"reason": "UNAVAILABLE", "message": message}})
		return
	}
	if m := refusedReason.FindStringSubmatch(stringOr(g.displayName)); m != nil {
		message := "node pool cannot be scaled: " + m[1]
		errorEnvelope(w, http.StatusConflict, "", message,
			[]any{map[string]any{"reason": m[1], "message": message}})
		return
	}

	corrections, grows := map[string]int32{}, map[string]int32{}
	var increase int32
	for _, want := range body.NodePools {
		desired, known := g.pools[want.NodePool]
		if !known {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "no node pool "+want.NodePool+" on this group")
			return
		}
		held := g.held[want.NodePool]
		switch {
		case desired != held && want.Size == held:
			corrections[want.NodePool] = held
		case desired != held:
			message := fmt.Sprintf("node pool %q asks for %d hosts but its reservation holds %d; "+
				"send size %d to record what it holds, then scale from there", want.NodePool, desired, held, held)
			errorEnvelope(w, http.StatusConflict, "", message,
				[]any{map[string]any{"reason": "POOL_COUNT_MISMATCH", "message": message}})
			return
		case held == 0 && want.Size > 0:
			message := fmt.Sprintf("node pool %q holds no hosts and cannot be scaled", want.NodePool)
			errorEnvelope(w, http.StatusConflict, "", message,
				[]any{map[string]any{"reason": "POOL_EMPTY", "message": message}})
			return
		case want.Size <= held:
			// "must exceed the pool's current settled count (scale down is not
			// yet implemented)". Enforced, so a provider that tries to shrink in
			// place fails here rather than appearing to work.
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				fmt.Sprintf("pool %s is at %d; a scale must exceed the settled count", want.NodePool, held))
			return
		default:
			grows[want.NodePool] = want.Size
			increase += want.Size - held
		}
	}
	if len(corrections) > 0 && len(grows) > 0 {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"a desired-size correction cannot be combined with a scale of another pool")
		return
	}
	// The correction sets desiredCount to what the pool holds, provisions
	// nothing, and leaves the group active.
	if len(corrections) > 0 {
		for pool, size := range corrections {
			g.pools[pool] = size
		}
		writeJSON(w, http.StatusOK, s.groupBody(region, g))
		return
	}

	if nameCarries(g.displayName, scaleOverQuota) {
		var used int32
		for _, size := range g.held {
			used += size
		}
		const admissible = 1
		summary := fmt.Sprintf("node_count: requested %d, %d admissible (project limit binding)", increase, admissible)
		errorEnvelope(w, http.StatusConflict, "QUOTA_EXCEEDED",
			"conflict: instance group scale would exceed the quota: "+summary,
			[]any{map[string]any{
				"reason":  "QUOTA_EXCEEDED",
				"message": summary,
				"metadata": map[string]any{
					"dimension":    "node_count",
					"requested":    increase,
					"admissible":   admissible,
					"bindingScope": "project",
					"scopes": []any{map[string]any{
						"scope": "project", "targetId": project,
						"limit": used + admissible, "used": used, "headroom": admissible,
					}},
				},
			}})
		return
	}
	if nameCarries(g.displayName, scaleSettling) && g.refusals < scaleRefusalsBeforeAdmit {
		g.refusals++
		inUse(w, "conflict: a previous size change on this resource is still settling; retry shortly")
		return
	}

	for _, pool := range sortedMapKeys(grows) {
		g.pools[pool] = grows[pool]
		g.provisioning = append(g.provisioning, pool)
	}
	g.state = "updating"
	g.reads = 0
	g.refusals = 0
	writeJSON(w, http.StatusOK, s.groupBody(region, g))
}

func stringOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *state) deleteInstanceGroup(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	g, where, ok := instanceGroupKind.find(w, r, o)
	if !ok {
		return
	}
	id := g.id
	switch g.state {
	case "updating", "deleting":
		// Recorded on v1.3.0-dev.4 for `deleting`. ASSUMPTION: `updating`
		// answers the same way.
		recordedConflict(w, fmt.Sprintf(
			"conflict: instance group %q is in state %s; delete requires terminal state (active or failed)", id, g.state),
			"CONFLICT_IN_USE")
		return
	case "creating":
		// Cancels the create, and the group is torn down once that settles.
		g.cancelled, g.reads = true, 0
	default:
		instanceGroupKind.tearDown(o, where, g)
		g.released = true
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *state) groupBody(region string, g *instanceGroup) map[string]any {
	pools := make([]any, 0, len(g.pools))
	for _, id := range sortedMapKeys(g.pools) {
		size := g.pools[id]
		// The response names the requested size `desiredCount`. Answering with
		// the request's `size` would decode as the required field at zero.
		pool := map[string]any{"nodePool": id, "desiredCount": size}
		// Read live from the reservation, so present in flight too, beside a
		// desiredCount that is already the new target.
		if held, ok := g.held[id]; ok {
			pool["readyCount"], pool["notReadyCount"] = held, 0
		}
		pools = append(pools, pool)
	}
	body := map[string]any{
		"id":             g.id,
		"clusterType":    g.clusterType,
		"nodePools":      pools,
		"state":          g.state,
		"securityGroups": []any{},
		"selfLink":       "/v1/regions/" + region + "/projects/" + g.project + "/compute/instance-groups/" + g.id,
		"uid":            g.uid,
		"createdAt":      fixedTime,
		"updatedAt":      fixedTime,
		// Provisioning records per-pool progress from the first read on.
		"progressUpdatedAt": fixedTime,
	}
	if g.displayName != nil {
		body["displayName"] = *g.displayName
	}
	if g.description != nil {
		body["description"] = *g.description
	}
	if g.released {
		// The platform soft-deletes the users as it starts the delete, and
		// dev.4 answered `"sshAccess":[]` from then on (live-shapes-dev4.txt,
		// section E). Nothing puts them back if the teardown fails.
		body["sshAccess"] = []any{}
	} else {
		// Required since v1.3.0-dev.4: `[]` when the group has none.
		body["sshAccess"] = []any{}
	}
	if !g.released && len(g.sshAccess) > 0 {
		// A k0rdent API build from main answers the users sorted by username, on create, get
		// and list alike, with each user's keys in the order sent.
		users := slices.Clone(g.sshAccess)
		slices.SortFunc(users, func(a, b sshUser) int { return strings.Compare(a.Username, b.Username) })
		body["sshAccess"] = users
	}
	return body
}

// sshUser is the contract's `SshUser`.
type sshUser struct {
	Username      string   `json:"username"`
	SSHPublicKeys []string `json:"sshPublicKeys"`
	Sudo          *string  `json:"sudo,omitempty"`
	DisplayName   *string  `json:"displayName,omitempty"`
}

// trimmedSSHKeys stores each key the way a k0rdent API build from main does, with
// surrounding whitespace trimmed, so the echo differs from a key sent with a
// trailing newline just as the platform's does.
func trimmedSSHKeys(users []sshUser) []sshUser {
	out := slices.Clone(users)
	for i := range out {
		keys := make([]string, len(out[i].SSHPublicKeys))
		for j, k := range out[i].SSHPublicKeys {
			keys[j] = strings.TrimSpace(k)
		}
		out[i].SSHPublicKeys = keys
	}
	return out
}

// sshUsernamePattern is the contract's pattern for `SshUser.username`.
var sshUsernamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// invalidSSHAccess checks only what the contract states about each user: the
// username pattern and 1 to 32 keys. The fake takes no view on the keys
// themselves.
func invalidSSHAccess(users []sshUser) string {
	for i, u := range users {
		if !sshUsernamePattern.MatchString(u.Username) {
			return fmt.Sprintf("sshAccess[%d].username %q does not match %s", i, u.Username, sshUsernamePattern)
		}
		if n := len(u.SSHPublicKeys); n < 1 || n > 32 {
			return fmt.Sprintf("sshAccess[%d].sshPublicKeys has %d keys, want 1 to 32", i, n)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Auth and plumbing
// ---------------------------------------------------------------------------

// gateway is the gateway's authorization, which runs after the bearer check: a request under an
// organization or a project that doesn't exist is refused with the 403 the
// gateway answered on v1.3.0-dev.4 for every kind's read, list and create,
// because the caller holds no role on it and no service is asked. An
// organization route's own {project} is the object asked for, not a scope,
// so a project missing there is the service's 404.
func (s *state) gateway(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if scopedToProject(w, r) {
			return
		}
		s.mu.Lock()
		known, locked := true, ""
		switch org, project := r.PathValue("orgId"), r.PathValue("project"); {
		case org != "":
			known = s.organizations[org]
		case project != "":
			var p *fakeProject
			p, known = projectKind.rows(s.global())[at{id: project}]
			if known {
				locked = p.region
			}
		}
		s.mu.Unlock()
		if !known {
			writeBody(w, http.StatusForbidden, errorBody{Error: errorFields{
				Code: http.StatusForbidden, Message: "forbidden", RequestID: fakeRequestID,
			}})
			return
		}
		// v1.3.0-dev.4 checks every route under a project against the
		// project's region once the caller is authorized, before anything is
		// read or changed. The message doesn't name the project's region. A
		// project with none, made before regions were required, isn't checked.
		if region := r.PathValue("region"); locked != "" && region != "" && region != locked {
			apiError(w, http.StatusConflict, "PROJECT_REGION_MISMATCH",
				fmt.Sprintf("project %q is not in region %q", r.PathValue("project"), region))
			return
		}
		next(w, r)
	}
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(r.Body)
	// The contract sets `additionalProperties: false` on these bodies, so an
	// unknown field is a client bug and the fake says so rather than ignoring
	// it. This is how a provider learns it sent something the server does not
	// accept, instead of watching it silently vanish.
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "cannot decode the request body: "+err.Error())
		return false
	}
	return true
}

// notFound answers a path the fake does not serve the way the real gateway
// answers a path it has no route for. A path that reaches no service has no
// service to send the contract's envelope, and the provider counts a 404 as
// the resource being gone only when that envelope is there. So a moved path
// turns the harness red on Read and Delete too, rather than reading as gone.
func notFound(w http.ResponseWriter, _ *http.Request) {
	gatewayNoRoute(w)
}

// gatewayNoRoute writes the gateway's bare 404 for an unrouted path, the
// undocumented `{"message": ...}` shape the client decodes as the gateway's.
func gatewayNoRoute(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"message":    "no Route matched with those values",
		"request_id": "fake-request-id",
	})
}

// fakeRequestID stands in for the request id the platform puts on every
// error. Nothing the provider does branches on its value.
const fakeRequestID = "fake-request-id"

// apiError writes the contract's error envelope, which is what every service
// answers with. The gateway's bare shape is gatewayNoRoute.
func apiError(w http.ResponseWriter, status int, reason, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":      status,
			"message":   message,
			"reason":    reason,
			"requestId": fakeRequestID,
		},
	})
}

// errorEnvelope writes the contract's envelope with the reason and details the
// caller gives, and leaves out what it does not: several documented refusals
// carry no top-level reason, and apiError always writes one.
func errorEnvelope(w http.ResponseWriter, status int, reason, message string, details []any) {
	body := map[string]any{"code": status, "message": message, "requestId": fakeRequestID}
	if reason != "" {
		body["reason"] = reason
	}
	if details != nil {
		body["details"] = details
	}
	writeJSON(w, status, map[string]any{"error": body})
}

func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	writeBody(w, status, body)
}

// writeBody is writeJSON for a body whose field order matters, which a map
// can't keep.
func writeBody(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("fakeapi: write response: %v", err)
	}
}

// patchString applies one PATCH field: absent leaves it, empty clears it.
func patchString(current, sent *string) *string {
	switch {
	case sent == nil:
		return current
	case *sent == "":
		return nil
	default:
		return sent
	}
}

// uidFor gives an object the same UUID on every read, distinct per scope, kind
// and id, so a VPC's `ownerId` matches its owner's `uid` and the same id in two
// projects is two objects here too.
func uidFor(sc scope, kind, id string) string {
	h := fnv.New64a()
	h.Write([]byte(sc.region + "/" + sc.project + "/" + kind + "/" + id))
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", h.Sum64()&0xffffffffffff)
}
