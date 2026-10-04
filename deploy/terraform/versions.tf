terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source = "hashicorp/aws"
      # 6.28 is the first version with `invoked_via_function_url` on
      # aws_lambda_permission, which public Function URLs need (see lambda.tf).
      version = "~> 6.28"
    }
  }

  # No remote backend: state stays in a local file (git-ignored). It holds
  # no secrets, because the API key is never managed by Terraform.
}

provider "aws" {
  region = var.region

  # Every resource is tagged, so the demo's cost and resources are easy to
  # find in the console.
  default_tags {
    tags = {
      Project   = var.name
      ManagedBy = "terraform"
    }
  }
}
