# Building the provider from source

Until the provider is published, build it once and tell Terraform where the
build is. For released versions, see [installing the provider](install.md).

You need Go 1.26 or newer and `git`.

## Build it

The build goes in a cache directory only you can write to, since whoever can
write it chooses the provider Terraform runs with your credential. If you set
`XDG_CACHE_HOME`, use it in place of `~/.cache` here and below.

```sh
git clone https://github.com/k0rdent/terraform-provider-k0rdent
cd terraform-provider-k0rdent
DEV=~/.cache/k0rdent-dev
mkdir -p $DEV
chmod 700 $DEV
go build -o $DEV/terraform-provider-k0rdent .
```

Then tell Terraform to use that build. Create `$DEV/terraform.rc`, for
example with `nano $DEV/terraform.rc`, using the path `echo $DEV` prints:

```hcl
provider_installation {
  # Use the local build instead of downloading the provider.
  dev_overrides {
    "k0rdent/k0rdent" = "/Users/you/.cache/k0rdent-dev"
  }
  # Every other provider installs as usual.
  direct {}
}
```

Point Terraform at that file. Put this line in your shell profile, unless
you use the walkthrough's `setup.sh`, which sets it for you:

```sh
export TF_CLI_CONFIG_FILE=~/.cache/k0rdent-dev/terraform.rc
```

For the hardware walkthrough, build into a run folder instead, as
[its step 1](walkthrough-hardware.md#1-make-a-run-folder) shows.

## Using it

- Every Terraform command warns that a development override is in effect.
  That's expected: it's your build.
- **Don't run `terraform init`** in a configuration that uses only this
  provider. There's nothing to install, and the error it gives is
  misleading. A configuration that also uses other providers does need
  `init` for those.

## Updating it

Pull, then rebuild. The override already points at the new binary.

```sh
cd terraform-provider-k0rdent
git pull
go build -o ~/.cache/k0rdent-dev/terraform-provider-k0rdent .
```

## When it goes wrong

| What you see | What it means |
| --- | --- |
| `Unable to open CLI configuration file`, then `Inconsistent dependency lock file` | `terraform.rc` is gone. Create it again, as above |
| `Failed to query available provider packages` | `TF_CLI_CONFIG_FILE` isn't exported in this shell |
