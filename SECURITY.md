# Security policy

## Report a vulnerability

Report it privately on GitHub:
[report a vulnerability](https://github.com/k0rdent/terraform-provider-k0rdent/security/advisories/new).
Don't open a public issue, pull request or discussion.

Include what you can:

- The provider version, and the Terraform or OpenTofu version
- What an attacker can do, and what they need first
- Steps or a configuration that reproduce it
- Whether a secret can reach state, a plan, a log or the console

Leave real credentials, tokens and kubeconfigs out. Describe them instead.

## What happens next

- A maintainer acknowledges the report and works on it in a private advisory,
  which you're invited to.
- The fix ships in a new release, and the advisory is published with it.
- You're credited unless you ask not to be.

## Release signatures

Every release's `SHA256SUMS` is signed with this key:

- Fingerprint `B217 83BF D3FD 4F36 5F33  DE67 F36C A5FC E570 B234`
- Public key: [`docs/signing-key.asc`](docs/signing-key.asc)

To check a release you downloaded:

```sh
gpg --import docs/signing-key.asc
gpg --verify terraform-provider-k0rdent_0.1.0_SHA256SUMS.sig \
             terraform-provider-k0rdent_0.1.0_SHA256SUMS
sha256sum --check --ignore-missing terraform-provider-k0rdent_0.1.0_SHA256SUMS
```

gpg names a signing subkey, then the primary key fingerprint, which has to be
the one above. The subkey changes every year, and the primary key doesn't.

A signature that doesn't verify, or a release signed by any other key, is a
vulnerability. Report it as above.

## Supported versions

Fixes go into the latest release. There's no long-term support branch while
the provider is 0.x.

## Scope

- This provider: its code, its releases and their signatures
- Anything that writes a credential, token or kubeconfig into state, a plan,
  a log or an error message

A problem in the k0rdent platform itself belongs with the platform. If you're
not sure which it is, report it here.
