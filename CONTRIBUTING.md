# Contributing

- Issues are welcome.
- Pull requests from outside the maintainers aren't accepted yet. Open an
  issue instead, with a suggested fix if you have one.
- Security problems go through [SECURITY.md](SECURITY.md), never an issue.
- Everyone follows the [k0rdent Code of Conduct](CODE_OF_CONDUCT.md).

## Bug reports

Use the bug report form. Include:

- The provider version, and `terraform version` or `tofu version`
- The smallest configuration that shows the problem
- What you expected, and what happened
- The full error
- Whether the plan or the apply fails, and whether a plan right after a
  successful apply shows changes
- A debug log if you can (`TF_LOG=DEBUG terraform apply`), trimmed to the
  failing request

Remove secrets first: client secrets, tokens, kubeconfigs,
`sealed_client_secret` and webhook secrets.

## Feature requests

Use the feature request form. Say what you're trying to do, not only the
attribute you'd add. If the k0rdent API can't do it yet, say so.

## For maintainers

You need Go 1.26.10 or later and Terraform or OpenTofu. See
[TESTING.md](TESTING.md) and [RELEASE.md](RELEASE.md).

```sh
make doctor
make check
make testacc
make demo
```

A pull request:

- is approved by a maintainer other than its author
- has signed commits, and merges with a merge commit
- passes CI
- adds a `.changelog/<number>.txt` entry when users can see the change, or
  carries the `no-changelog-needed` label. Format:
  [.changelog/README.md](.changelog/README.md)
- runs `make docs` and commits the result when it changes a schema, an
  example or a template
