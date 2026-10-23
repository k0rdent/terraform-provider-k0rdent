# Release signing

Signed releases are built only by a workflow that runs from protected `main`
and waits for a second maintainer. This page lists the controls and what each
one stops. [RELEASE.md](../RELEASE.md) has the steps and the commands that
check each setting. Publishing to the registries is in
[distribution](distribution.md).

## Repository settings

| Control | What it stops |
| --- | --- |
| `main` needs a reviewed pull request and passing checks, and refuses force pushes | Unreviewed code reaching a release. The workflow only checks the tag is on `main`, so `main` has to be protected |
| `release-v*` branches have the same rules, and refuse deletion | Unreviewed code reaching a candidate or a patch. The workflow refuses a release branch without them, and also checks the branch left `main` at one of its commits and holds only merged pull requests since, because rules don't cover the push that creates a branch |
| A `release-signing` environment with required reviewers, no self-review and no admin bypass | One person signing a release alone |
| The environment deploys from protected branches only | A signing job started from another branch |
| A ruleset on `v*` tags that refuses delete and update | A released tag being moved to other code |
| The three GPG secrets are `release-signing` environment secrets, and nowhere else | Any other job reading the signing key |
| The old tag-triggered `release.yml` workflow is disabled | A tag on an old commit running that commit's old release workflow |

The secrets:

- `TERRAFORM_PROVIDER_GPG_PRIVATE_KEY`
- `TERRAFORM_PROVIDER_GPG_KEY_FINGERPRINT`
- `TERRAFORM_PROVIDER_GPG_PASSPHRASE`

## Workflow controls

`scripts/check-workflow-actions.sh` refuses a change that undoes any of these,
and `make workflow-actions-test` proves it still does. CI runs it.

- **Actions are pinned to a full commit**, with the release in a `# vX.Y.Z`
  comment. A tag like `@v4` can be moved by its owner.
- **Every checkout sets `persist-credentials: false`**, so later steps can't
  read the token from `.git/config`. The one exception is the tag check's
  checkout, which needs the token to fetch while the repository isn't public.
  That job holds no other secret.
- **The signing job deletes the keyring** in an `if: always()` step before
  uploading anything.
- **The signing check holds to the same rules.** It's dispatched by hand from
  `main`, runs in `release-signing` and deletes the keyring and passphrase
  file in `if: always()` steps. It's the only other job that gets the key.

## What a dispatch does

1. A maintainer pushes a release tag on `main`, or on the version's release
   branch. That alone signs nothing.
2. A maintainer runs **Dispatch signed provider release** from `main`, with
   the tag as input.
3. A job with no secrets checks the tag is a valid version on `main`'s own
   history, or on a protected `release-vMAJOR.MINOR.x` branch for its version, and
   records its commit. For a release branch it also checks the branch's rules,
   that the branch left `main` at a commit of `main`'s own history, and that
   every commit on it since is the merge of a pull request into it, approved
   by someone other than its author. It
   refuses a new major until the dispatch sets `allow_major`, counting only
   releases this workflow published. A tag that was never released doesn't
   count.
4. A required reviewer approves. Only then does the signing job get the key,
   check out that commit and build.
5. The job checks the five archives carry the tag's version, that
   `SHA256SUMS` covers exactly those archives, their SBOMs and the registry
   manifest, and that its signature verifies. Then it uploads them as a workflow
   artifact.

## Check the key

The public key is [`signing-key.asc`](signing-key.asc), and its fingerprint is
in [SECURITY.md](../SECURITY.md#release-signatures).

Run **Check release signing** from `main`:

- after any change to the key or its secrets, such as a new subkey
- before the first release on a new subkey

A required reviewer approves it, as for a release. It signs a test file with
the secrets, checks the signature against `signing-key.asc`, and fails if:

- the fingerprint secret doesn't name the committed key
- the secret holds the parent's secret part, not just the signing subkey
- the signature doesn't verify against the committed key alone
- GitHub signs with a subkey that isn't the newest one committed, or isn't
  committed at all

`scripts/check-signing-key.sh` does the checking, and
`make signing-key-check-test` proves each refusal against throwaway keys.

The nightly reads `signing-key.asc` with no secret. It warns once the signing
subkey is a year old or the parent five, which is when they rotate. It fails
when either has under 90 days left, which turns the nightly red. Run it with
`make signing-key-check`. While either is due, the nightly keeps one
`signing-key-rotation` issue open with the dates and where the how-to is, and
closes it once nothing is due.

To rotate: add a new signing subkey, put it in
`TERRAFORM_PROVIDER_GPG_PRIVATE_KEY`, commit the new public key here, upload
it to both registries, then run the check. Don't revoke the old subkey unless
it leaked: releases it signed stop verifying.

## Tags

`vMAJOR.MINOR.PATCH`, or a candidate such as `v0.1.0-rc.1`. Build metadata
(`+...`) is refused, so each tag names one build.

## The API client

The release builds the client generated in `internal/k0rdent`, from the
pinned API contract in the [support matrix](support-matrix.md). It's
committed, so rebuilding a tag uses the same client. To upgrade it:

1. Regenerate it with `make client-sync`, as `internal/k0rdent/README.md`
   says.
2. Run the tests, `make docs-check` and `make release-validate`.
3. Merge through `main` before tagging.
