# Changelog entries

Every pull request that changes something a configuration can see adds one
file here, named for its number: `.changelog/123.txt` for pull request 123.
`make changelog` renders the files into the next release's section of
[`CHANGELOG.md`](../CHANGELOG.md), in the format HashiCorp's
[changelog specification](https://developer.hashicorp.com/terraform/plugin/best-practices/versioning#changelog-specification)
sets out and the AWS and Google providers publish.

One file per pull request means two branches never edit the same lines, so
entries don't conflict however the branches are stacked or merged. It's the
[go-changelog](https://github.com/hashicorp/go-changelog) convention, and the
blocks are go-changelog's, so the files read the same as
[the AWS provider's](https://github.com/hashicorp/terraform-provider-aws/tree/main/.changelog).

The examples below are indented so this file isn't read as an entry. In a real
entry the fences start at the beginning of the line.

## The format

A file holds one or more blocks. Each block is a type and one line of text:

    ```release-note:enhancement
    resource/k0rdent_project: Add `region` argument
    ```

    ```release-note:bug
    data-source/k0rdent_regions: Fix `project_region_ids` reading as empty when every region hosts projects
    ```

The line starts with what it changes, then a colon:

| Prefix | For |
| --- | --- |
| `provider: ` | the provider block, authentication, or anything every resource sees |
| `resource/k0rdent_<name>: ` | a managed resource |
| `data-source/k0rdent_<name>: ` | a data source |
| `ephemeral-resource/k0rdent_<name>: ` | an ephemeral resource |
| `list-resource/k0rdent_<name>: ` | a list resource, read by `terraform query` |
| `action/k0rdent_<name>: ` | an action |
| `function/<name>: ` | a provider function |

A change to several resources gets a block for each, so a reader scanning for
one resource finds every line about it.

## The types

Each type renders under one heading, in this order:

| Type | Heading | The line holds |
| --- | --- | --- |
| `breaking-change` | BREAKING CHANGES | prefix and message |
| `note` | NOTES | prefix and message. Deprecations go here |
| `new-resource` | FEATURES | the type name only: `k0rdent_project` |
| `new-data-source` | FEATURES | the type name only |
| `new-ephemeral` | FEATURES | the type name only |
| `new-list-resource` | FEATURES | the type name only |
| `new-action` | FEATURES | the type name only |
| `new-function` | FEATURES | the function name only: `parse_id` |
| `new-guide` | FEATURES | the guide's title |
| `enhancement` | ENHANCEMENTS | prefix and message |
| `bug` | BUG FIXES | prefix and message |

A new resource's own arguments don't need entries of their own: the
`new-resource` line covers it. Names in a `new-*` block go without backticks,
since the rendered line adds them.

## Writing the line

- Imperative and specific: "Add `region` argument", "Fix `storage_ceiling_gib`
  reading as zero after import". Name the attribute, the value or the error.
- Backticks around attribute names, values and type names in the message.
- A `breaking-change` line says what a configuration has to change, and the
  migration goes in [`docs/public-compatibility.md`](../docs/public-compatibility.md).
- A deprecation is a `note` that says what replaces the deprecated thing and
  the version it's removed in, matching its `DeprecationMessage`. The
  deprecation policy in `docs/public-compatibility.md` sets that version.

## What CI checks on a pull request

The changelog workflow compares `.changelog/` with the pull request's base:

- The pull request adds or edits its own `<number>.txt`.
- It doesn't add a `PENDING-*.txt` placeholder. One it inherits from a branch
  further down a stack is left alone: that's the other branch's to rename.
- It doesn't add, edit or delete an entry named for another pull request.
  Each entry belongs to the pull request it's named for.
- Every entry in the directory is well formed.

This file and any file whose name starts with a dot aren't entries, and
none of the rules read them.

## When a pull request needs no entry

Tests, documentation, CI, refactoring and dependency bumps that change nothing
a configuration can see need no entry. Label the pull request
`no-changelog-needed` instead, and CI accepts it without one.

The same label lets a pull request add, edit or delete other pull requests'
entries, for one whose purpose is fixing them up. It never lets a pull request
add a placeholder, and it never lets a malformed entry through.

## Before the pull request has a number

Name the file `PENDING-<branch>.txt` and rename it to the number once the pull
request is open. `make changelog-check` accepts a placeholder, and CI refuses
one the pull request adds, naming the file it should be.

## Checking and rendering

| Command | What it does |
|---|---|
| `make changelog-check` | Checks every entry here is well formed. |
| `CHANGELOG_PR=123 CHANGELOG_BASE=origin/main make changelog-check` | Also checks what pull request 123 changed here. |
| `make changelog VERSION=0.2.0` | Renders the section, headed (Unreleased). |
| `make changelog VERSION=0.2.0 CHANGELOG_DATE=2026-10-20` | Renders the section, headed with that date. |

```sh
make changelog-check
CHANGELOG_PR=123 CHANGELOG_BASE=origin/main make changelog-check
make changelog VERSION=0.2.0
make changelog VERSION=0.2.0 CHANGELOG_DATE=2026-10-20
```

[`RELEASE.md`](../RELEASE.md) says how the rendered section goes into
`CHANGELOG.md` when a release is cut.
