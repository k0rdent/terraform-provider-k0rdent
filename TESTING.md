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
| `make testacc` | Each resource's lifecycle works through real Terraform |
| `make plan-quality` | Refresh-only plans find nothing, and plans stay within the unknown budget |
| `make walkthrough-test` | The hardware walkthrough's directories work as the page says |
| `make bench` | Plan, refresh and apply stay within the performance budget at 10 to 1,000 resources |

CI also runs `make docs-check`, `make doc-commands`, `make client-check`,
`make changelog-check`, `make shellcheck` and each script's own test.
[`.github/workflows/ci.yml`](.github/workflows/ci.yml) lists them all.
`make help` says what each target does.

None of these prove the real API behaves like the fake. The live tests do.

### `make check`

- Includes the state guard: `TestStateSchemaSnapshots` fails when a
  resource's stored state changes shape with no migration, and says what to
  add. The shapes are in `internal/provider/testdata/state-schemas`.
- With `terraform` on `PATH`, it also has the real CLI upgrade an old state
  and plan a `moved` block.

### `make testacc`

Real Terraform against the fake, one lifecycle per resource. Each case checks
its resources are gone after the destroy.

- Needs `terraform`. It doesn't run under `tofu`. `make demo` runs under both.

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
  `internal/acceptance/perpetual_diff_test.go` names the two attributes
  allowed to move.
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
  the worker count after the scale, and the version the upgrade pinned.
  Without the version, every plan moves the cluster back to the unversioned
  reference.
- Use the `acc-` prefix, so `make sweep` can clean up a killed run.

```sh
LIVE_KEEP=1 LIVE_CONFIRM=api.example.com LIVE_CLUSTER=acc-live-drift make live
export TF_CLI_CONFIG_FILE="$PWD/.live/terraform.rc"
cd examples/live
terraform apply -refresh-only -auto-approve -var-file=../../.live/kept.tfvars
terraform plan -detailed-exitcode -var-file=../../.live/kept.tfvars
terraform destroy -auto-approve -var-file=../../.live/kept.tfvars
```

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
