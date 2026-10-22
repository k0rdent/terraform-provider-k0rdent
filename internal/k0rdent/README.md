# internal/k0rdent

The provider's k0rdent API client. It's generated from `spec/openapi.yaml`,
which holds only the operations the provider calls.

## Where it comes from

| What | Source |
| --- | --- |
| `spec/openapi.yaml` | `go/spec/prepared/openapi.yaml` in k0rdent-clients, cut to the operations in `spec/operations.txt` by `internal/tools/clientspec` |
| `spec/SOURCE.json` | The k0rdent-clients commit, the prepared spec's SHA-256, and the API release it was made from |
| `*.gen.go` | Generated here from `spec/` by `go generate` |
| `auth.go`, `client.go`, `error.go`, `httpresult.go`, the tests, `oapi-codegen.*.yaml`, `templates/`, `internal/tools/clientlimits` | Copied from `go/` in k0rdent-clients at the commit in `spec/SOURCE.json` |

k0rdent-clients is where the API spec is prepared and the generator is set up.
The prepared spec holds only public operations, and this package holds the
subset the provider sends.

Changed from the copy:

- The generator paths point at this directory.
- `httpresult.go` asserts `HTTPResulter` on `MintTokenResponse`, because the
  operation it named isn't here.
- The tests use operations that are here. Path parameter cases for enums, and
  the null enum member test, are left out because no operation here has one.

## Make targets

| Target | What it does | In CI |
| --- | --- | --- |
| `make client-generate` | Regenerates `*.gen.go` from `spec/` | No |
| `make client-check` | Regenerates, then fails if `*.gen.go` differ from what's committed or a new one appears. Runs `client-template-check` first | Yes, and in `make check` |
| `make client-template-check` | Fails if a vendored template in `templates/` no longer matches the pinned generator's stock one | Yes, through `client-check` |
| `make client-sync CLIENTS_GO=../k0rdent-clients/go` | Re-cuts `spec/` from a clean k0rdent-clients checkout, then regenerates | No, it needs the checkout |

## Changing which operations it has

1. Add or remove the operationId in `spec/operations.txt`.
2. `make client-sync CLIENTS_GO=<k0rdent-clients>/go`.
3. Commit `spec/` and `*.gen.go` together.

`TestTheClientHoldsOnlyTheOperationsTheProviderCalls` in `internal/provider`
fails when an operation here is one the provider no longer calls. A call to an
operation that isn't here doesn't compile.

## Moving to a newer API release

Update k0rdent-clients first, then run `make client-sync` against the new
commit. `spec/SOURCE.json` records it.

The generator is pinned by the `tool` line in `go.mod`. After an upgrade, run
`make client-template-check` and see `templates/README.md`.
