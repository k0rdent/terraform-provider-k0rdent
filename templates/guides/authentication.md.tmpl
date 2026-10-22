---
page_title: "Authenticating a pipeline"
subcategory: ""
description: |-
  Which credential to ask for, and how to supply it without writing it down.
---

# Authenticating a pipeline

The provider reads its credential from the environment and never writes it
anywhere.

## Ask for a service account, not an API key

- A **service account** is its own identity with its own roles. It keeps
  working when people leave, and it can hold several credentials at once, so
  you can rotate without downtime.
- An **API key** acts as the person who created it, limited to their current
  roles. When they leave or their roles change, the pipeline breaks. The
  provider warns when you use one.

The platform's audit log records each call under the identity you
authenticate as, so a service account also gives you a readable audit trail.

## Getting one

The provider can't create a credential. Ask a tenant administrator to create
a service account and give you its client id and client secret. The secret is
shown only once.

## Supplying it

The provider block has no credential argument. Set it in the environment:

```bash
export K0RDENT_CLIENT_ID='sa-terraform-provider@vendor.iam'
printf 'client secret: ' >&2
read -rs K0RDENT_CLIENT_SECRET && export K0RDENT_CLIENT_SECRET && echo >&2
```

`read -rs` keeps the secret out of your shell history.

```hcl
provider "k0rdent" {
  endpoint = "https://api.k0rdent.ai"
  region   = "us-texas-1"
  project  = "cp-production"
  # No credential here. It comes from the environment.
}
```

- An API key goes in `K0RDENT_API_KEY` instead.
- In a pipeline, keep both values in the runner's secret store and expose them
  as environment variables to the Terraform step.
- Never put them in a tfvars file, a variable file or version control.

## Rotation

1. Create a new credential on the service account.
2. Update the pipeline's secret.
3. Confirm a run works.
4. Revoke the old credential.

No configuration changes. Tokens are renewed during a run, so a long cluster
create doesn't fail when one expires.

If a token request fails with 401, check the client id and secret.

## What the credential can reach

Exactly what its roles allow through the API, nothing more. To limit what a
pipeline can do, limit the service account's roles.

## Nothing is written to state

- No credential, certificate or kubeconfig is saved in Terraform state.
- The `k0rdent_cluster_kubeconfig` ephemeral resource returns the kubeconfig
  for the length of the run only.
- The `k0rdent_cluster_kubeconfig` data source returns metadata only, never
  the kubeconfig itself.
- The provider writes no files of its own and doesn't log headers or request
  bodies.
