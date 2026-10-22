---
page_title: "Running from remote state"
subcategory: ""
description: |-
  Keeping state in a remote backend, so any runner can plan and apply.
---

# Running from remote state

The provider works with any Terraform backend and keeps nothing on the machine
that runs it. One runner can apply, and another can plan or destroy later from
the same state, with only the configuration and the credential.

## What a runner keeps

- The provider writes no files: no token cache, kubeconfig or lock.
- It reads settings only from the `provider` block and `K0RDENT_*` environment
  variables.
- `.terraform` and `.terraform.lock.hcl` are Terraform's, and `terraform init`
  rebuilds them on a fresh runner.
- If you need the kubeconfig on disk, write it yourself from the
  `k0rdent_cluster_kubeconfig` ephemeral resource.

## Configure a backend

Use a backend that locks. The provider doesn't stop two runs against the same
state. Keep backend secrets in the environment, like the provider's
credential.

An S3-compatible bucket:

```terraform
terraform {
  required_providers {
    k0rdent = {
      source = "k0rdent/k0rdent"
    }
  }

  backend "s3" {
    bucket       = "platform-terraform-state"
    key          = "clusters/training/terraform.tfstate"
    region       = "us-east-1"
    use_lockfile = true
  }
}

provider "k0rdent" {}
```

An `http` backend:

```terraform
terraform {
  backend "http" {
    address        = "https://state.example.internal/clusters/training"
    lock_address   = "https://state.example.internal/clusters/training"
    unlock_address = "https://state.example.internal/clusters/training"
  }
}
```

## A pipeline step

```bash
export K0RDENT_ENDPOINT=https://api.example.internal
export K0RDENT_REGION=us-texas-1
export K0RDENT_PROJECT=ml-training
export K0RDENT_CLIENT_ID="$PIPELINE_SA_CLIENT_ID"
export K0RDENT_CLIENT_SECRET="$PIPELINE_SA_CLIENT_SECRET"

terraform init -input=false
terraform plan -input=false -out=tfplan
terraform apply -input=false tfplan
```

Protect the saved plan like state: it holds variable values and prior state.
It never holds the provider's credential.
