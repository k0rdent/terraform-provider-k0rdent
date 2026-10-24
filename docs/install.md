# Installing the provider

**Nothing is published yet.** Until the first release, [build from
source](build-from-source.md). [Distribution](distribution.md) has what's
left before a release.

## From the registry

```hcl
terraform {
  required_providers {
    k0rdent = {
      # Terraform reads this from registry.terraform.io, OpenTofu from
      # registry.opentofu.org.
      source  = "k0rdent/k0rdent"
      version = "~> 0.1"
    }
  }
}
```

Then run `terraform init`, or `tofu init`.

- No registry account or token is needed.
- `init` checks each download against the release's signed checksums and
  records them in `.terraform.lock.hcl`. Commit that file.
- `~> 0.1` never installs a release candidate such as `0.1.0-rc.1`. To try
  one, set `version = "0.1.0-rc.1"` exactly.
- The registry only says where each version is. The files themselves download
  from the provider's GitHub Releases, so a GitHub outage stops a fresh
  `init`. A directory that already ran `init` keeps working. If that would
  hurt, for example in CI, install from a mirror: `terraform providers mirror`
  into a directory or bucket you serve as a `filesystem_mirror` or
  `network_mirror`, as in [Air-gapped sites](#air-gapped-sites), or a registry
  proxy such as Artifactory. The signatures still check out from a mirror.

## Air-gapped sites

Build a mirror on a machine with internet access, copy it across, and point
Terraform at it on the isolated machine.

### On the connected machine

Write a configuration that names the provider, then mirror it for every
platform the isolated site runs. A platform you leave out can't be installed
there.

```bash
mkdir k0rdent-mirror && cd k0rdent-mirror
cat > providers.tf <<'EOF'
terraform {
  required_providers {
    k0rdent = {
      source  = "registry.terraform.io/k0rdent/k0rdent"
      version = "~> 0.1"
    }
  }
}
EOF

terraform providers mirror \
  -platform=linux_amd64 \
  -platform=linux_arm64 \
  -platform=darwin_arm64 \
  ./mirror
```

Verify the release before you carry it across, since nothing on the isolated
machine checks a signature. Download the release's `SHA256SUMS` and its
`.sig`, then:

```bash
curl -fsSLO https://raw.githubusercontent.com/k0rdent/terraform-provider-k0rdent/main/docs/signing-key.asc
gpg --import signing-key.asc
gpg --verify terraform-provider-k0rdent_0.1.0_SHA256SUMS.sig \
             terraform-provider-k0rdent_0.1.0_SHA256SUMS
sha256sum --check --ignore-missing terraform-provider-k0rdent_0.1.0_SHA256SUMS
```

From a checkout of this repository, `gpg --import docs/signing-key.asc` does
the same. Check the primary key fingerprint gpg prints is the one in
[SECURITY.md](../SECURITY.md#release-signatures).

Then, in the configuration the isolated site will apply, record the lock file
for the same platforms, and commit `.terraform.lock.hcl` with it:

```bash
terraform providers lock \
  -platform=linux_amd64 \
  -platform=linux_arm64 \
  -platform=darwin_arm64
```

### On the isolated machine

Copy the `mirror` directory to a fixed path. Then add this to the Terraform
CLI config, `~/.terraformrc` (`%APPDATA%\terraform.rc` on Windows):

```hcl
provider_installation {
  # Install k0rdent providers from the copied mirror.
  filesystem_mirror {
    path    = "/opt/terraform/providers"
    include = ["registry.terraform.io/k0rdent/*"]
  }
  # Never look for them online. Without this, a missing version means a
  # long network timeout instead of a clear error.
  direct {
    exclude = ["registry.terraform.io/k0rdent/*"]
  }
}
```

Run `terraform init` as usual. It refuses any package whose hash doesn't
match the committed lock file.

### Upgrading

On the connected machine, rerun `terraform providers mirror` with the new
version and `terraform providers lock`, then carry both across. Keep the old
version in the mirror so you can roll back.

## Not ways to install

- **Downloading an archive from a GitHub Release by hand.** It skips the
  signature check `init` does.
- **`dev_overrides`.** It's for provider developers, and skips the checksum
  check too.

## What a release contains

- Archives for `darwin_amd64`, `darwin_arm64`, `linux_amd64`, `linux_arm64`
  and `windows_amd64`
- `SHA256SUMS`, its GPG signature, and the signing key's fingerprint

Check a new version installs on a clean machine before relying on it in
automation.
