# M3.5 trial deploy: /healthz on ECS-on-EC2 behind an ALB, then destroy.
# See docs/decisions/021-trial-deploy.md. State is local and git-ignored:
# this stack lives for an hour, so a remote backend (S3) waits for M6.

terraform {
  required_version = ">= 1.16"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.66"
    }
  }
}

provider "aws" {
  region = var.region

  # Every resource carries these, so leftovers after `terraform destroy`
  # can be found with one tag query.
  default_tags {
    tags = {
      Project   = "auction-engine"
      Milestone = "m3.5-trial"
      ManagedBy = "terraform"
    }
  }
}
