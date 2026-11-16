# Support matrix

What this provider runs with, and what's not decided yet.

## Terraform and OpenTofu

| Provider | Terraform | OpenTofu |
| --- | --- | --- |
| 0.x | 1.16 and 1.17, not 1.16.0 to 1.16.4 | 1.12 and 1.13 |

- CI runs the acceptance suite on the newest patch of each of these minor
  versions, the ones in the matrices in `.github/workflows/ci.yml`. Older
  versions aren't tested.
- Terraform 1.16.0 to 1.16.4 can empty the state file when an apply is
  interrupted. See [known limitations](known-limitations.md#terraform-1160-to-1164).
- The nightly run tests Terraform and OpenTofu on the same configuration.

Some features need a minimum Terraform version, from its changelog:

| Feature | Used by | Terraform |
| --- | --- | --- |
| Ephemeral resources | the cluster kubeconfig | [1.10](https://github.com/hashicorp/terraform/blob/v1.10/CHANGELOG.md) |
| Write-only attributes | `ssh_public_keys_wo`, `secret_wo` | [1.11](https://github.com/hashicorp/terraform/blob/v1.11/CHANGELOG.md) |
| `import` blocks with `identity` | importing any resource by its parts | [1.12](https://github.com/hashicorp/terraform/blob/v1.12/CHANGELOG.md) |
| Actions | `k0rdent_cluster_cancel_operation`, `k0rdent_instance_power` | [1.14](https://github.com/hashicorp/terraform/blob/v1.14/CHANGELOG.md) |
| List resources and `terraform query` | finding resources to import | [1.14](https://github.com/hashicorp/terraform/blob/v1.14/CHANGELOG.md) |

- OpenTofu has ephemeral resources and write-only attributes from
  [1.11](https://github.com/opentofu/opentofu/blob/v1.11/CHANGELOG.md).
- OpenTofu has no actions and no list resources.

## The k0rdent platform

| Provider | Platform | Status |
| --- | --- | --- |
| 0.x | Not decided | **Open** |

- The provider is built against API contract **v1.3.0-rc.2**. The client is
  generated from it into `internal/k0rdent` and committed, so every build uses
  the same client.
- CI checks that the fake the tests run against was last compared with the
  real API at the version in `ci/real-api/api-version`.
- The contract version and the provider version change separately.

Still to be decided:

1. Which platform versions each provider version supports.
2. How long a provider version stays supported after the next one ships.
3. Whether support is stated against platform releases or contract versions.

Until then, releases aren't tested against a list of platform versions, and
platform release notes can't say which provider version they need.

## Version mismatches

The provider can't detect them. The API has no version endpoint and no
version header. Against a platform it doesn't support, the provider fails at
whichever call changed, with that call's error.

## Platforms the binary is built for

| Operating system | amd64 | arm64 |
| --- | --- | --- |
| Linux | yes | yes |
| macOS | yes | yes |
| Windows | yes | no |

Each is a single static binary with no other dependencies.
