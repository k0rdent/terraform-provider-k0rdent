# Support matrix

What this provider runs with, and what's not decided yet.

## Terraform and OpenTofu

| Provider | Terraform | OpenTofu |
| --- | --- | --- |
| 0.x | 1.10 and later | Tested nightly |

- 1.10 is the minimum because the cluster kubeconfig is an ephemeral
  resource, which older versions don't support.
- CI tests a current Terraform release, not 1.10 itself.
- The nightly run tests Terraform and OpenTofu on the same configuration.

## The k0rdent platform

| Provider | Platform | Status |
| --- | --- | --- |
| 0.x | Not decided | **Open** |

- The provider is built against API contract **v1.3.0-rc.2**. The client is
  generated from it into `internal/k0rdent` and committed, so every build uses
  the same client.
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
