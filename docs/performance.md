# Performance and scale

How large a configuration can be, and how long its commands take.

## Supported limit

**1,000 managed resources in one configuration** (one root module, one
state).

At that size, with Terraform's default `-parallelism=10`:

| Command | Local platform (3ms a request) | Remote platform (50ms a request) |
| --- | ---: | ---: |
| `terraform plan` | 2s | 6s |
| `terraform plan -refresh-only` | 1s | 6s |
| `terraform apply`, creating all 1,000 | 26s | 49s |

Apply times leave out how long the platform takes to build each object. The
50ms column is a model, not a measurement of a real deployment.

- Plan and refresh read each resource about once, so their time grows in
  step with the resource count.
- Past 1,000, Terraform's own work grows faster than the count. Split the
  configuration instead.

## Larger estates

Split into several configurations, each with its own state:

1. **Shared networking:** networks, subnets, security groups and VPC
   peerings.
2. **One per cluster or instance group**, with its file stores and VPC
   security group bindings.
3. **Notifications:** webhook subscriptions.

A configuration reads what another manages through data sources
(`k0rdent_networks`, `k0rdent_security_groups`, `k0rdent_vpcs` and others),
or through `terraform_remote_state`.

- Don't use `-target` or `-refresh=false` to speed up routine plans. Both
  hide part of the estate from the plan.
- A higher `-parallelism` speeds up apply against a slow platform. At 50ms,
  apply of 1,000 went from 49s at 10 to 17s at 32. Past the platform's rate
  limit it stops helping, and the provider backs off when told to.

## Figures at `-parallelism=10`

From `make bench` against the local fake, 3ms a request:

| Resources | plan (s) | refresh (s) | apply (s) | destroy (s) |
| ---: | ---: | ---: | ---: | ---: |
| 10 | 0.14 | 0.13 | 1.50 | 0.86 |
| 100 | 0.30 | 0.22 | 2.63 | 2.08 |
| 500 | 0.99 | 0.60 | 12.72 | 10.60 |
| 1,000 | 1.77 | 1.05 | 26.35 | 25.88 |

- 3ms is the median request time of a local kind stack.
- Apply makes about 5 requests a resource against the fake. A real platform
  takes longer to settle each object, so it needs more reads, one per
  `poll_interval`.
- The provider's memory grows by about 0.1 MiB a resource during a plan,
  about 140 MiB at 1,000.

## Run it

```bash
make bench
make bench BENCH_SIZES=100,1000 BENCH_PARALLELISM=10 BENCH_LATENCY=50ms
```

For each size and parallelism it starts a fresh fake, writes a configuration
of that many resources, and runs apply, plan three times, refresh three times
and destroy. Any plan after the apply that isn't empty fails the run. Results
go to `.bench/results.json` and `.bench/results.md`.

The configuration is mostly networks, subnets, security groups and file
stores, with 10 clusters, 10 instance groups and 10 webhook subscriptions at
1,000. `internal/tools/bench/config.go` writes it.

## Nightly check

The nightly run fails when a figure at `-parallelism=10` breaks a limit in
`internal/tools/bench/budget.json`:

```bash
make bench BENCH_SIZES=100,500,1000 BENCH_PARALLELISM=10 BENCH_BUDGET=internal/tools/bench/budget.json
```

| Limit | Value | What breaking it means |
| --- | --- | --- |
| Requests a resource | plan and refresh 1.15, apply 5.0, destroy 4.75 | A command sends more requests per resource than before |
| Lists read | exact counts at each size, 21 in a plan of 1,000 | A lookup that lists every object for each object is back |
| Growth | plan and refresh time a resource, at the largest size over 100: 2.0 | The work no longer grows in step with the size |
| Seconds at 1,000 | plan 15, refresh 10, apply 120, destroy 150 | A large slowdown |

When a change moves a figure on purpose, rerun `make bench`, update the
tables here, and change the budget in the same pull request.

## Not covered

- **A real platform at this size.** No run at 1,000 has been made against a
  real deployment yet.
- **Timeouts at size.** Each timeout is tested on its own in unit tests.
- **Data sources.** The benchmark has none. A list data source reads every
  page of its collection on each plan.
