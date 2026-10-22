# Distribution

The provider will be published as `k0rdent/k0rdent` on the public
[Terraform Registry](https://registry.terraform.io) and
[OpenTofu registry](https://search.opentofu.org). There's no private
registry or mirror.

**Nothing is published yet.** There's no tag, release or registry listing.

## What's left, in order

| # | Step | Side | Needs |
| --- | --- | --- | --- |
| 1 | Clean up the repository, history included, and make it public | Provider, then Platform | Nothing |
| 2 | Put an organization-owned RSA GPG key in the `release-signing` environment | Platform | Nothing |
| 3 | Publish on registry.terraform.io under the `k0rdent` namespace, with the key's public half | Platform | 1 and 2 |
| 4 | Submit the provider and key to the [OpenTofu registry](https://github.com/opentofu/registry) | Provider | 1 and 2 |
| 5 | Cut the first tag, and attach the signed files to a GitHub Release | Provider | 1 and 2 |

[RELEASE.md](../RELEASE.md) has the steps for a release.

## What a release contains

- Archives for `darwin_amd64`, `darwin_arm64`, `linux_amd64`, `linux_arm64`
  and `windows_amd64`
- `SHA256SUMS`, its GPG signature, and the registry manifest
- A signed SPDX SBOM per archive
- Release notes from `.changelog/`

Only the `release-signing` workflow on protected `main` signs a release. See
[release signing](release-signing.md).

## Source

The API client is generated into this repository, in `internal/k0rdent`, so
[building from source](build-from-source.md) needs nothing private.

## Installing

[Installing the provider](install.md) covers the registry and air-gapped
sites.
