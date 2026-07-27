# The three machines.
#
# Everything they share is in locals rather than repeated, because a difference between
# the runner and the subject that nobody intended is a difference that ends up in a
# result — and the fingerprint would faithfully record it without anyone noticing it was
# an accident.

locals {
  ssh_keys = "${var.ssh_user}:${trimspace(var.ssh_public_key)}"

  common_metadata = {
    ssh-keys = local.ssh_keys
    # Project-wide keys are blocked so the key above is the only way in. A stray
    # project key is a second door and a second thing to account for when a campaign's
    # numbers are questioned.
    block-project-ssh-keys = "TRUE"
    enable-oslogin         = "FALSE"
  }
}

resource "google_compute_instance" "runner" {
  name         = "${var.prefix}-runner"
  machine_type = var.runner_machine_type
  zone         = var.zone
  tags         = [local.tag_runner]
  labels       = merge(var.labels, { role = "runner" })

  boot_disk {
    initialize_params {
      image = var.image
      size  = var.boot_disk_gb
      type  = "hyperdisk-balanced"
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.lab.self_link
    # The runner keeps an external address so an operator can copy a finished campaign
    # directory off it without a tunnel. The subject and deps hosts do not.
    access_config {}
  }

  metadata = merge(local.common_metadata, {
    startup-script = templatefile("${path.module}/scripts/runner.sh", {
      k6_version = var.k6_version
      ssh_user   = var.ssh_user
    })
  })

  # A campaign is hours of sustained load. A preemptible or spot instance that
  # disappears at cell forty does not cost a cell, it costs the comparison: the
  # remaining arms would then have run on a different machine.
  scheduling {
    preemptible        = false
    automatic_restart  = true
    provisioning_model = "STANDARD"
  }

  service_account {
    scopes = ["cloud-platform"]
  }
}

resource "google_compute_instance" "subject" {
  name         = "${var.prefix}-subject"
  machine_type = var.subject_machine_type
  zone         = var.zone
  tags         = [local.tag_subject]
  labels       = merge(var.labels, { role = "subject" })

  boot_disk {
    initialize_params {
      image = var.image
      size  = var.boot_disk_gb
      type  = "hyperdisk-balanced"
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.lab.self_link
    # No access_config: no external IP. The runner reaches it on the subnet and an
    # operator reaches it through IAP. Nothing else on the internet can.
  }

  metadata = merge(local.common_metadata, {
    startup-script = templatefile("${path.module}/scripts/subject.sh", {
      ssh_user     = var.ssh_user
      versions     = join(" ", var.octo_versions)
      url_template = var.octo_release_url_template
    })
  })

  scheduling {
    preemptible        = false
    automatic_restart  = true
    provisioning_model = "STANDARD"
  }

  service_account {
    scopes = ["cloud-platform"]
  }
}

resource "google_compute_instance" "deps" {
  count = var.enable_deps ? 1 : 0

  name         = "${var.prefix}-deps"
  machine_type = var.deps_machine_type
  zone         = var.zone
  tags         = [local.tag_deps]
  labels       = merge(var.labels, { role = "deps" })

  boot_disk {
    initialize_params {
      image = var.image
      size  = var.boot_disk_gb
      type  = "hyperdisk-balanced"
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.lab.self_link
  }

  metadata = merge(local.common_metadata, {
    startup-script = templatefile("${path.module}/scripts/deps.sh", {
      ssh_user = var.ssh_user
    })
  })

  scheduling {
    preemptible        = false
    automatic_restart  = true
    provisioning_model = "STANDARD"
  }

  service_account {
    scopes = ["cloud-platform"]
  }
}
