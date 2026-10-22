# Releasing

For maintainers: how to cut a signed release, and the repository settings it
needs. [docs/release-signing.md](docs/release-signing.md) says what each
control protects.

The flow:

1. Push a signed tag on `main`.
2. Dispatch `release-dispatch.yml` from `main`.
3. A job with no secrets checks the tag.
4. A second maintainer approves the `release-signing` environment.
5. The signing job builds, signs and checks the release, and uploads it as a
   workflow artifact.
6. You attach the files to a GitHub Release, where the Terraform and OpenTofu
   registries read them. This step is by hand for now
   (`.goreleaser.yaml` has `release: disable: true`).

The commands below use this shorthand for the repository's API path:

```sh
R=repos/k0rdent/terraform-provider-k0rdent
```

## Repository settings

Each needs a repository admin. Check them before a release.

| Setting | Where | Check | Want |
| --- | --- | --- | --- |
| Signing key in environment secrets only | Settings, Environments, `release-signing` | `gh api $R/environments/release-signing/secrets --jq '[.secrets[].name] \| sort'` | The three names below |
| Not at repository scope | Settings, Secrets and variables, Actions | `gh api $R/actions/secrets --jq '[.secrets[].name \| select(startswith("TERRAFORM_PROVIDER_GPG"))]'` | `[]` |
| Not at organization scope | Organization settings | `gh api $R/actions/organization-secrets --jq '[.secrets[].name \| select(startswith("TERRAFORM_PROVIDER_GPG"))]'` | `[]` |
| Two or more reviewers, no self-review | `release-signing`, Required reviewers | `gh api $R/environments/release-signing --jq '.protection_rules[] \| select(.type == "required_reviewers") \| {prevent_self_review, reviewers: (.reviewers \| length)}'` | `reviewers` 2 or more, `prevent_self_review: true` |
| No admin bypass | `release-signing`, "Allow administrators to bypass" | `gh api $R/environments/release-signing --jq .can_admins_bypass` | `false` |
| Deploys from protected branches only | `release-signing`, Deployment branches | `gh api $R/environments/release-signing --jq .deployment_branch_policy` | `protected_branches: true` |
| Fork runs get no secrets | Settings, Actions, General | `gh api $R/actions/permissions/fork-pr-workflows-private-repos` | `send_secrets_and_variables: false`, or approval required |
| Old tag workflow disabled | Actions, "Retired tag release workflow" | `gh api $R/actions/workflows/release.yml --jq .state` | `disabled_manually` |
| Release tags can't change | Settings, Rules, Rulesets, the `refs/tags/v*` ruleset | `gh api $R/rulesets --jq '.[].name'`, then that ruleset's `[.rules[].type]` | `deletion`, `non_fast_forward` and `update` |
| `main` protected | Settings, Rules | `gh api $R/rules/branches/main --jq '[.[].type]'` | A pull request with one approval, signed commits, the `CI` check, no force push |

- The fork-run setting matters only while the repository is private.
- The tag ruleset needs `update` too, since moving a tag forward counts as a
  fast-forward.

### The signing key

Three `release-signing` environment secrets:

| Secret | Value |
| --- | --- |
| `TERRAFORM_PROVIDER_GPG_PRIVATE_KEY` | The ASCII-armored secret key, from `gpg --armor --export-secret-keys <fingerprint>` |
| `TERRAFORM_PROVIDER_GPG_KEY_FINGERPRINT` | The primary key's 40-hex-digit fingerprint, no spaces |
| `TERRAFORM_PROVIDER_GPG_PASSPHRASE` | The key's passphrase |

The key must be RSA or DSA, since the registry rejects ECC. `gpg
--full-generate-key` offers ECC by default, so pick "RSA and RSA", 4096 bits.

Set each one like this, reading the value from a file:

```sh
gh secret set TERRAFORM_PROVIDER_GPG_PRIVATE_KEY --env release-signing < private-key.asc
```

## Cut a release

`VERSION` is the version without the leading `v`, for example `0.1.0`.

1. **Pick the version.** `vMAJOR.MINOR.PATCH`, or a prerelease such as
   `v0.1.0-beta.1`. Build metadata (`+...`) is refused. A new major is
   refused until the dispatch sets `allow_major` (step 5).

2. **Update `CHANGELOG.md` in a pull request.** Render the section from
   `.changelog/`:

   ```sh
   make changelog VERSION="$VERSION" CHANGELOG_DATE=YYYY-MM-DD
   ```

   - Put it under the preamble, above the previous release.
   - Delete the `.changelog/<number>.txt` files it used, in the same pull
     request. Keep `.changelog/README.md`.
   - A new major also needs a migration in `docs/public-compatibility.md` for
     each BREAKING CHANGES line.
   - First release only: rename the hand-written `## [Unreleased]` section to
     `## [VERSION] - YYYY-MM-DD, recorded before .changelog/` and keep its
     text. The first beta replaces the
     `[0.1.0-beta] - planned, not published` placeholder.
   - A dated heading makes `TestNoReleaseIsRecordedInTheChangelog` fail on
     purpose. Rewrite `docs/distribution.md`, which says nothing is published,
     in the same pull request.

3. **Rehearse on the commit you'll tag.** CI must be green. On Linux, or with
   GNU findutils and bash 4, with `syft` on `PATH`:

   ```sh
   make release-validate release-config-test
   ```

   `release-validate` builds an unsigned snapshot and checks its files.

4. **Tag the merged commit on `main` and push the tag.** `-s` signs the tag.

   ```sh
   git fetch origin main
   git tag -s "v$VERSION" -m "v$VERSION" origin/main
   git push origin "v$VERSION"
   ```

   - The tag must be on `main`'s first-parent history.
   - Pushing it starts nothing.
   - A pushed tag can't be deleted or moved. If it's wrong, cut the next
     version.

5. **Dispatch from `main`**, then find the run:

   ```sh
   gh workflow run release-dispatch.yml --ref main -f tag="v$VERSION"
   gh run list --workflow release-dispatch.yml --limit 1
   ```

   For a new major only, add `-f allow_major=true` once the questions the
   check prints are answered.

6. **Approve.** "Validate release tag" runs first. Then "Build signed provider
   artifacts" waits for approval. A required reviewer who didn't dispatch the
   run approves it under "Review deployments".

7. **Download the artifact** straight away, since workflow artifacts expire:

   ```sh
   gh run download <run-id> -n "terraform-provider-k0rdent-v$VERSION" -D "release-v$VERSION"
   ```

   It holds:

   - five archives: `terraform-provider-k0rdent_VERSION_{darwin,linux}_{amd64,arm64}.zip`
     and `..._windows_amd64.zip`
   - an SBOM per archive, `<archive>.sbom.json`, each with a `.sig`
   - `terraform-provider-k0rdent_VERSION_manifest.json`
   - `terraform-provider-k0rdent_VERSION_SHA256SUMS`, covering all of the
     above, and its signature `..._SHA256SUMS.sig`

8. **Verify it as a user would.** Import the public key, then check the
   signature and every file's checksum:

   ```sh
   cd "release-v$VERSION"
   gpg --verify "terraform-provider-k0rdent_${VERSION}_SHA256SUMS.sig" "terraform-provider-k0rdent_${VERSION}_SHA256SUMS"
   sha256sum --check "terraform-provider-k0rdent_${VERSION}_SHA256SUMS"
   ```

   Check the fingerprint gpg prints is the published one. To rerun the full
   check from the repository root, on Linux:

   ```sh
   EXPECTED_VERSION="$VERSION" GPG_KEY_FINGERPRINT=<fingerprint> ./scripts/validate-release-artifacts.sh "release-v$VERSION"
   ```

9. **Publish.** Attach exactly these files to a GitHub Release for the tag.
   Never replace a published file. Then check `terraform init` and `tofu init`
   install it on a clean machine. See [docs/distribution.md](docs/distribution.md)
   and [docs/install.md](docs/install.md).

The release is built with the Go version in `.go-release-version`. To change
it, change that file. `make vulncheck` scans under both CI's Go and that one.

## If something fails

The workflow names each failure in its error title.

| Title | Cause | Fix |
| --- | --- | --- |
| Release tag is not on protected main | The tag isn't on `origin/main`'s first-parent history, usually because it was made on a branch before the merge | Don't move the tag. Tag the merged commit as the next version |
| Dispatcher must run from main | The dispatch used another `--ref` | Dispatch with `--ref main` |
| Invalid release tag | Not a valid version | Use `vMAJOR.MINOR.PATCH` or a prerelease |
| Required release secret is not set | One of the three GPG secrets is empty. Nothing was built | Set it, then dispatch again |
| Unsupported signing key algorithm | The key isn't RSA or DSA | Make an RSA key, replace all three secrets, dispatch again |
| Major version release is gated | A new major needs `allow_major` | See step 5 |
