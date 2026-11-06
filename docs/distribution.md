# Distribution

The provider is published as `k0rdent/k0rdent` on the public
[Terraform Registry](https://registry.terraform.io/providers/k0rdent/k0rdent)
and [OpenTofu registry](https://search.opentofu.org/provider/k0rdent/k0rdent).
There's no private registry or mirror.

Each version is a signed
[GitHub Release](https://github.com/k0rdent/terraform-provider-k0rdent/releases)
of this repository, and both registries read it from there. 0.1.0 is the first
final version. The candidates before it, `0.1.0-rc.1` to `0.1.0-rc.3`, are
published too, and only an exact `version` installs one.

To see what each registry holds:

```sh
curl -s https://registry.terraform.io/v1/providers/k0rdent/k0rdent/versions | jq -r '.versions[].version'
curl -s https://registry.opentofu.org/v1/providers/k0rdent/k0rdent/versions | jq -r '.versions[].version'
```

[RELEASE.md](../RELEASE.md) has the steps for a release.

## What a release contains

- Archives for `darwin_amd64`, `darwin_arm64`, `linux_amd64`, `linux_arm64`
  and `windows_amd64`
- `SHA256SUMS`, its GPG signature, and the registry manifest
- A signed SPDX SBOM per archive

The release's notes say which k0rdent API version it was tested against, the
`version` that installs it, upgrade notes and known issues. Each version's
changes are in [CHANGELOG.md](../CHANGELOG.md), rendered from `.changelog/`.

Only the `release-signing` workflow on protected `main` signs a release. See
[release signing](release-signing.md).

## Source

The API client is generated into this repository, in `internal/k0rdent`, so
[building from source](build-from-source.md) needs nothing private.

## Installing

[Installing the provider](install.md) covers the registry and air-gapped
sites.
