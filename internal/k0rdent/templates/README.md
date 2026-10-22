# Generator template overrides

Each `<name>.tmpl` in this directory overrides the pinned generator's stock
template of the same name, and an `oapi-codegen.*.yaml` points
`user-templates` at it. Beside each one sits `<name>.stock.tmpl`, the
unmodified stock template, byte for byte. Nothing reads the stock copy at
generation time. It exists so `make client-template-check` can diff it against the
pinned generator's own copy, which is the only thing that catches the failure
mode an override introduces: on a generator upgrade the vendored copy keeps emitting
the old shape, the committed output still matches it, and
`make client-check` passes while the client silently misses everything the
new template would have produced.

`make client-template-check` finds the pairs by name, so it needs no editing when an
override is added. It fails when an override has no stock copy, when a stock
copy has no override, when no uncommented line in an `oapi-codegen.*.yaml`
names the override, or when a stock copy differs from the generator's. It only
looks at this directory's top level, matching the generator's top-level
`templates/`. A template with no stock counterpart goes in `added/`, and the
check fails if the generator ever ships a stock template with its name, since
it would then be an override with nothing to diff it against.

Each override is the stock template with its changes:

- `client-with-responses.tmpl` adds `HTTPResult`, a nil-safe accessor returning
  the response and the body together. `k0rdent/httpresult.go` says why that
  method has to exist and why the stock getters cannot provide it.
  `ParseXxxResponse` also returns the response together with the error when a
  body fails to decode or a declared header fails to bind. Stock returns
  `nil, err`, which drops `Body` and `HTTPResponse` with it. For a credential
  create that loses the one-time secret, which the server can't show again. A
  nil response still means nothing arrived: the transport failed or the body
  couldn't be read. The stock unmarshal code is emitted by a Go function, not
  the template, and writes its own `return nil, err`, so the override emits it
  in an unexported `decodeXxxResponse` helper and returns the response from
  `Parse`. The switch keeps its indentation, so a regenerate diffs by the call
  alone.
- `constants.tmpl`, read by `oapi-codegen.types.yaml`, adds `XxxValues()`,
  the member list in spec order, beside each enum's `Valid()`. A spec value
  the block can't match to a constant emits an undefined name, so the build
  fails rather than the list coming up short.
- `request-bodies.tmpl`, read by `oapi-codegen.types.yaml`, declares a request
  body over a union or a map as an alias of its body type. Stock declares
  `type MintTokenJSONRequestBody MintTokenJSONBody`, a defined type, which
  does not carry the union's `MarshalJSON`. `json.Marshal` then sees a struct
  whose only field is unexported, so the typed `MintToken` sent `{}` and the
  server answered 400. Every other request body is a plain struct, so its
  declaration is unchanged.
- `client.tmpl` adds the path parameter check to every request builder, and
  `PathParamError`, which it returns. The stock builder percent-escapes a path
  parameter and then resolves the URL against the server address, and that
  resolution removes dot segments: id `..` on deleteCluster built
  `DELETE /v1/regions/r1/projects/proj/compute/`. The check refuses a value
  that is not exactly one segment, and holds each parameter to the `pattern`,
  `minLength`, `maxLength` and `enum` the prepared spec declares for it.
  It also adds two lines and an include. Each generated method puts its
  `operationId` on the request context, where `OperationIDFromContext` reads
  it, so a transport can tell which operation a request is without matching
  its URL. The include renders `added/operations.tmpl`.

`added/` holds templates with no stock counterpart, so there is no stock copy
to diff, and `make client-template-check` only makes sure none of them shares a stock
template's name. `added/operations.tmpl` renders `Operations`, the method,
path template, deprecated flag and documented responses of every operation,
from the same operation list the client methods are built from. It keys
responses by exact status, so a `default` or a range like `4XX` is left out,
and it keeps descriptions only outside 2xx. `templates_test.go` generates a
client for a spec with both kinds of key.

Overriding a template was the alternative to hand-writing one method per
generated response type, or one wrapper per operation. Those would go stale
the first time the contract adds an operation, and nothing would say so. The
path check is in the builder for the same reason, and because the builder is
the only place that sees the segment the URL is resolved from. A caller that
builds its own request with `NewXxxRequest` gets it too.

## Changing an override

1. `make client-template-check`. If it fails, the generator moved: copy the new stock
   template over `<name>.stock.tmpl` first.
2. Make the edit to `<name>.tmpl`, never to the `.stock.tmpl`.
3. `diff -u <name>.stock.tmpl <name>.tmpl` should show only the additions, so
   the next upgrade can be re-applied by reading that diff.
4. `make client-generate` and read the diff on the generated file.

## Adding another override

Copy the stock template in twice, as `<name>.tmpl` and `<name>.stock.tmpl`,
point `user-templates` at `templates/<name>.tmpl`, regenerate, and confirm
`git diff` on the generated output is empty before changing a character. An
empty diff shows the copy matches the stock template, but an override the
generator never reads gives an empty diff too. To prove it is read, add a Go
comment such as `// override read` to `<name>.tmpl`, regenerate, see it land in
the output, and take it out again. `make client-template-check` picks the new pair up
on its own.
