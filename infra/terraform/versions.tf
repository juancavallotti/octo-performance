terraform {
  required_version = ">= 1.6"

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
