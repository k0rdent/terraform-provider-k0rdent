# Terraform provider for k0rdent AI

Manage k0rdent AI infrastructure with Terraform or OpenTofu: clusters,
instance groups, networks, subnets, security groups, VPC peerings, file
stores, projects and webhooks.

- Registry address: `k0rdent/k0rdent`
- Needs Terraform 1.10 or later, or OpenTofu
- Reference docs: [`docs/`](docs/index.md), the same pages the registry shows

## Install

```hcl
terraform {
  required_providers {
    k0rdent = {
      source  = "k0rdent/k0rdent"
      version = "~> 0.1"
    }
  }
}
```

```sh
terraform init
```

- `tofu init` works the same way.
- Air-gapped sites: [docs/install.md](docs/install.md).
- Until the first release is out, [build from source](#build-from-source).

## Authenticate

The credential comes from the environment only, so it never lands in a `.tf`
file, a plan or state.

`read -rs` reads the secret without echoing it, so it stays out of your shell
history:

```sh
export K0RDENT_CLIENT_ID='sa-terraform@example.iam'
printf 'client secret: ' >&2
read -rs K0RDENT_CLIENT_SECRET && export K0RDENT_CLIENT_SECRET && echo >&2
```

| Variable | Provider argument | What it is |
| --- | --- | --- |
| `K0RDENT_CLIENT_ID`, `K0RDENT_CLIENT_SECRET` | none | A service account's credentials. Use these |
| `K0RDENT_API_KEY` | none | An API key. Works, with a warning |
| `K0RDENT_ENDPOINT` | `endpoint` | API URL, scheme and host only, no `/v1`. Default `https://api.k0rdent.ai` |
| `K0RDENT_REGION` | `region` | Region id. List them with `k0rdent_regions` |
| `K0RDENT_PROJECT` | `project` | Project id |
| `K0RDENT_OPERATION_TIMEOUT` | `operation_timeout` | How long to wait for a change to finish |
| `K0RDENT_POLL_INTERVAL` | `poll_interval` | How often to check while waiting |

- An argument wins over its variable. An empty variable counts as unset.
- Setting both a service account and an API key is an error.
- A tenant administrator creates the service account. See the
  [authentication guide](docs/guides/authentication.md).

## Example

```hcl
provider "k0rdent" {
  region  = "us-texas-1"
  project = "my-project"
}

data "k0rdent_regions" "all" {}

data "k0rdent_cluster_types" "available" {
  status = "available"
}

output "region_ids" {
  value = data.k0rdent_regions.all.ids
}

# Each cluster type's id, mapped to the reference a cluster's cluster_type takes.
output "cluster_types" {
  value = { for t in data.k0rdent_cluster_types.available.cluster_types : t.id => t.self_link }
}
```

## Guides

| Guide | What it covers |
| --- | --- |
| [Getting started](docs/guides/getting-started.md) | From a credential to a cluster you can reach |
| [Authentication](docs/guides/authentication.md) | Service accounts, API keys, rotation |
| [Import](docs/guides/import.md) | Adopting existing objects |
| [Remote state](docs/guides/remote-state.md) | Running from a remote backend in a pipeline |
| [Hardcoded values](docs/guides/hardcoded-values.md) | Values you still have to write as literals |

Also:

- [`examples/`](examples): a configuration for each resource and data source
- [Walkthrough](docs/walkthrough.md): the whole lifecycle on your laptop
- [Known limitations](docs/known-limitations.md)
- [Support matrix](docs/support-matrix.md)
- [Compatibility and upgrades](docs/public-compatibility.md)

## Try it without an account

`make demo` builds the provider and runs apply, re-plan, scale, import and
destroy against a local fake API. No credential needed.

```sh
git clone https://github.com/k0rdent/terraform-provider-k0rdent
cd terraform-provider-k0rdent
make demo
```

`make demo-up` leaves the fake running so you can edit
`examples/demo/main.tf`. `make demo-down` stops it.

## Build from source

Needs Go 1.26 or later.

```sh
git clone https://github.com/k0rdent/terraform-provider-k0rdent
cd terraform-provider-k0rdent
make build
```

`make build` writes `terraform-provider-k0rdent` in the clone. Point
Terraform at that directory in a CLI config file, and don't run
`terraform init`:

```hcl
provider_installation {
  # Use the local build instead of downloading the provider.
  dev_overrides {
    "registry.terraform.io/k0rdent/k0rdent" = "/path/to/terraform-provider-k0rdent"
  }
  # Every other provider installs as usual.
  direct {}
}
```

Full steps: [docs/build-from-source.md](docs/build-from-source.md).

## Develop and test

| Command | What it does |
| --- | --- |
| `make check` | Format, vet, lint and unit tests |
| `make testacc` | Each resource's lifecycle through real Terraform, against the fake |
| `make demo` | The whole lifecycle against the fake |
| `make help` | Every target |

See [TESTING.md](TESTING.md) and [RELEASE.md](RELEASE.md).

## Support

- Bugs and feature requests: [GitHub issues](https://github.com/k0rdent/terraform-provider-k0rdent/issues)
- Security problems: report privately, as [SECURITY.md](SECURITY.md) says.
  Don't open an issue.
- Contributing: [CONTRIBUTING.md](CONTRIBUTING.md)

## License

Apache-2.0. See [LICENSE](LICENSE).
