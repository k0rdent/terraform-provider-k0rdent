# Releasing

For maintainers: how to cut a signed release, and the repository settings it
needs. [docs/release-signing.md](docs/release-signing.md) says what each
control protects.

The flow:

1. Push a signed tag on `main`, or on the version's release branch.
2. Dispatch `release-dispatch.yml` from `main`.
3. A job with no secrets checks the tag.
4. A second maintainer approves the `release-signing` environment.
5. The signing job builds, signs and checks the release, and uploads it to a
   draft GitHub Release.
6. The same job checks the draft holds exactly the checked files, then
   publishes it. The Terraform and OpenTofu registries read the published
   release.

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
| Deploys from `main` only | `release-signing`, Deployment branches, "Selected branches and tags" | `gh api $R/environments/release-signing/deployment-branch-policies --jq '[.branch_policies[] \| {name, type}]'` | `[{"name":"main","type":"branch"}]`, and `.deployment_branch_policy` shows `custom_branch_policies: true` |
| Fork runs get no secrets | Settings, Actions, General | `gh api $R/actions/permissions/fork-pr-workflows-private-repos` | `send_secrets_and_variables: false`, or approval required |
| Old tag workflow disabled | Actions, "Retired tag release workflow" | `gh api $R/actions/workflows/release.yml --jq .state` | `disabled_manually` |
| Release tags can't change | Settings, Rules, Rulesets, the `refs/tags/v*` ruleset | `gh api $R/rulesets --jq '.[].name'`, then that ruleset's `[.rules[].type]` | `deletion`, `non_fast_forward` and `update` |
| `main` protected | Settings, Rules | `gh api $R/rules/branches/main --jq '[.[].type]'` | A pull request with one approval, signed commits, the `CI` check, no force push |
| Release branches protected | Settings, Rules, a ruleset on `refs/heads/release-v*` | `gh api $R/rules/branches/release-v0.1.x --jq '[.[].type]'` | The same as `main`, plus `deletion`. The dispatch refuses a branch without `pull_request` with one approval or more, `required_signatures`, `required_status_checks` including `CI`, `non_fast_forward` and `deletion` |

- `main` only, not every protected branch: both jobs that use the
  environment refuse to run unless started from `main`, and the release
  builds a release branch's tags from `main` by their name.
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

## Versions

- **Spelling.** `vMAJOR.MINOR.PATCH` for a release. A release candidate adds
  `-rc.N`: `v0.1.0-rc.1`, `v0.1.0-rc.2`, then `v0.1.0`. The dot makes `rc.10`
  sort after `rc.9`.
- **Candidates are opt-in.** Terraform and OpenTofu never choose a
  prerelease for a constraint such as `~> 0.1`. Only an exact
  `version = "0.1.0-rc.1"` installs one, so a candidate on the registry reaches
  only the people testing it.
- **Candidates aren't GitHub pre-releases.** registry.terraform.io ignores a
  GitHub release marked pre-release. The registry's first publish of
  `v0.1.0-rc.1` failed with "release is a draft or pre-release" until the mark
  was cleared by hand, though registry.opentofu.org listed it while marked.
  So `.goreleaser.yaml` doesn't set `prerelease`, and the release job publishes
  with `prerelease=false` whatever the tag's `.goreleaser.yaml` says. HashiCorp's
  [publishing docs](https://developer.hashicorp.com/terraform/registry/providers/publishing)
  don't say so, and
  [other providers hit it](https://discuss.hashicorp.com/t/publishing-pre-release-versions-of-provider-ignored-draft-release-event/69536).
- **Every version is permanent.** A published release can't be replaced, and
  users' lock files and caches hold its checksums. If one is wrong, fix
  forward with the next version.
- **Only the newest final version is Latest.** GitHub marks a release Latest
  by date unless told otherwise, and `/releases/latest` and the repository
  page point people to it. The release job publishes a candidate, and a
  patch to an older minor, with `make_latest=false`. Any later update to a
  release that doesn't pass `make_latest` makes it Latest, which is how
  editing `v0.1.0-rc.2`'s notes made it Latest. Before the first final
  version, a candidate can still be Latest. The version's `-rc.N`, not any
  GitHub setting, is what Terraform and OpenTofu read.
- **A tag push starts nothing.** A release is built only when someone
  dispatches the release workflow from `main` and a second maintainer
  approves it.

## Release branches

Release candidates and patches for a minor version come from its release
branch, `release-vMAJOR.MINOR.x`, so `main` can take new work meanwhile.

- **Cut it from `main`** when the first candidate is due, and push it before
  tagging:

  ```sh
  git fetch origin main
  git push origin origin/main:refs/heads/release-v0.1.x
  ```

- **Tag on it**, not on `main`: `v0.1.0-rc.1`, `v0.1.0-rc.2`, then `v0.1.0`,
  and later `v0.1.1`. The dispatch builds a tag from `main` or from the release
  branch for that tag's minor version, and from no other branch.
- **Everything on it comes through a pull request.** Rules alone don't prove
  that, since the push that creates a branch isn't a pull request. So for a
  tag on a release branch, the dispatch also checks:
  - the branch leaves `main` at a commit of `main`'s own history
  - every commit on the branch's own history since then is the merge of a
    pull request into that branch, approved by someone other than its author

  Cut the branch at a `main` commit, as above, and change it only by merging
  pull requests.
- **Fix on `main` first.** Then backport each fix in its own pull request to
  the release branch, made with `git cherry-pick -x <main commit>`, so the
  commit names where it came from. If it doesn't apply, rewrite the fix for
  the branch's code, port its test, and say so in the pull request.
- **Don't merge `main` into the release branch** once candidates are being
  tested. Only backports go in.
- **Never merge the release branch back into `main`.** Fixes reach `main`
  first. A tag that ends up reachable from `main` only through such a merge
  is checked as a release-branch tag, not as a `main` one.

## Cut a release

`VERSION` is the version without the leading `v`, for example `0.1.0`.

1. **Pick the version.** `vMAJOR.MINOR.PATCH`, or a candidate such as
   `v0.1.0-rc.1`. See [Versions](#versions). Build metadata (`+...`) is refused. A new major is
   refused until the dispatch sets `allow_major` (step 5).

2. **Update `CHANGELOG.md`, for a final version only.** A candidate doesn't
   change it: skip to step 3. Every candidate for `0.1.0` ships the same
   entries, and they all belong in the `0.1.0` section.

   For the final version, render the section on the release branch, whose
   `.changelog/` holds exactly what ships. `main` may already hold entries
   for the next minor.

   ```sh
   git switch --detach origin/release-v0.1.x
   make changelog VERSION="$VERSION" CHANGELOG_DATE=YYYY-MM-DD
   ```

   Then, like any fix, `main` first:

   - Open a pull request to `main` that adds that section under the preamble,
     above the previous release, and deletes the `.changelog/<number>.txt`
     files it used. Keep `.changelog/README.md`, and keep any entry the
     release branch doesn't have.
   - Backport it to the release branch with `git cherry-pick -x`, so both
     branches carry the same section and neither renders those entries again.
   - A new major also needs a migration in `docs/public-compatibility.md` for
     each BREAKING CHANGES line.
   - First final release only: rename the hand-written `## [Unreleased]`
     section to `## [VERSION] - YYYY-MM-DD, recorded before .changelog/` and
     keep its text. It replaces the `[0.1.0] - planned, not published`
     placeholder, which candidates leave in place.
   - A dated heading makes `TestNoReleaseIsRecordedInTheChangelog` fail on
     purpose. Rewrite `docs/distribution.md`, which says nothing is published,
     in the same pull request.

3. **Rehearse on the commit you'll tag.** CI must be green. On Linux, or with
   GNU findutils and bash 4:

   ```sh
   make release-validate release-config-test
   ```

   `release-validate` builds an unsigned snapshot and checks its files.

   Then check an upgrade by hand, since CI doesn't. State written by the
   previous release, installed from the registry, must plan empty under the
   candidate with the previous release's configuration. Build the candidate
   into a filesystem mirror, and write one CLI configuration for each source:

   ```sh
   PREVIOUS=0.1.0-rc.1
   WORK="$(mktemp -d)"
   git archive "v$PREVIOUS" examples/walkthrough | tar -x -C "$WORK"
   PLATFORM="$(go env GOOS)_$(go env GOARCH)"
   for host in registry.terraform.io registry.opentofu.org; do
     go build -ldflags "-X main.version=$VERSION" -o "$WORK/mirror/$host/k0rdent/k0rdent/$VERSION/$PLATFORM/terraform-provider-k0rdent_v$VERSION" .
   done
   printf 'provider_installation {\n  direct {}\n}\n' > "$WORK/registry.tfrc"
   cat > "$WORK/candidate.tfrc" <<EOF
   provider_installation {
     filesystem_mirror {
       path    = "$WORK/mirror"
       include = ["registry.terraform.io/k0rdent/k0rdent", "registry.opentofu.org/k0rdent/k0rdent"]
     }
     direct {
       exclude = ["registry.terraform.io/k0rdent/k0rdent", "registry.opentofu.org/k0rdent/k0rdent"]
     }
   }
   EOF
   ```

   Then in each directory under `$WORK/examples/walkthrough`, with the
   variables [docs/walkthrough-hardware.md](docs/walkthrough-hardware.md)
   gives, against a deployment you can create test objects on or the fake from
   `make demo-up`:

   1. Pin the previous release: add `version = "<PREVIOUS>"`, such as
      `version = "0.1.0-rc.1"`, under
      `source = "k0rdent/k0rdent"` in `main.tf`. Create its objects:

      ```sh
      TF_CLI_CONFIG_FILE="$WORK/registry.tfrc" terraform init
      TF_CLI_CONFIG_FILE="$WORK/registry.tfrc" terraform apply
      ```

   2. Change the pin to the candidate's version, keep the rest of the
      configuration, and plan:

      ```sh
      TF_CLI_CONFIG_FILE="$WORK/candidate.tfrc" terraform init -upgrade
      TF_CLI_CONFIG_FILE="$WORK/candidate.tfrc" terraform plan -detailed-exitcode
      ```

      Exit 0 means no changes. Exit 2 means the candidate would change
      something the previous release wrote. Read the plan. A change a fix in
      this version explains, such as an output that now lists more, goes in
      the upgrade notes (step 11). Don't tag a candidate with any other
      change.

   3. Destroy with the candidate. Then repeat steps 1 to 3 with `tofu` in a
      fresh copy of the directory.

4. **Tag the merged commit and push the tag.** `-s` signs the tag. `BRANCH`
   is the release branch for a candidate, the final version and its patches.
   It's `main` only for a version released straight from `main`, with no
   release branch.

   ```sh
   BRANCH=release-v0.1.x
   git fetch origin "$BRANCH"
   git tag -s "v$VERSION" -m "v$VERSION" "origin/$BRANCH"
   git push origin "v$VERSION"
   ```

   - The tag must be on that branch's first-parent history: a merge, or a
     commit made on it.
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

7. **Download the published release:**

   ```sh
   gh release download "v$VERSION" -D "release-v$VERSION"
   ```

   The job publishes it only after checking the draft holds exactly the files
   it built and validated. It holds:

   - five archives: `terraform-provider-k0rdent_VERSION_{darwin,linux}_{amd64,arm64}.zip`
     and `..._windows_amd64.zip`
   - an SBOM per archive, `<archive>.sbom.json`, each with a `.sig`
   - `terraform-provider-k0rdent_VERSION_manifest.json`
   - `terraform-provider-k0rdent_VERSION_SHA256SUMS`, covering all of the
     above, and its signature `..._SHA256SUMS.sig`

8. **Verify it as a user would.** Import the public key, then check the
   signature and every file's checksum:

   ```sh
   gpg --import docs/signing-key.asc
   cd "release-v$VERSION"
   gpg --verify "terraform-provider-k0rdent_${VERSION}_SHA256SUMS.sig" "terraform-provider-k0rdent_${VERSION}_SHA256SUMS"
   sha256sum --check "terraform-provider-k0rdent_${VERSION}_SHA256SUMS"
   ```

   Check the primary key fingerprint gpg prints is the one in `SECURITY.md`. To rerun the full
   check from the repository root, on Linux:

   ```sh
   EXPECTED_VERSION="$VERSION" GPG_KEY_FINGERPRINT=<fingerprint> ./scripts/validate-release-artifacts.sh "release-v$VERSION"
   ```

9. **Check the registries took it.** Each lists its versions:

   ```sh
   curl -s https://registry.terraform.io/v1/providers/k0rdent/k0rdent/versions | jq -r '.versions[].version'
   curl -s https://registry.opentofu.org/v1/providers/k0rdent/k0rdent/versions | jq -r '.versions[].version'
   ```

   registry.terraform.io learns of a release from the repository webhook
   whose URL starts `https://registry.terraform.io/`. If the version is
   missing there, read that webhook's recent deliveries. A `release` delivery
   the registry answered with "Ignored draft release event" means the release
   was marked pre-release or still a draft. Reading deliveries needs the
   `admin:repo_hook` scope (`gh auth refresh -s admin:repo_hook`). Delivery ids
   are above 2^53, where a float64 no longer holds every integer, so as a
   precaution the second command prints the list with Python, not `--jq`:

   ```sh
   HOOK="$(gh api $R/hooks --jq '.[] | select(.config.url | startswith("https://registry.terraform.io/")) | .id')"
   gh api "$R/hooks/$HOOK/deliveries?per_page=20" | python3 -c 'import json, sys; [print(d["id"], d["delivered_at"], d["event"], d["action"], d["status_code"]) for d in json.load(sys.stdin)]'
   gh api "$R/hooks/$HOOK/deliveries/<id>" --jq '.response.payload'
   ```

   registry.opentofu.org lags behind its own repository. For `v0.1.0-rc.2`
   the repository had the version 1 minute after the publish and the API
   listed it 21 minutes after, because its CDN caches the versions list
   (`max-age=14400`, so up to four hours). `curl -sI` on the versions URL
   skips that cache: a `content-length` there larger than a plain GET's means
   the version is in and only the cache is behind.

10. **Check it installs.** Never replace a published file. Check
    `terraform init` and `tofu init` install it on a clean machine, a
    candidate with an exact `version`. See
    [docs/distribution.md](docs/distribution.md) and
    [docs/install.md](docs/install.md).

11. **Write the release notes** in the GitHub release's body. The job
    publishes it empty. Say:

    - the k0rdent API version it was tested against, such as "the k0rdent API
      v1.3.0-rc.4"
    - the exact pin that installs it: `version = "0.1.0-rc.2"`
    - upgrade notes: what changes for state or configuration written by the
      previous release, or that nothing does
    - known issues, each with its workaround

    Pass `--latest`, or the edit makes this release Latest. Work it out as
    the job does: true only for a final version at least as new as every
    published final one.

    ```sh
    LATEST=false
    if [[ "$VERSION" != *-* ]]; then
      PUBLISHED="$(gh api --paginate "$R/releases?per_page=100" --jq '.[] | select((.draft or .prerelease) | not) | .tag_name')"
      NEWEST="$(printf '%s\n' "$PUBLISHED" "v$VERSION" | grep -v -e '-' | sort -V | tail -n 1)"
      if [[ "$NEWEST" == "v$VERSION" ]]; then LATEST=true; fi
    fi
    gh release edit "v$VERSION" --notes-file notes.md --latest="$LATEST"
    ```

The release is built with the Go version in `.go-release-version`. To change
it, change that file. `make vulncheck` scans under both CI's Go and that one.

## If something fails

The workflow names each failure in its error title.

| Title | Cause | Fix |
| --- | --- | --- |
| Release tag is not a commit of a release branch | The tag is on the own history of neither `main` nor its version's release branch: on another branch, or on a branch merged into one of them, usually because it was made before the merge | Don't move the tag. Tag the merge commit, or a commit on one of them, as the next version |
| Release branch is not protected | The release branch lacks one of the rules the dispatch checks, its pull requests need no approval, or it doesn't require `CI` | Fix the release-branch ruleset, then dispatch again |
| Release branch doesn't start from main | The branch was cut from a commit that isn't on `main`'s own history | Cut a new release branch for the next version from `main` |
| Release branch has an unreviewed commit | A commit was pushed to the release branch rather than merged through a pull request, or its pull request has no approval from someone other than its author | Don't move the tag. Revert it through a pull request and tag the next version |
| Dispatcher must run from main | The dispatch used another `--ref` | Dispatch with `--ref main` |
| Invalid release tag | Not a valid version | Use `vMAJOR.MINOR.PATCH` or a prerelease |
| Required release secret is not set | One of the three GPG secrets is empty. Nothing was built | Set it, then dispatch again |
| Unsupported signing key algorithm | The key isn't RSA or DSA | Make an RSA key, replace all three secrets, dispatch again |
| Major version release is gated | A new major needs `allow_major` | See step 5 |
