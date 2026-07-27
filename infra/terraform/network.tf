# The network exists to make one claim defensible: load between the runner and the
# subject never leaves this subnet.
#
# The old lab could not make that claim at all — it ran both on one machine over
# loopback, which is not a network hop and understates the cost of one. A VPC subnet is
# an ordinary hop, which is what a real deployment has and what a report can describe
# without a caveat.

locals {
  subnet_cidr = "10.20.0.0/24"
  # Google's fixed source range for Identity-Aware Proxy TCP forwarding. Operator SSH
  # arrives from here and nowhere else, which is what lets the subject and deps hosts
  # have no external address at all.
  iap_range = "35.235.240.0/20"

  tag_runner  = "${var.prefix}-runner"
  tag_subject = "${var.prefix}-subject"
  tag_deps    = "${var.prefix}-deps"
}

resource "google_compute_network" "lab" {
  name = "${var.prefix}-net"
  # Custom, not auto: an auto-mode network creates a subnet in every region, and this
  # campaign wants exactly one place for traffic to be.
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "lab" {
  name          = "${var.prefix}-subnet"
  ip_cidr_range = local.subnet_cidr
  region        = var.region
  network       = google_compute_network.lab.id
}

# Operator access, and only through IAP. The subject and the deps host have no external
# IP, so this is the only route to them.
resource "google_compute_firewall" "ssh_via_iap" {
  name          = "${var.prefix}-ssh-iap"
  network       = google_compute_network.lab.name
  source_ranges = [local.iap_range]
  target_tags   = [local.tag_runner, local.tag_subject, local.tag_deps]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# Everything the campaign itself needs, restricted to the subnet.
#
# Listed by port with a comment each, rather than opened as a range, because an open
# port on the subject is a way for something other than the load generator to reach the
# runtime — and "what else was talking to it" is not a question a finished campaign can
# answer retrospectively.
resource "google_compute_firewall" "internal" {
  name          = "${var.prefix}-internal"
  network       = google_compute_network.lab.name
  source_ranges = [local.subnet_cidr]
  target_tags   = [local.tag_runner, local.tag_subject, local.tag_deps]

  allow {
    protocol = "tcp"
    ports = [
      "8080",  # the workload
      "39999", # the runtime's admin port: healthz, readyz, metrics
      # Postgres for scenario 003, and 55432 is not a typo. The scenario publishes the
      # container on that port and its PG_DSN names it, so 5432 — which is what was
      # opened here, and what anyone writing this list from memory would open — is a
      # port nothing has ever listened on.
      #
      # The cost of the mismatch is not a clear error. octo starts, cannot reach its
      # database, and answers /readyz with 503 "starting" until the harness gives up
      # sixty seconds later, so the campaign reports the runtime as slow to start rather
      # than the network as closed. It failed at cell 5 of 14, twenty minutes in.
      #
      # This list duplicates knowledge that lives in the scenarios. Anything added to
      # scenarios/*/setup.sh that listens on a new port has to be added here too.
      "55432",
      "9090", # the slow backend, scenario 005
      "22",   # the harness drives the subject over ssh from the runner
    ]
  }
}

# Egress to the internet for the machines with no external address, so apt and the
# release downloads work at boot. Removed once images are baked, if start-up time ever
# becomes the bottleneck.
resource "google_compute_router" "lab" {
  name    = "${var.prefix}-router"
  region  = var.region
  network = google_compute_network.lab.id
}

resource "google_compute_router_nat" "lab" {
  name                               = "${var.prefix}-nat"
  router                             = google_compute_router.lab.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"
}
