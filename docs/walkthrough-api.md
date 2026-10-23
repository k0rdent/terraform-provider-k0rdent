# Checking the API with curl

Creates a cluster, checks its nodes, and deletes it, with only `curl` and
`jq`. If this works, the API can do what
[the hardware walkthrough](walkthrough-hardware.md) needs. If it fails, the
problem is the platform, not the provider.

Run it when a platform is new or has changed, before a demo, and when a
provider run fails.

It builds the same cluster as the hardware walkthrough: the first worker pool
of one cluster type, at its smallest size.

You need `curl`, `jq`, and `kubectl` for step 5. Works in bash and zsh.

## 1. Set up the shell

Fill in the values at the top. `read -rs` reads the secret without echoing
it, so it stays out of your shell history. The token stays in a shell
variable.

| Value | What it is |
| --- | --- |
| `REGION` | The region id. `global` on a platform that has only that |
| `PROJECT` | An existing project id |
| `CLUSTER_TYPE` | The cluster type to build from |
| `NAME` | The new cluster's id |

The block also defines four helpers:

| Helper | What it does |
| --- | --- |
| `mint` | Gets a token. A credential gets ten a minute |
| `api` | Sends one request with the token. Prints the body, then the HTTP code on the last line |
| `look` | Shows the cluster's state and current operation |
| `events` | Shows what the platform logged for the current operation |

```sh
export K0RDENT_ENDPOINT=https://<the gateway's host>
export K0RDENT_CLIENT_ID=<the service account's client id>
printf 'client secret: ' >&2
read -rs K0RDENT_CLIENT_SECRET && export K0RDENT_CLIENT_SECRET && echo >&2

REGION=<region id>
PROJECT=<a project id>
CLUSTER_TYPE=<a cluster type id>
NAME=curl-walkthrough

T="$K0RDENT_ENDPOINT/v1/regions/$REGION/compute/cluster-types/$CLUSTER_TYPE"
P="$K0RDENT_ENDPOINT/v1/regions/$REGION/projects/$PROJECT/compute/clusters"
C="$P/$NAME"

mint() {
  TOKEN=$(jq -nc '{grant_type:"client_credentials", client_id:env.K0RDENT_CLIENT_ID, client_secret:env.K0RDENT_CLIENT_SECRET}' \
    | curl -sS -X POST "$K0RDENT_ENDPOINT/v1/regions/global/auth/token" \
        -H 'Content-Type: application/json' --data-binary @- \
    | jq -r .access_token)
  [ -n "$TOKEN" ] && [ "$TOKEN" != null ] && echo "token ok"
}

api() { printf 'Authorization: Bearer %s\n' "$TOKEN" | curl -sS -H @- -w '\n%{http_code}\n' "$@"; }

look() { api "$C" | sed '$d' | jq -c '{state, stateReason, stateMessage, operation, nodeCount, updatedAt}'; }

events() { api "$C/runtime/events" | sed '$d' | jq -c '.events[]? | {occurredAt, level, stage, message}'; }
```

## 2. Check the cluster type

```sh
mint
date -u
api "$T" | sed '$d' | jq '{status, selfLink, workers: [.nodePools[] | select(.role == "worker") | {id, nodeCountMin, nodeCountMax}]}'
```

`status` has to be `available`, with at least one worker pool. If the token
fails, check the endpoint and the credential.

## 3. Create the cluster

This builds the request body from the cluster type's first worker pool, at
its minimum size, then sends it.

```sh
BODY=$(api "$T" | sed '$d' | jq -c --arg id "$NAME" '{
  id: $id,
  displayName: "curl walkthrough",
  clusterType: .selfLink,
  nodePools: [[.nodePools[] | select(.role == "worker")][0] | {id, nodeCount: (.nodeCountMin // 0)}]
}')
echo "$BODY" | jq .
date -u
api -X POST -H 'Content-Type: application/json' --data-binary "$BODY" "$P" | sed '$s/^/HTTP /'
```

`201` means the create was accepted. Anything else prints the platform's
error above the code. Note the time.

## 4. Wait for it to be active

Checks every 30 seconds, gets a new token when the old one expires, and stops
at `active`, at `failed`, or when a read fails.

```sh
while :; do
  r=$(api "$C"); code=$(printf '%s' "$r" | tail -1)
  [ "$code" = 401 ] && { mint; continue; }
  [ "$code" = 200 ] || { printf '%s\n' "$r"; break; }
  state=$(printf '%s' "$r" | sed '$d' | jq -r .state)
  echo "$(date -u +%H:%M:%S) $state"
  case "$state" in active | failed) break ;; esac
  sleep 30
done
look
events
```

On `failed`, keep what `look` and `events` printed, and the time from step 3.

## 5. Check the nodes

The kubeconfig is a credential. `mktemp` makes a file only you can read, and
the block deletes it at the end.

```sh
KC=$(mktemp)
code=$(printf 'Authorization: Bearer %s\n' "$TOKEN" | curl -sS -H @- -o "$KC" -w '%{http_code}' "$C/kubeconfig")
echo "HTTP $code"
[ "$code" = 200 ] && kubectl --kubeconfig "$KC" get nodes -o wide
rm -f "$KC"
```

The nodes should be `Ready`. A `404` means the kubeconfig isn't ready yet, so
wait a minute and run it again.

## 6. Delete it

```sh
date -u
api -X DELETE "$C" | sed '$s/^/HTTP /'
```

`204` means the delete was accepted. A `409` names the state that blocks it.

## 7. Wait for it to be gone

```sh
while :; do
  r=$(api "$C"); code=$(printf '%s' "$r" | tail -1)
  [ "$code" = 401 ] && { mint; continue; }
  [ "$code" = 404 ] && { echo "$(date -u +%H:%M:%S) gone"; break; }
  [ "$code" = 200 ] || { printf '%s\n' "$r"; break; }
  state=$(printf '%s' "$r" | sed '$d' | jq -r .state)
  echo "$(date -u +%H:%M:%S) $state"
  [ "$state" = failed ] && break
  sleep 30
done
```

On `failed`, run `look` and `events` and keep what they print.

## 8. Clean up the shell

```sh
unset TOKEN K0RDENT_CLIENT_SECRET BODY
```

## Testing a cancel

A destroy of a cluster that's still `creating` cancels the create, waits for
it to settle, then deletes. To test that: do steps 1 to 3, run this, then do
step 4 to wait for the cluster to settle, and then steps 6 and 7. Skip step 5.

```sh
EXEC=$(api "$C" | sed '$d' | jq -r .operation.executionId)
echo "cancelling $EXEC"
date -u
api -X POST -H 'Content-Type: application/json' \
  --data-binary "$(jq -nc --arg e "$EXEC" '{executionId: $e}')" "$C/cancel" | tail -1 | sed 's/^/HTTP /'
look
```

`202` means the stop was requested. The cluster stays `creating`, with
`operation.status` `cancelling`, while the run winds down, then settles
`failed` with `stateReason` `CANCELLED`. Step 4 waits for that, and a delete
sent before it is refused with `409`. A `409` here means the create already
finished.

## What to send when it fails

| What | Where it came from |
| --- | --- |
| Cluster, project, organization, region | Step 1 |
| Cluster type and the body sent | Steps 2 and 3 |
| When the create or delete was accepted | The `date -u` before it |
| `state`, `stateReason`, `stateMessage`, `operation` | `look` |
| The runtime events, or that there were none | `events` |
