terraform {
  required_providers {
    k0rdent = {
      source = "k0rdent/k0rdent"
    }
  }
}

# Every argument is optional and falls back to an environment variable, so a
# pipeline can use an empty block.
provider "k0rdent" {
  # Scheme and host only, without /v1. Falls back to K0RDENT_ENDPOINT.
  endpoint = "https://api.k0rdent.ai"

  # List regions with `k0rdent_regions`. Falls back to K0RDENT_REGION.
  region = "us-texas-1"

  # The project's id. Falls back to K0RDENT_PROJECT.
  project = "cp-production"

  # Tags for every cluster, network, security group and VPC peering. A
  # resource's own `tags` win over these. A VPC peering only gets them when
  # it's created.
  default_tags = {
    cost-center = "ml-platform"
    managed-by  = "terraform"
  }

  # There's no credential argument. Set a service account in the environment:
  #
  #   export K0RDENT_CLIENT_ID='sa-terraform@example.iam'
  #   read -rs K0RDENT_CLIENT_SECRET && export K0RDENT_CLIENT_SECRET
  #
  # read -rs keeps the secret out of your shell history.
  #
  # K0RDENT_API_KEY also works, with a warning: it stops working when the
  # person who created it leaves.
}
