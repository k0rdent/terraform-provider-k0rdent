# Walkthrough on your laptop

Takes the provider from nothing to a cluster and back, against a local fake
API. You need only this repository: no account, no VPN, no credential.

For a real platform, use [the hardware walkthrough](walkthrough-hardware.md)
instead.

The result is `terraform output scorecard`: one row per feature, filled in by
the run.

## What you need

Go 1.26.10 or newer, `terraform` or `tofu`, `curl` and `lsof`. Check with:

```sh
scripts/doctor.sh
```

## 1. Build the provider and start the fake

Paste this whole block from the repository root. It defines a function,
`k0rdent_fake_up`, and runs it. The function:

- builds the provider and the fake into your own cache directory
- writes a `terraform.rc` that points Terraform at that build
- stops any fake already on port 18901, then starts a new one and waits for it
- exports the endpoint, region, project and the fake's credential

It's safe to run again whenever you come back: run `k0rdent_fake_up`.

```sh
k0rdent_fake_up() {
  DEV="${XDG_CACHE_HOME:-$HOME/.cache}/k0rdent-dev"
  PORT=18901
  command -v lsof >/dev/null || { echo "lsof is needed to free the port" >&2; return 1; }

  mkdir -p "$DEV" && chmod 700 "$DEV" || return 1
  go build -o "$DEV/terraform-provider-k0rdent" . || return 1
  go build -o "$DEV/fakeapi" ./internal/tools/fakeapi || return 1

  cat > "$DEV/terraform.rc" <<EOF
provider_installation {
  dev_overrides { "k0rdent/k0rdent" = "$DEV" }
  direct {}
}
EOF

  for pid in $(lsof -ti "tcp:$PORT" -sTCP:LISTEN); do
    if ps -o comm= -p "$pid" | grep -q fakeapi; then
      kill "$pid"
    else
      echo "port $PORT is held by $(ps -o comm= -p "$pid"), not a fake. Stop it, or change PORT." >&2
      return 1
    fi
  done
  tries=0
  while lsof -ti "tcp:$PORT" -sTCP:LISTEN >/dev/null; do
    tries=$((tries + 1)); [ "$tries" -lt 50 ] || { echo "the old fake did not let go of port $PORT" >&2; return 1; }
    sleep 0.1
  done

  rm -f "$DEV/fake-secrets.env"
  "$DEV/fakeapi" -addr "127.0.0.1:$PORT" -secrets-file "$DEV/fake-secrets.env" >"$DEV/fakeapi.log" 2>&1 &
  tries=0
  until curl -s -o /dev/null "http://127.0.0.1:$PORT/"; do
    tries=$((tries + 1)); [ "$tries" -lt 50 ] || { echo "the fake did not answer; its log:" >&2; cat "$DEV/fakeapi.log" >&2; return 1; }
    sleep 0.2
  done
  echo "fake is up on http://127.0.0.1:$PORT (log: $DEV/fakeapi.log)"

  export TF_CLI_CONFIG_FILE="$DEV/terraform.rc"
  export K0RDENT_ENDPOINT="http://127.0.0.1:$PORT"
  export K0RDENT_REGION=us-texas-1
  export K0RDENT_PROJECT=demo
  . "$DEV/fake-secrets.env"
  export K0RDENT_CLIENT_ID="$FAKE_CLIENT_ID"
  export K0RDENT_CLIENT_SECRET="$FAKE_CLIENT_SECRET"
}
k0rdent_fake_up
```

- The fake makes a new credential each time it starts, in
  `fake-secrets.env`, which only you can read.
- Every `terraform` command warns that a provider override is in effect.
  That's expected: it's your build.
- **Don't run `terraform init`.** There's nothing to install.

| What you see | What to do |
| --- | --- |
| `Unable to open CLI configuration file`, then `Inconsistent dependency lock file` | The build is gone, or your shell points at an old one. Run `k0rdent_fake_up` |
| `port 18901 is held by ..., not a fake` | Stop that process, or change `PORT` in the block |
| A plan wants to recreate everything | Restarting the fake forgets its objects. Run `rm -f examples/live/terraform.tfstate*` to start clean |

## 2. Say what to build, and where

The run's settings go in `TF_VAR_` variables:

```sh
export TF_VAR_cluster_name=tf-walkthrough
export TF_VAR_capabilities='{vpc_and_subnets=true,security_groups=true,kubeconfig=true}'
```

Then pick one of these two, not both.

**A new project.** The run creates one, builds in it, and deletes it last.
The credential has to be scoped to the organization, with
`organizations-admin` on it: `organizations-member` can create a project but
not delete it.

```sh
export TF_VAR_organization_id=org-demo
```

**A project you already have.** A wrong id stops the plan before anything is
created.

```sh
export TF_VAR_project_id=demo
```

With neither, it uses `K0RDENT_PROJECT`. To switch later, destroy first:
moving a cluster to another project replaces it.

## 3. Apply

```sh
terraform -chdir=examples/live apply -auto-approve
```

## 4. Read the scorecard

```sh
terraform -chdir=examples/live output scorecard
terraform -chdir=examples/live output project
```

## 5. Plan again

```sh
terraform -chdir=examples/live plan -detailed-exitcode
```

**Exit code 0 is the result you want.** 2 means the provider wants to change
something it just created, which is a bug.

In the `cluster_type_round_trip` output, `written` is what you asked for and
`returned` is the version the platform pinned. They differ, and the plan is
still empty.

## 6. Destroy

```sh
terraform -chdir=examples/live destroy -auto-approve
```

A project the run created goes last.

## Next

- Turn on more of the run with `TF_VAR_capabilities`. The hardware
  walkthrough lists what each part needs on a real platform.
- Run the same steps on real hardware with
  [the hardware walkthrough](walkthrough-hardware.md).
