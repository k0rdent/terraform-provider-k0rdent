# Walkthrough on real hardware

Takes you from nothing to a Kubernetes cluster on a real k0rdent platform and
back, with plain Terraform in a directory of your own.

- To try the provider with no platform, see [the laptop walkthrough](walkthrough.md).
- To check the platform itself first, with only `curl`, see
  [Checking the API with curl](walkthrough-api.md).

Every command is one short line, so you can paste them one at a time.

**In a hurry?** [`walkthrough.sh`](../examples/walkthrough/walkthrough.sh)
runs the same commands, printing each one first. From your clone:

```sh
cd terraform-provider-k0rdent
examples/walkthrough/walkthrough.sh setup
```
It makes a new run folder and prints its name. Go to it, then:

```sh
cd ~/k0rdent-runs/2026-10-16-142233-origin-main/walkthrough
. ./setup.sh
./walkthrough.sh up look
```
Fill in `cluster/terraform.tfvars` from what `look` printed, then run
`./walkthrough.sh up cluster`, and `./walkthrough.sh down cluster` when
you're done. Each step below says which command does it.

## What you need

Ask whoever runs the platform for:

| What | Looks like |
| --- | --- |
| The API URL, scheme and host only, no `/v1` | `https://api.example.k0rdent.ai` |
| A service account's client id and secret | `sa-...`, and a secret shown once |
| An organization, or a project in one | `org-acme`, `proj-a1b2c3d4` |
| A published cluster type with hardware behind it | `gpu-training` |

- You don't need the region, organization or project ids up front. Step 4
  lists them.
- **Ask for a service account scoped to the organization.** It can run every
  step, including the ones that create a project.
- A service account scoped to a project, with `compute-admin` on it, runs
  `look/`, `cluster/`, `nodes/` and `network/` in that project. It runs
  `full/` with `quota_checks = false`, because reading the organization and
  its quota needs an organizations role on the organization. It can't run
  `cluster-own-project/` or `full/`'s `own_project`, which create a project.
- An API key also works, but prefer a service account. `setup.sh` only asks
  for a client id and secret, so with a key, export `K0RDENT_ENDPOINT`,
  `K0RDENT_API_KEY` and `TF_CLI_CONFIG_FILE` yourself.

You also need Go, and `terraform` or `tofu` at a version in the
[support matrix](support-matrix.md).

## 1. Make a run folder

Each run gets a new folder with its own checkout of the commit you're
testing, the provider built from it, a copy of the walkthrough, and its own
Terraform state. `walkthrough.sh setup` does this whole step. Give it a branch
to test that instead: `walkthrough.sh setup my-branch`.

By hand, clone the repository once:

```sh
URL=https://github.com/k0rdent/terraform-provider-k0rdent
git clone $URL
```
Then for each run, name a new folder and build into it. Use a branch or
commit in place of `origin/main` to test that instead. `chmod 700` makes the
folder yours alone, since whoever can write `bin/` chooses the provider
Terraform runs with your credential.

```sh
cd terraform-provider-k0rdent
git fetch origin
RUN=~/k0rdent-runs/2026-10-16-main
mkdir -p $RUN/bin
chmod 700 $RUN
git worktree add --detach $RUN/provider origin/main
cd $RUN/provider
go build -o $RUN/bin/terraform-provider-k0rdent .
cp -R examples/walkthrough $RUN/walkthrough
```
Last, create `$RUN/terraform.rc` in an editor, for example
`nano $RUN/terraform.rc`, using the path `echo $RUN/bin` prints:

```hcl
provider_installation {
  # Use the run folder's build instead of downloading the provider.
  dev_overrides {
    "k0rdent/k0rdent" = "/Users/you/k0rdent-runs/2026-10-16-main/bin"
  }
  # Every other provider installs as usual.
  direct {}
}
```
See [Build it](build-from-source.md#build-it) for more on the build and the
override.

## 2. Go to the walkthrough

```sh
cd $RUN/walkthrough
```
The walkthrough is a few small Terraform directories, from
[`examples/walkthrough`](../examples/walkthrough), and two scripts. You only
edit the `terraform.tfvars` in each directory.

To reuse your values in every run, keep filled-in copies in
`~/.config/k0rdent-walkthrough`, named for the directory, such as
`cluster.tfvars`. `walkthrough.sh setup` copies them in. By hand:

```sh
CFG=~/.config/k0rdent-walkthrough
cp $CFG/cluster.tfvars cluster/terraform.tfvars
```
| What | What it's for |
| --- | --- |
| [`setup.sh`](../examples/walkthrough/setup.sh) | Sets the endpoint and credential in your shell |
| [`walkthrough.sh`](../examples/walkthrough/walkthrough.sh) | The quick path: `setup`, `up`, `check` and `down` |
| [`look/`](../examples/walkthrough/look/main.tf) | Lists what the credential can see and build. Creates nothing |
| [`cluster/`](../examples/walkthrough/cluster/main.tf) | A cluster in a project you already have |
| [`cluster-own-project/`](../examples/walkthrough/cluster-own-project/main.tf) | A cluster in a new project, deleted with it |
| [`nodes/`](../examples/walkthrough/nodes/main.tf) | Lists a cluster's nodes with an `ssh` command for each. Creates nothing |
| [`network/`](../examples/walkthrough/network/main.tf) | A network, a subnet and a security group. No cluster |
| [`full/`](../examples/walkthrough/full/main.tf) | Everything a tenant can build, in one apply, with switches |

## 3. Set the endpoint and credential

The leading `.` runs the script in your current shell, so its exports stay
set:

```sh
. ./setup.sh
```
- It asks for the endpoint, client id and secret. The secret isn't shown or
  saved in your history. Press Enter to keep a value already set.
- It points Terraform at this run folder's build and prints which build that
  is.
- It clears settings left over from another platform.
- Run it again in each new shell. `walkthrough.sh` can't do it for you, since
  it has to change your own shell.

## 4. Look around

```sh
cd look
terraform apply
```
It only reads, so it reports `0 added, 0 changed, 0 destroyed`, then lists
regions, organizations, projects, and the cluster types in each region.
`./walkthrough.sh up look` does the same.

If it fails here, it's the endpoint or the credential:

| What you see | What it means |
| --- | --- |
| A 401 | Wrong client id or secret |
| A 429 | Too many token requests. Wait a minute |
| A connection error | Wrong URL, or you need the VPN |

Note a region, a cluster type listed under it, and a project or organization.
An empty list of cluster types means the region has nothing to build from
yet. Ask the platform's operator.

## 5. Build a cluster

Pick one of the two cluster directories:

- **[`cluster/`](../examples/walkthrough/cluster/main.tf)** builds in a project
  you already have. A wrong project id stops the plan before anything is
  created.
- **[`cluster-own-project/`](../examples/walkthrough/cluster-own-project/main.tf)**
  creates a project, builds in it, and deletes it last. Needs a credential
  scoped to the organization.

```sh
cd ../cluster
```
Or `cd ../cluster-own-project`. Fill in `terraform.tfvars` from what `look`
printed. The plan refuses until every value is set. In `cluster/`:

```hcl
# Starts every name the walkthrough makes. Use your own, such as your
# username, so two people on one platform don't collide.
prefix       = "jdoe"
region       = "global"
project      = "proj-a1b2c3d4"
cluster_type = "gpu-training"

# Your public key, for logging in to nodes as `ops`. Set it before the first
# apply: a different key later replaces the cluster.
ssh_public_key_file = "~/.ssh/id_ed25519.pub"
```
`cluster-own-project/` takes `organization` instead of `project`, and no key.

Plan, then apply:

```sh
terraform plan
terraform apply
```
- The plan should create one cluster (and one project in
  `cluster-own-project/`), and nothing else.
- The apply waits until the cluster is ready, which on real hardware can take
  more than 2 hours. `main.tf` allows 3.
- **Let it finish.** Stopping it, or destroying while it's creating, cancels
  the create, and the platform has to tear down what it built.
- To watch progress, run the apply with `TF_LOG_PROVIDER=INFO`. That log never
  holds the credential.

`./walkthrough.sh up cluster` does steps 5 and 6.

## 6. Plan again

`echo $?` prints the plan's exit code:

```sh
terraform plan -detailed-exitcode
echo $?
```
**Exit code 0 is the result you want.** 2 means the provider wants to change
something it just created, which is a bug.

In the `cluster` output, `written` is the reference you gave and `bound` is
the version the platform pinned. They differ, and the plan is still empty.

## 7. Log in to a node

The platform doesn't report node addresses, so
[`nodes/`](../examples/walkthrough/nodes/main.tf) asks the cluster. It reads
the kubeconfig as an ephemeral resource, so it's never saved, and prints an
`ssh` command for each node.

```sh
cd ../nodes
```
Fill in its `terraform.tfvars` with the same `prefix`, `region` and `project`
as the cluster. This directory also uses the `kubernetes` provider, so it
needs `terraform init` once. `init` skips `k0rdent`, which is still your
build. `./walkthrough.sh up nodes` runs both.

```sh
terraform init
terraform apply
```
The apply prints something like:

```text
ssh = {
  "node-a" = "ssh ops@10.0.0.11"
}
```
Run one, adding `-i` and your private key if it isn't your default. Your
machine has to reach the cluster's API server and the node. If either times
out, ask whoever runs the platform which address to use, and whether you need
their VPN.

## 8. Change it

```sh
cd ../cluster
```
In `main.tf`, change the cluster's `display_name`, for example to
`"${var.prefix} walkthrough, changed"`, then:

```sh
terraform plan
terraform apply
```
The plan shows `1 to change, 0 to destroy`: an update in place. Plan again
afterwards, and it's empty.

## 9. Destroy

Destroy from the run folder that created the cluster, with its `setup.sh`
sourced, so it uses the same build. `nodes/` creates nothing, so skip it.

```sh
cd ../cluster
terraform destroy
```
In `cluster-own-project/`, the project goes last. `./walkthrough.sh down
cluster` does the same.

When everything is destroyed, delete the run folder, then run
`git worktree prune` in your clone.

## 10. More to try

Both directories run the same way: fill in `terraform.tfvars`, then `plan`,
`apply`, `plan -detailed-exitcode` and `destroy`. `walkthrough.sh up` and
`down` work for both.

### Networking on its own

[`network/`](../examples/walkthrough/network/main.tf) makes a network, a
subnet, and a security group that lets in SSH, the Kubernetes API and ping
from `allowed_cidr`, the range you connect from, and lets out what nodes
need. No cluster, so it takes seconds.

- It takes `prefix`, `region`, `project` and `allowed_cidr`.
- The subnet is a /28 from the region's first active address pool that isn't
  routable. A region without one stops the plan and says so.
- The outputs show the VLAN, the subnet's CIDR and the group's rules.

### Everything at once

[`full/`](../examples/walkthrough/full/main.tf) builds everything a tenant can
in one apply. Inputs are in `variables.tf`, resources in `main.tf`, and
results in `outputs.tf`.

By default it makes:

- a network with a /28 subnet each for `nodes` and `storage`
- a security group with a rule for each service and each range in
  `allowed_cidrs`
- a cluster behind that group, with your users on every node
- the group on the cluster's VPC, before the VPC's default group
- a webhook

It takes what `cluster/` takes, plus:

- `allowed_cidrs`, the ranges you connect from, such as
  `["203.0.113.0/24"]`. `0.0.0.0/0` is refused.
- `webhook_url`, where cluster events go.
- The webhook's signing secret, from your shell, never a file. Every command
  needs it while `webhook` is on, `destroy` included, so set it once for the
  session. This makes a random one:

```sh
export TF_VAR_webhook_secret=$(openssl rand -hex 32)
```
The other inputs have defaults, listed commented out in `terraform.tfvars`.
With `environment = "prod"`, a destroy waits for a running operation instead
of cancelling it.

The `build` block in `terraform.tfvars` turns each part on or off:

| Switch | Default | What it makes | What it needs |
| --- | --- | --- | --- |
| `network` | on | A network and a subnet per purpose | An address pool that isn't routable |
| `security_group` | on | A group on the cluster | A cluster type whose nodes sit on a `nico` VPC |
| `vpc_security_groups` | on | The same group on every VPC this makes, before the VPC's default group | `security_group` |
| `webhook` | on | A subscription to cluster events | `webhook_url` and the secret |
| `instance_group` | off | One bare host, with your users on it | A free host |
| `file_store` | off | 100 GiB of shared storage the host can mount | A share type in the region |
| `vpc_peering` | off | The cluster's VPC peered with the host's | `instance_group` |
| `own_project` | off | A new project, deleted last | `organization`, and a credential scoped to it |

- `terraform output scorecard` shows a line for each: what it made, or the
  switch that turns it on. It also shows what the platform reports back, such
  as quota, the kubeconfig and webhook deliveries.
- `nodes/` works with this cluster too, since it's also
  `<prefix>-walkthrough`.
- On the `nico` fabric, traffic that no rule permits is denied, in both
  directions. Rules the operator sets for the site are checked first, so a
  site permit or deny wins over the group. The `kubernetes_api` output gives
  an `nc` test to run from inside and outside `allowed_cidrs`.
- From API v1.3.0-rc.3 a VPC's default group lets nothing out of the VPC.
  So the groups in `full/` and `network/` list what nodes reach out to: DNS, NTP,
  HTTP, HTTPS, the Kubernetes API on 6443 and konnectivity on 8132, and NFS
  in `full/`. Each is a `tcp` or `udp` rule with a destination port and no
  source port, the only shape whose replies the `nico` fabric lets back in.
  A cluster type can use other ports for those two, such as 30443 and 30132
  for a `NodePort` control plane. Add those, or anything else your nodes
  reach, to `egress_out` in `main.tf`.
- Ingress rules are stateless, so each service you let in also gets an
  egress rule for its replies, from its own port.
- With `vpc_security_groups` on, each VPC gets this group and then the
  VPC's default group, which permits traffic between the VPC's subnets.
  This group comes first, so its denies also apply between nodes. That's
  why `full/` adds a deny for each service from anywhere else, except the
  Kubernetes API port. A list
  without the default would replace it. The default's id is random, so
  `full/` finds it by the name the platform gives it, `Default (<VPC id>)`,
  and stops with an error if it can't.
- A security group change can take a few minutes to reach the nodes.

## When something goes wrong

| What you see | What to do |
| --- | --- |
| 401 | Check the client id and secret |
| `Unauthorized` from `nodes/` | The cluster refused its kubeconfig. Report it to the platform operator with the cluster id and the time |
| 404 that reads like a missing project | Copy the region and project ids from `look` |
| 429 | Too many token requests. Wait a minute between commands |
| `No such project` at plan | This credential can't see that project |
| 403 or 409 creating the project | The credential is project-scoped, or the project already exists. Change `prefix`, or use `cluster/` |
| 403 reading the organization or quota in `full/` | The credential is project-scoped. Set `quota_checks = false` |
| `The cluster did not become active` | The create timed out, but the cluster is still building. Don't apply again, which would replace it. Wait until it's `active`, run `terraform untaint k0rdent_kubernetes_cluster.walkthrough`, then go to step 6 |
| `The cluster did not finish deleting`, with `timed out` | Freeing hardware can take hours. Wait until the cluster is gone from the UI, then run `terraform destroy` again |
| `The cluster did not finish deleting`, with `failed` | Try the delete once [with curl](walkthrough-api.md#6-delete-it). If that fails too, send the platform operator the execution id from the error |
| A 422 about the node count's range | The cluster type's pool has a minimum or maximum. Check the cluster type |
| `Unable to open CLI configuration file`, `Inconsistent dependency lock file`, or a registry lookup for `k0rdent/k0rdent` | Source `setup.sh` again. If you ran `terraform init` in a k0rdent-only directory, delete `.terraform.lock.hcl` |

## Next

- Each resource's page under [`docs/resources/`](resources/) has its full
  reference and an example.
- `examples/live` covers more: networks, subnets, security groups, a
  kubeconfig, and a scorecard.
- Maintainers: `make live-preflight` and `make live` run that configuration
  end to end. See `TESTING.md`.
