variable "project" {
  description = "GCP project id."
  type        = string
}

variable "region" {
  description = "Region for the network and the subnet."
  type        = string
  default     = "us-central1"
}

variable "zone" {
  description = <<-EOT
    Zone for every machine. One zone, not several: a campaign's whole point is that its
    cells differ only in the arm under test, and cross-zone latency between the runner
    and the subject would be an extra millisecond on every request that no gate could
    subtract.
  EOT
  type        = string
  default     = "us-central1-a"
}

variable "prefix" {
  description = "Name prefix for every resource, so two campaigns can coexist."
  type        = string
  default     = "octoperf"
}

variable "subject_machine_type" {
  description = <<-EOT
    The machine under test. Everything published is a property of this shape as much as
    of the runtime, which is why it is recorded in every result's fingerprint.
  EOT
  type        = string
  default     = "c4-standard-8"
}

variable "runner_machine_type" {
  description = <<-EOT
    The load generator, deliberately twice the subject.

    This is the single most important line in this file. The old lab ran k6 beside the
    runtime and produced 15,996 req/s and 6,939 req/s from a byte-identical binary a day
    apart — k6 grew its virtual-user pool, the extra goroutines took cores from the
    server, the server slowed, and the pool grew further. A feedback loop, not a constant
    tax, and no gate can subtract it after the fact.

    Twice the subject is not a guess at sufficiency; it is headroom wide enough that the
    generator-headroom gate has something to confirm rather than something to excuse.
  EOT
  type        = string
  default     = "c4-standard-16"
}

variable "deps_machine_type" {
  description = <<-EOT
    Postgres and the slow backend, for scenarios 003 and 005.

    On its own machine because the alternative is what METHODOLOGY.md calls "not merely
    indicative — misleading": the old lab reached both through host.docker.internal, so
    the dependency shared the subject's cores and the measurement folded the database's
    CPU into the runtime's.
  EOT
  type        = string
  default     = "c4-standard-4"
}

variable "enable_deps" {
  description = <<-EOT
    Whether to create the dependency machine. Only scenarios 003 and 005 need it; a
    campaign that runs neither should not pay for it.
  EOT
  type        = bool
  default     = true
}

variable "image" {
  description = "Boot image for every machine. Pinned to a family, recorded per host."
  type        = string
  default     = "debian-cloud/debian-12"
}

variable "boot_disk_gb" {
  description = <<-EOT
    Boot disk size. Larger than the machines need, because on the balanced disk type
    throughput scales with size, and a subject whose disk throttles mid-campaign
    produces a latency artifact indistinguishable from a regression.
  EOT
  type        = number
  default     = 100
}

variable "ssh_user" {
  description = "Login the harness reaches the subject and deps hosts as."
  type        = string
  default     = "perf"
}

variable "ssh_public_key" {
  description = <<-EOT
    Public key installed on every machine, e.g. file("~/.ssh/id_ed25519.pub").

    Project-level keys are deliberately blocked on these instances, so this is the only
    key that works — an operator's stray project key would otherwise be a second way in
    and a second thing to explain when a campaign's numbers are questioned.
  EOT
  type        = string
}

variable "octo_versions" {
  description = <<-EOT
    Octo releases to place on the subject, e.g. ["0.5.0", "0.6.0"].

    They are staged at boot rather than pushed per cell, because pushing a binary is a
    difference between the first cell of a campaign and the rest of them.
  EOT
  type        = list(string)
  default     = []
}

variable "octo_release_url_template" {
  description = <<-EOT
    Where a release is fetched from. %s is the version.

    Empty means the operator stages the binaries themselves — which is the honest
    default here, because this lab's subject is an internal runtime with no public
    release URL, and inventing one that 404s at boot would fail the campaign at its
    first cell rather than at apply time.
  EOT
  type        = string
  default     = ""
}

variable "k6_version" {
  description = "k6 release installed on the runner. Recorded with every series it produces."
  type        = string
  default     = "1.3.0"
}

variable "labels" {
  description = "Labels applied to every resource, for cost attribution and cleanup."
  type        = map(string)
  default     = { purpose = "octo-performance" }
}
