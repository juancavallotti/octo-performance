variable "project" {
  description = "GCP project id."
  type        = string
}

variable "region" {
  description = <<-EOT
    Region for the network and the subnet.

    us-central1 and not us-west1, and the reason is worth stating because it is not a
    preference. C4 is not offered in us-west1 at all, and GCP expresses that as a quota
    of zero rather than as an unknown machine type:

      Error: Quota 'CPUS_PER_VM_FAMILY' exceeded. Limit: 0.0 in region us-west1
             dimensions = map[region:us-west1 vm_family:C4]

    A limit of 0 reads like a quota problem and is really an availability one, so raising
    it is not possible and requesting an increase will not help. Before moving this to a
    region you prefer, confirm the family is there:

      gcloud compute machine-types list \
        --filter="name=c4-standard-8 AND zone~<region>" --format="value(zone)"

    Empty output means pick another region, or move runner and subject to a family that
    region does have — keeping the 2:1 ratio between them, which is the part that matters.
  EOT
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

    Deliberately NOT a C4, and that is the one thing to preserve if you change it.

    GCP bills a CPUS_PER_VM_FAMILY quota per family per region, and a new project gets
    24 for C4. The runner and the subject are what the campaign is actually about — one
    must not bottleneck, the other is the thing being measured — and at c4-standard-16
    plus c4-standard-8 they consume exactly the whole allowance. A c4-standard-4 for the
    dependencies pushes the request to 28 and `terraform apply` dies partway through,
    having already built the two machines that fit:

      Error: Quota 'CPUS_PER_VM_FAMILY' exceeded. Limit: 24.0 ... vm_family:C4

    Putting the dependency host in a different family takes it out of that budget
    entirely. It costs nothing methodologically: this machine is held constant across
    every arm, so its performance is not a variable the comparison can be confounded by
    — only its *colocation with the subject* ever was, and that is what moving it off
    the subject already fixed.

    Note that C4 is then consumed exactly to the limit, so two campaigns cannot run
    concurrently in one region on the default quota. Raise CPUS_PER_VM_FAMILY, or give
    the second campaign a different region, rather than shrinking the runner.
  EOT
  type        = string
  default     = "n2-standard-4"
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

variable "boot_disk_type" {
  description = <<-EOT
    Boot disk type for the runner and the subject.

    Disk types are not portable across machine families, and the coupling is enforced at
    create time rather than at plan time — so a mismatch costs an apply, not a plan:

      Error 400: hyperdisk-balanced disk type cannot be used by n2-standard-4 machine type

    hyperdisk-balanced goes with the C4 defaults above. Change this whenever you change
    subject_machine_type or runner_machine_type to another family; pd-balanced is the
    portable choice.
  EOT
  type        = string
  default     = "hyperdisk-balanced"
}

variable "deps_boot_disk_type" {
  description = <<-EOT
    Boot disk type for the dependency host, which is its own variable precisely because
    that host is deliberately in a different machine family — see deps_machine_type.

    pd-balanced rather than hyperdisk-balanced: N2 does not accept hyperdisk, and this
    disk carries a Postgres nobody is measuring. The two machines whose disk throughput
    could show up in a result are the runner and the subject, and they keep hyperdisk.
  EOT
  type        = string
  default     = "pd-balanced"
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

variable "harness_version" {
  description = <<-EOT
    Lab release to install, e.g. "0.3.1" for tag harness/v0.3.1. Empty stages nothing and
    the operator unpacks the archive by hand.

    One version, one archive: `perf`, `labbackend`, every scenario and every campaign
    spec come out of the same tarball, so the harness and the scenarios it runs cannot
    be from different commits. That is not fastidiousness — a scenario that loads is not
    the same as a scenario the harness was written against, and the disagreement has no
    symptom until a number is wrong.

    The version of the thing *under test* lives in the campaign spec. The version of the
    *measuring instrument* lives here, beside the machine shape, which is the right place
    for it: changing either changes what the numbers mean.
  EOT
  type        = string
  default     = ""
}

variable "harness_repo" {
  description = "GitHub repository the release archive is fetched from."
  type        = string
  default     = "juancavallotti/octo-performance"
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
