terraform {
  # 1.9 and not 1.6, because var.zone's validation refers to var.region — cross-variable
  # validation is a 1.9 feature, and on an older CLI the check does not degrade to a
  # warning, it fails to parse. The alternative was a lifecycle precondition on the
  # subnet, which reports the same mistake one stage later and further from its cause.
  required_version = ">= 1.9"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }

  # Local state, deliberately. The infrastructure is ephemeral and single-operator:
  # it is applied, used for one campaign, and destroyed. A remote backend would add a
  # bucket to provision before the first apply and a lock to break after the first
  # interrupted one, in exchange for collaboration on state that never outlives an
  # afternoon.
}

provider "google" {
  project = var.project
  region  = var.region
  zone    = var.zone
}
