# Testing

How to run each test, and what a pass does and doesn't prove. Run every
`make` from the repository root.

| Kind | Runs against | Needs |
| --- | --- | --- |
| Local | A fake API on `127.0.0.1` (`internal/tools/fakeapi`) | No credential |
| Live | A real k0rdent API | An endpoint, and service-account credentials for a project you can create in |

## Set up

You need make, Go 1.26.10 or later, Python 3, and Terraform or OpenTofu. On
macOS:

```sh
brew install make go python hashicorp/tap/terraform
```

Then install the pinned linter, put Go's bin directory on `PATH`, and check
the machine:

```sh
make lint-install
export PATH="$(go env GOPATH)/bin:$PATH"
make doctor
```

- `make lint-install` builds the golangci-lint version `make check` expects.
  Any other version can fail lint for reasons that aren't the code's.
- `make doctor` checks every local prerequisite and says how to install what's
  missing.

## Local tests

Fastest first:

| Command | A pass means |
| --- | --- |
| `make check` | Format, vet, lint, unit tests and the name audit pass |
| `make demo` | The whole lifecycle works against the fake: apply, re-plan, scale, import, destroy |
| `make plan-fake` | Credential handling and configuration errors work |
| `make drift` | No resource plans a change after an apply or a refresh |
| `make walkthrough-api-test` | The API walkthrough's commands work as the page shows them |
| `make testacc` | Each resource's lifecycle works through real Terraform, and `docs/testing-matrix.md` is what those tests do |
| `make plan-quality` | Refresh-only plans find nothing, and plans stay within the unknown budget |
| `make walkthrough-test` | The hardware walkthrough's directories work as the page says |
| `make upgrade-test` | State from the previous release plans empty under this tree, but for accepted changes |
| `make bench` | Plan, refresh and apply stay within the performance budget at 10 to 1,000 resources |

CI also runs `make docs-check`, `make doc-commands`, `make client-check`,
`make changelog-check`, `make shellcheck` and each script's own test.
[`.github/workflows/ci.yml`](.github/workflows/ci.yml) lists them all.
`make help` says what each target does.

None of these prove the real API behaves like the fake.
[`make real-api-check`](#make-real-api-check) and the other live tests do.

### `make check`

- Includes the state guard: `TestStateSchemaSnapshots` fails when a
  resource's stored state changes shape with no migration, and says what to
  add. The shapes are in `internal/provider/testdata/state-schemas`.
- With `terraform` on `PATH`, it also has the real CLI upgrade an old state
  and plan a `moved` block.

### `make testacc`

Real Terraform against the fake, one lifecycle per resource. Each case checks
its resources are gone after the destroy. CI runs the same command, so a
local run sees what CI sees.

- Tests run side by side against one fake, `TESTACC_PARALLEL` at a time, 8
  by default. The serial ones run first, one at a time. Lint fails a new test
  that doesn't call `t.Parallel()`, unless it calls `t.Setenv`, which Go
  keeps serial, or carries `//nolint:paralleltest // <why>`.
- Runs under `terraform`, then checks the [testing matrix](#the-testing-matrix).
  `make testacc TF_BIN=tofu` runs it under OpenTofu, without the matrix
  check, and CI runs both.
- `make testacc RUN=TestAccImport` runs only the tests the regexp matches.
  It doesn't record or check the matrix, so the last whole run's record
  stays.
- Under OpenTofu, a test that needs something only Terraform has skips and
  says why. `skipUnderOpenTofu` in `internal/acceptance/cli_test.go` holds
  the reasons:

| Skipped under OpenTofu | Why |
| --- | --- |
| The cancel and power action tests, and the three that cancel a cluster with the action | OpenTofu has no actions or `apply -invoke` |
| `TestAccQueryGeneratesConfigThatPlansClean` | OpenTofu has no `query` command or list resources |
| `TestAccEphemeralKubeconfigInTheSameRun` | OpenTofu checks an ephemeral resource's postconditions at plan, before it opens |
| `TestAccEphemeralKubeconfigDependsOnDoesNotDeferTheOpen` | It records a Terraform behaviour that OpenTofu doesn't share |

- `TestAccImport` for `k0rdent_file_store` expects OpenTofu's error at the
  generated-configuration step. OpenTofu 1.12 and 1.13 write a map key like
  `10.0.0.31` without quotes, then can't read it back. The step fails once
  OpenTofu fixes that.

#### Terraform and OpenTofu versions in CI

CI runs the acceptance suite under the two newest releases of Terraform and
of OpenTofu: for each, the newest patch of the newest minor, and the newest
patch of the minor before it. HashiCorp fixes only those two Terraform
minors. The `testacc` job's matrix in `.github/workflows/ci.yml` holds the
Terraform pair, and the `testacc-opentofu` job's matrix the OpenTofu pair.
Every other job runs the newer one.

- When either project ships a new patch or minor, the nightly's
  `make cli-releases-check` fails, and the nightly-failing issue says so.
  Move that CLI's pins in `.github/workflows/` in one pull request: the
  matrix to the two releases the check names, every other job to the newer
  one. For example, Terraform 1.18.0 moves the matrix from 1.16 and 1.17 to
  1.17's newest patch and 1.18.0.
- `scripts/check-workflow-actions.sh` refuses anything else: a third version,
  two that aren't adjacent minors, a matrix check name that doesn't show its
  version, or a job outside the matrix on the older release.

### The testing matrix

[docs/testing-matrix.md](docs/testing-matrix.md) names, for every attribute
of every resource, the acceptance tests that import it and change it. No
test declares what it covers. `make testacc` runs the suite with a recorder
around the provider, which writes down what each test made the provider do,
then checks the page against that record. The page is terraform's run: under
OpenTofu, the tests it can't run skip, so `make testacc TF_BIN=tofu` doesn't
check it. The page and the check depend only on the commit, so a release tag
changes neither.

- A gap is a cell no test covers yet. The page shows it as gap, and each one
  is a line in `internal/repocheck/testdata/testing-matrix-gaps.txt`. A cell
  with no test and no line fails the check, and so does a line for a cell a
  test now covers. Drop the line in the same change as the test.
- A change to an attribute that only sometimes replaces, such as
  `ssh_access`, counts for its update cell whether the plan updates it in
  place or replaces the resource. The page says which it saw.
- A test that runs a built provider binary has to add `matrixTestEnv(t)` to
  the command's environment, or the check fails.

Upgrades aren't on the page and the check doesn't gate them.
[`make upgrade-test`](#make-upgrade-test) is what gates upgrades. After the
check, `make testacc` reports upgrade coverage as information: which
attributes the tests upgrade from an older schema version, and which the
previous release's walkthrough sets, for the release `make upgrade-test`
upgrades from. It's in the job summary of CI's acceptance job, and in
`.testing-matrix/upgrade-coverage.md` after a local run. It never fails the
run. Without that release's tag, or without the network the release rule
needs, it says so in one line.

When the check says the page isn't current, regenerate it and commit it:

```sh
make testing-matrix
```

After a change to the gap list alone, `make testing-matrix-page` rewrites
the page from the last run's record in `.testing-matrix/`, without the suite.
Both refuse a record from a run that failed, ran only some tests or stopped
early, or one where a test skipped, which the check names. A test here skips
only through `skipTest`, which notes the skip in the record.

### `make demo`

Builds the provider, starts the fake, then applies, re-plans, scales, imports
and destroys.

- The fake answers like the API does, not with what it was sent: it pins an
  unversioned cluster type, returns relative paths, adds node pools and drops
  an empty tag map. So a clean re-plan is a real result.
- Each of those behaviours is an `ASSUMPTION` comment in
  `internal/tools/fakeapi`.

| Command | What it does |
| --- | --- |
| `make demo-up` | Leaves the fake running for hand edits |
| `make demo-env` | Prints the exports a new shell needs |
| `make demo-down` | Stops the fake |
| `make demo-session` | Use instead of `demo-up` in a runner that stops background processes after each command |

Set a different `DEMO_RUN_DIR` for each demo you run at the same time.

### `make plan-fake`

Checks the error for a missing credential, a bad secret, an endpoint ending
in `/v1`, and a service account and an API key set together. Also checks a
rejected token request isn't retried. Creates no resources.

### `make drift`

Applies every resource, then plans, refreshes and plans twice more. Any
planned change fails the run.

- A refresh also has to leave state unchanged. `refreshMayMove` in
  `internal/acceptance/perpetual_diff_test.go` names the attribute allowed
  to move.
- Objects with `fake-churns` in their id change on every read, in the fields
  the platform moves. `internal/tools/fakeapi/churn.go` has the rules.
- `TestPerpetualDiffCoversEveryResource` fails when a resource has no case.

### `make plan-quality` and the unknown budget

Applies every directory in `examples/walkthrough` against the fake, except
`nodes/`, which needs real hardware.

- After each apply and change, `terraform plan -refresh-only` must report no
  drift and no change.
- Every value the first plan shows as unknown is counted and held to
  `ci/unknown-budget.json`. The run fails when a count goes up or down, or a
  new path goes unknown.

**Updating the budget**, after a change that makes a value known at plan or
leaves a new one unknown:

```sh
make unknown-budget-update
git diff ci/unknown-budget.json
```

Give every new path a reason, since an empty one fails. Run
`make plan-quality`, then commit the file. If a count goes up, say why in the
pull request.

### `make walkthrough-test` and `make walkthrough-api-test`

Follow [docs/walkthrough-hardware.md](docs/walkthrough-hardware.md) and
[docs/walkthrough-api.md](docs/walkthrough-api.md) against the fake. A page
that no longer matches its directories or commands fails here.

### `make upgrade-test`

Applies each walkthrough directory, as the previous release shipped it, with
the previous release from the public registry. Then it switches to this
tree's build and plans again. Runs under `terraform` and `tofu`, and needs the
network.

- The previous release is the newest one both registries serve, leaving out
  a tag on `HEAD`. For a pull request into, or a push to, a `release-vX.Y.x`
  branch it's the newest up to X.Y. Locally, a branch with commits from a
  release branch counts as that branch. A tag either registry doesn't serve
  yet is skipped, with a warning on the run.
- A finding is a planned change, a changed output, drift, a failing
  `show -json` of the old state, or a plan that fails. Drift is an attribute
  the new build reads back differently, such as a renamed one. Drift the
  previous release sees on its own state too is the fake's, and isn't one.
- The directories are the previous release's, except `nodes/`. A variable
  in a `terraform.tfvars` that `scripts/lib/walkthrough.sh` doesn't fill in
  fails the run.
- Every resource type the previous release registers has to end up in some
  directory's state.
- `DIRS="look cluster"` runs only those directories, and skips the coverage
  check. `UPGRADE_KEEP=1` keeps the run directory.

**Accepting a change**, when it's meant and the upgrade notes say so: add it
to `changes` in `ci/upgrade-accepted.json`, with the release it's from, the
directory, the change as the run prints it and a reason. The release has to
be the one the check upgrades from by default: an entry for any other fails
the run, as does one nothing matches. So once a new release ships, its
entries go. A resource type no directory can create goes under
`not_covered`, with a reason.

`PREVIOUS=0.1.0-rc.1` upgrades from another release. The file only holds
changes from the default, so such a run reports every change from that
release, accepted or not. Read it, don't gate on it.

### `make bench`

Plan, refresh and apply at size, against a fresh fake with a delay added to
every request. A plan after the apply that isn't empty fails the run.
[docs/performance.md](docs/performance.md) has the figures.

The second line sets the sizes, parallelism and delay. The third is what the
nightly runs:

```sh
make bench
make bench BENCH_SIZES=100,1000 BENCH_PARALLELISM=10 BENCH_LATENCY=50ms
make bench BENCH_SIZES=100,500,1000 BENCH_PARALLELISM=10 BENCH_BUDGET=internal/tools/bench/budget.json
```

## Live tests

These run against a real k0rdent API. All but the preflight **create real
resources**. You need:

- An API endpoint: scheme and host, no `/v1`
- A region id. `make live-preflight` lists them
- A project id, or an organization id the run can create a project in
- Service-account credentials with a compute role on that project

`read -rs` reads the secret without echoing it, so it stays out of your shell
history:

```sh
export K0RDENT_ENDPOINT=https://api.example.com
export K0RDENT_REGION=<region id>
export K0RDENT_PROJECT=<project id>
export K0RDENT_CLIENT_ID=<client id>
printf 'client secret: ' >&2
read -rs K0RDENT_CLIENT_SECRET && export K0RDENT_CLIENT_SECRET && echo >&2
```

`K0RDENT_API_KEY` works instead of the two client variables, with a warning.

| Command | Creates | A pass means |
| --- | --- | --- |
| `make live-preflight` | nothing | The credential works, regions and cluster types read, and `terraform plan` runs |
| `make live` | a cluster, then destroys it | Apply, re-plan, scale, upgrade, import and destroy work against the real API |
| `make live-storage` | a cluster, a volume claim and a pod | A volume claim binds through the kubeconfig the provider hands over |
| `make sweep` | nothing, it deletes | No `acc-` test object from a killed run is left |

Run the preflight first. `LIVE_CONFIRM` has to name the endpoint's host:

```sh
make live-preflight
LIVE_CONFIRM=api.example.com make live
```

### `make live`

The same steps as `make demo`, against the real API.

- Always tries a destroy on the way out, after a failure or ctrl-C too.
- Writes a PASS, FAIL or SKIP line per step to `.live/results.txt`.
- Waits between token requests, since they're rate limited. Raise
  `LIVE_MINT_PACE` if you still get a 429.

| Variable | Effect |
| --- | --- |
| `LIVE_CONFIRM` | Required. The endpoint's host |
| `LIVE_CLUSTER` | The cluster id. Default `tf-live-<epoch>` |
| `LIVE_KEEP=1` | Skip the destroy, to inspect what was made. Writes the variables the cluster ended on to `.live/kept.tfvars` |
| `LIVE_WORKERS` | Worker count. Default 0 |
| `LIVE_UPGRADE=1` | Run the upgrade steps. On by default for localhost only |
| `TF_VAR_organization_id` | Create a project in this organization, and delete it after |
| `TF_VAR_project_id` | Build in this project, or name the new one |

A passing run:

```text
PASS  build
PASS  apply                  cluster tf-live-1789985940, 0 worker(s)
PASS  replan                 exit 0, no changes
PASS  scale                  0 -> 1, re-plan exit 0
PASS  upgrade                version 1 -> 3, 2 steps, re-plan exit 0
PASS  head-ref               in-place to the unversioned reference, re-plan exit 0
PASS  import                 re-plan exit 0
PASS  destroy
```

The upgrade steps need two or more `available` versions of the cluster type.
With fewer, they're SKIP.

### `make real-api-check`

The acceptance tests run against the fake, so they're only as good as the
fake's answers. This check runs the same Terraform against the fake and
against the real API on a local kind stack, and compares the shape of every
answer: each field's path and JSON type, never a value. Then it runs
`make live` against the stack. It refuses an endpoint that isn't on
localhost.

It runs:

- `look/` and every directory of `examples/walkthrough` the fake can apply,
  each applied, planned again and destroyed
- A cluster and an instance group with write-only SSH keys, applied, removed
  from state, imported and planned again (`ci/real-api/lifecycle`)
- `fakedrift compare` on the two recordings
- `make live` against the stack

Each difference `fakedrift` finds is either fixed in the fake or listed in
`ci/fake-drift-accepted.json` with a reason. A listed difference that no
longer turns up fails the check too, so the list can't go stale.

When every step passes, the check writes `ci/real-api/record.json`. Commit it
with the change. The record holds two hashes:

| Hash | Covers | CI checks it |
| --- | --- | --- |
| `fake` | The fake, the API client and its spec, `fakedrift`, the accepted list, the lifecycle and the script | On every pull request, with the API version in `ci/real-api/api-version` |
| `code` | Everything in `fake`, plus the provider, `make live` and the walkthroughs | Before a release, with `scripts/real-api-check.sh verify-release` |

So a pull request needs a new run when it changes the fake or the API client,
or moves `ci/real-api/api-version` to a new API. A change to the provider alone
doesn't, since its tests run against a fake that's known to match. A release
does.

`ci/real-api/paths` lists what each hash covers. The script reads it from the
tree it checks, so a release branch's tag is held to that branch's own lists,
and a change to the lists says so when the record is checked.

#### Set up the stack

`make real-api-stack` builds the stack on a kind cluster from the platform
API's source, and writes an env file that sets every variable the check reads.
You need:

- Read access to the platform API's source repository, and a GitHub token
  with `read:packages` for its images on `ghcr.io`. Both are private to the
  GitHub organization. Ask a platform maintainer for the repository and for
  access to it.
- Docker, kind, kubectl, helm, jq and curl, as well as what [Set up](#set-up)
  installs. The API source's `go.work` names the Go it builds with, 1.27.3 at
  v1.3.0. An older Go downloads that version the first time, as Go does
  by default.

1. Check out the API's source at the version `ci/real-api/api-version` names,
   and point the setup at it:

   ```sh
   export REAL_API_SOURCE=<path to the API source>
   git -C "$REAL_API_SOURCE" fetch --tags
   git -C "$REAL_API_SOURCE" checkout "$(cat ci/real-api/api-version)"
   ```

2. Log docker in to `ghcr.io`. The user is your GitHub user, and the password a
   token with `read:packages`:

   ```sh
   docker login ghcr.io
   ```

3. Build the stack. On an M-series Mac the first run took six minutes,
   mostly pulling images and installing. A run on a built stack took one:

   ```sh
   make real-api-stack
   ```

- It creates the kind cluster `k0rdent-rc5` and adds 6000 to every forwarded
  port, so the gateway is on `localhost:14000`. `REAL_API_KIND_CLUSTER` and
  `REAL_API_PORT_OFFSET` change them. It refuses a kind cluster of that name
  it didn't make, unless that cluster already runs the API or
  `REAL_API_KIND_REUSE=1` is set.
- It installs the API in mock mode, with the source's kind values and a mock
  identity provider. The secrets in those values are throwaway ones the
  source ships, so there's nothing to supply.
- It seeds a machine type, an address pool, and two cluster types:
  `seed-ct-local` with three versions for `make live`, and `kind-ct-subnets`
  with subnets for `full/`. It signs in as the mock identity provider's
  administrator to do it.
- It issues the two service accounts and writes `.live/real-api-kind.env`,
  mode 600, which git ignores. `REAL_API_ENV_FILE` puts it elsewhere.
- Run it again to restart the port-forwards after Docker or the machine
  restarts. It reuses what's there, writes the env file again, and issues new
  credentials only when the old ones stop working.
- `REAL_API_FRESH=1 make real-api-stack` deletes the cluster and builds it
  again. It deletes only a cluster it made, not one it found and installed
  into. Use it when `make real-api-check` says the stack holds what an
  earlier run left, as an interrupted run does. The check refuses such a
  stack before it starts, since `look/` would read those objects and the
  comparison would count them.

Then run the check with the env file:

```sh
. .live/real-api-kind.env
make real-api-check
git add ci/real-api/record.json
```

For a stack built another way, set the variables yourself:

| Variable | Value |
| --- | --- |
| `K0RDENT_ENDPOINT` | The stack's gateway, such as `http://localhost:14000` |
| `K0RDENT_REGION` | A region the stack serves |
| `K0RDENT_PROJECT` | A project to build in |
| `K0RDENT_CLIENT_ID`, `K0RDENT_CLIENT_SECRET` | A service account scoped to that project, with `compute-admin` and `credentials-admin`, so an import can check SSH keys |
| `REAL_API_ORG_CLIENT_ID`, `REAL_API_ORG_CLIENT_SECRET` | A service account scoped to the organization, with `compute-admin`, `organizations-admin` and `iam-viewer`, for the walkthroughs, as `docs/walkthrough-hardware.md` lists them |
| `REAL_API_CLUSTER_TYPE` | A cluster type in the region with a worker pool and two or more `available` versions. `make live` uses it too, and a skipped step fails the check |
| `REAL_API_WALKTHROUGH_GAPS` | What the stack can't do that a walkthrough asks for. `subnet-cidr`: the stack answers a subnet's CIDR without allocating it from the pool, so `full/` doesn't check it. `no-file-store`: no storage service, so `full-everything` leaves `file_store` off. `no-hosts`: no server for an instance group's host, so `full-everything` leaves `instance_group` and `vpc_peering` off. A kind stack needs all three. The record lists them, and a release names the resources they leave to `make real-api-check-hardware`. Default none |
| `REAL_API_KUBE_CONTEXT` | The stack's kube context. The API version is read from its images. Default `kind-k0rdent-rc5` |

### `make real-api-check-hardware`

A kind stack can't do everything the walkthroughs ask for, so some resources
never meet the real API in `make real-api-check`. This runs them on a
landscape with hardware: `look/`, then `full/` with the switches for what kind
can't do turned on, against the fake and against the landscape, through the
same recorder, then `fakedrift compare`. It creates real clusters and hosts.

It refuses to start unless `REAL_API_CONFIRM` names the endpoint's host, and it
refuses localhost, where `make real-api-check` runs. Before it creates
anything, a preflight reads the landscape: the credential's token, the region,
the project, the cluster type and its versions, the address pool and, with
file stores on, a share type. A missing piece fails there, in seconds.

| Build switch in `full/` | Here | Why |
| --- | --- | --- |
| `network`, `security_group`, `vpc_security_groups` | On | Subnets allocated from a real pool, and groups on a real VPC |
| `instance_group`, `vpc_peering` | On | Kind has no host for a group |
| `file_store` | On | Kind runs no storage service |
| `webhook`, `own_project` | Off | Kind covers both. A webhook to an address nobody serves gets turned off partway through a long run, and a project of its own needs `organizations-admin` |

A cluster with hosts can take two hours to create and as long to destroy. Each
Terraform command prints a line when it starts, with its log file, and one when
it ends, with how long it took. In between, a line every five minutes names
what Terraform is still creating or destroying. `results.txt` gets a line as
each step ends, so a killed run still leaves one.

On any exit, ctrl-C included, the run destroys what it made. With
`REAL_API_KEEP=1` it leaves everything in place and prints how to destroy it.
When a destroy fails, the summary lists what each directory's state still holds
and the command that removes it. Every name starts with the run's id,
`rapi-hw-<number>`, so what a killed run left is easy to find.

When every step passes, it writes `ci/real-api/hardware-record.json`: the
landscape's label, the API version and where it came from, the code hash, the
build switches and the resources covered. `scripts/real-api-check.sh
verify-release` prints it. When it doesn't cover the release's code, or a
resource kind can't run isn't in it, the release check says so plainly, such as
`kind only, no hardware for: k0rdent_file_store`.

| Variable | Value |
| --- | --- |
| `K0RDENT_ENDPOINT` | The landscape's gateway, https |
| `REAL_API_CONFIRM` | The endpoint's host, to confirm the landscape |
| `K0RDENT_REGION`, `K0RDENT_PROJECT` | Where to build. The walkthrough takes them from here, not from what `look/` lists first |
| `K0RDENT_CLIENT_ID`, `K0RDENT_CLIENT_SECRET` | A service account scoped to the organization, with `compute-admin`, `organizations-viewer` and `iam-viewer` |
| `REAL_API_CLUSTER_TYPE` | A cluster type in the region with a worker pool and hardware free for one control plane, its workers and one host |
| `REAL_API_VERSION` or `REAL_API_KUBE_CONTEXT` | The landscape's API version, stated, or a kube context to read it from the API's images. The record says which |
| `REAL_API_LANDSCAPE` | A label for the record, never a host name. Default `hardware` |
| `REAL_API_ORGANIZATION` | The organization the quota checks read. Default: the first `look/` lists |
| `REAL_API_ADDRESS_POOL` | A pool that isn't routable. Default: the first active one |
| `REAL_API_BUILD_OFF` | Switches to turn off as well, such as `file_store` when the landscape's storage is down. The record lists what they leave uncovered |
| `REAL_API_KEEP` | `1` leaves everything in place |

```sh
set -a; . ./hardware.env; set +a
REAL_API_CONFIRM=api.example.com make real-api-check-hardware
git add ci/real-api/hardware-record.json
```

### Seed a cluster type

`make live` builds from a cluster type in the region's catalog. An empty
catalog passes the preflight but leaves nothing to build. Only a platform
operator can publish a cluster type, so ask whoever runs the deployment. Ask
for two `available` versions to run the upgrade steps.

### `make live-storage`

Creates a cluster with one worker, connects the `hashicorp/kubernetes`
provider through `k0rdent_cluster_kubeconfig`, and creates a volume claim
and a pod that mounts it. Then plans again, expects no changes, and destroys
everything.

```sh
LIVE_CONFIRM=api.example.com make live-storage
```

It needs:

- A kubeconfig whose server the machine running Terraform can reach
- A cluster type with a worker pool, and room for one node. `LIVE_WORKERS`
  raises the count
- A default StorageClass, or one named in `TF_VAR_storage_class_name`
- An image with `sh` the cluster can pull. `TF_VAR_probe_image` names a
  mirror
- Registry access for `terraform init`, to fetch `hashicorp/kubernetes`

Results go to `.live/storage/results.txt`. `make live-storage-validate`
checks the configuration without an endpoint and creates nothing.

### `make sweep`

Deletes every object whose id starts with `acc-`, children first, in the
configured project and in every `acc-` project.

```sh
SWEEP_CONFIRM=api.example.com make sweep SWEEP_REGION=<region id>
```

- Uses the provider's endpoint, project and credential variables.
  `SWEEP_CONFIRM` has to name the endpoint's host.
- Deletes an `acc-` project only once it's empty in every region.
- Doesn't check age, so it also removes `acc-` objects a parallel run is
  still using.

### Drift against a live API

Keep what `make live` made, then refresh and plan a few minutes apart. A plan
that doesn't exit 0 is a finding.

- `.live/kept.tfvars` holds the variables the run ended on: the cluster id,
  the worker count after the scale, and the cluster type and version the
  upgrade pinned. Without the version, every plan moves the cluster back to
  the unversioned reference. Without the type, it lands on whichever type
  sorts first in the catalog.
- Use the `acc-` prefix, so `make sweep` can clean up a killed run.

```sh
LIVE_KEEP=1 LIVE_CONFIRM=api.example.com LIVE_CLUSTER=acc-live-drift make live
export TF_CLI_CONFIG_FILE="$PWD/.live/terraform.rc"
cd examples/live
terraform apply -refresh-only -auto-approve -var-file=../../.live/kept.tfvars
terraform plan -detailed-exitcode -var-file=../../.live/kept.tfvars
terraform destroy -auto-approve -var-file=../../.live/kept.tfvars
```

## Writing tests

Five rules keep the suite fast to run and easy to trust:

- Test each behavior at the lowest layer that can see it. A plan modifier's
  decision is a unit test. An acceptance test checks only what needs
  Terraform: the wire shape, state across a refresh, import, and the plan
  the CLI shows.
- Name the resource in the test's name, or in the subtest's when one test
  covers several, such as `TestAccInstanceGroupSSHAccess`. A resource missing
  a case then shows up in `go test -list`.
- Start an acceptance test with `t.Parallel()`, and give everything it
  creates a name no other test uses. The fake is shared, so a change to a
  shared object, such as revoking an SSH key, reaches the tests running
  beside it.
- A test that reads a whole list, or binds a VPC, which lists every security
  group, runs in a project of its own from `ownProject`. A list read in pages
  skips a row when a test beside it deletes one ahead of it.
- Delete a test only when every mutant it catches is caught by another test.
  A mutant is the code with one line broken on purpose, such as a condition
  negated. Two tests can run the same lines and check different things, so
  matching coverage doesn't make a test redundant.

## Looks like a failure, isn't

- **`make check` prints a long list under "Places a rename must edit".**
  That's the name audit listing files. It fails only when a Go file is
  listed. Check `make check; echo $?`.
- **`Terminated: 15` after the PASS of `make demo` or `make plan-fake`.** The
  script stopping its fake.
- **"Provider development overrides are in effect"** on every command. The
  local build is in use, as intended.
- **Don't run `terraform init` with `dev_overrides`.** There's nothing to
  install.
- **An empty cluster-type list in `make live-preflight`.** A pass. See
  [Seed a cluster type](#seed-a-cluster-type).
- **`apply ... 0 worker(s)` in `make live`.** Intended. Growing an empty pool
  needs no extra hardware.
- **An ephemeral kubeconfig failing with `not found: cluster` at plan.** Pass
  `cluster_id = <cluster>.state != null ? <cluster>.id : null` so Terraform
  waits for the cluster. `depends_on` doesn't.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| `429 Too Many Requests` on the token request | Wait a minute, or raise `LIVE_MINT_PACE` |
| `401` with an API key, repeatedly | Wrong key, or its creator's account changed. Use a service account |
| `403 forbidden` on a project route | The credential needs a compute role on the project |
| `403 Principal type … is not in the allowed list` | That route doesn't accept service accounts |
| `Invalid region` | Region ids are lower-case. Copy one from `make live-preflight` |
| `API endpoint includes a version segment` | Drop the `/v1` |
| `Ambiguous credentials` | Unset either the API key or the service account |
| `make check` fails only on lint | Run `make lint-install` |

## Reporting a result

Open an issue with the command, the PASS and FAIL lines (`.live/results.txt`
for a live run), the failing step's full output, and your Terraform or
OpenTofu version. Never paste a client secret, a token or a kubeconfig.
