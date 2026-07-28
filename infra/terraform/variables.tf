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

    Expect to change this. A zone that offers a machine family is not the same as a zone
    that can build one right now, and the difference only surfaces at apply time:

      Error: The zone 'projects/.../zones/us-central1-a' does not have enough resources
             available to fulfil the request.

    That is capacity, not configuration — nothing here is wrong and nothing needs fixing
    beyond moving. Try another zone in the same region, in which case only this variable
    changes and the subnet stays where it is. Distinguish it from the region-level
    failure documented above, where the family is absent entirely and moving zone within
    the region will not help.

    Whichever zone you pick has to carry both families, because the deps host is
    deliberately not a C4:

      gcloud compute machine-types list \
        --filter="name=(c4-standard-8,c4-standard-16,n2-standard-4) AND zone~<region>" \
        --format="value(zone,name)" | sort

    This must be a zone of var.region. The subnet is regional and an instance cannot use
    a subnet from another region, so the two settings move together; the validation below
    turns that into a plan-time error rather than a half-built campaign.
  EOT
  type        = string
  default     = "us-central1-a"

  validation {
    # Not a regex on the shape of a zone name — the point is agreement with the region,
    # and a zone that merely looks well-formed is exactly the input that gets this wrong.
    condition     = startswith(var.zone, "${var.region}-")
    error_message = "zone must be in region ${var.region}, e.g. \"${var.region}-a\". The subnet is regional and instances cannot use a subnet from another region, so region and zone are changed together."
  }
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

variable "ssh_public_key_file" {
  description = <<-EOT
    Path to the public key to install, e.g. "~/.ssh/octo-perf-lab.pub". Read here rather
    than pasted, so terraform.tfvars names the key instead of carrying a copy of it that
    can drift from the file it came from.

    This is the variable to set. It exists because the obvious thing to write in a
    terraform.tfvars — the same expression that works everywhere else in Terraform —
    is rejected before any variable is read:

      Error: Function calls not allowed
        on terraform.tfvars line 19:
        ssh_public_key = file("~/.ssh/octo-perf-lab.pub")

    A .tfvars file is data, not configuration, so no function may be called in one. The
    file() call belongs in the module instead, which is what happens below: the tfvars
    supplies a path, the module resolves it. Set ssh_public_key directly only when the
    key is a literal you already have in hand rather than a file on disk.

    ~ is expanded, which file() alone does not do. A relative path is resolved against
    the directory terraform runs in, not the one your shell is in.
  EOT
  type        = string
  default     = ""

  validation {
    # Exactly one, not a precedence rule. Two sources for the single key that opens
    # these machines is one more than can be reasoned about later, and a silent winner
    # is how the key on the box stops matching the key in the file.
    condition     = (var.ssh_public_key_file == "") != (var.ssh_public_key == "")
    error_message = "Set exactly one of ssh_public_key_file (a path, the usual choice) or ssh_public_key (a literal key). Both are currently set, or neither is."
  }

  validation {
    condition     = var.ssh_public_key_file == "" || can(file(pathexpand(var.ssh_public_key_file)))
    error_message = "ssh_public_key_file is \"${var.ssh_public_key_file}\", which cannot be read. ~ is expanded; a relative path resolves against the directory terraform runs in."
  }

  validation {
    # Catches the paste that omits .pub. A private key is the same shape of line noise
    # to the eye, and GCP accepts one into instance metadata without complaint — the
    # mistake would surface as instances nobody can log into, having already published
    # the key that was supposed to stay on the operator's laptop.
    condition = (
      var.ssh_public_key_file == "" ||
      !can(file(pathexpand(var.ssh_public_key_file))) ||
      can(regex("^(ssh-|ecdsa-|sk-)", trimspace(file(pathexpand(var.ssh_public_key_file)))))
    )
    error_message = "ssh_public_key_file is \"${var.ssh_public_key_file}\", which does not look like an OpenSSH public key. Check for a missing .pub — that path is the private key."
  }
}

variable "ssh_public_key" {
  description = <<-EOT
    The public key itself, as a literal, for the case where it is not a file on disk.
    Prefer ssh_public_key_file; see there for why a path cannot be resolved in a .tfvars.

    Project-level keys are deliberately blocked on these instances, so whichever of the
    two is set is the only key that works — an operator's stray project key would
    otherwise be a second way in and a second thing to explain when a campaign's numbers
    are questioned.
  EOT
  type        = string
  default     = ""
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
    Where a release is fetched from. %s is the version, without a leading v.

    This defaulted to empty on the claim that the runtime had no public release URL.
    That was simply false — every version is published with assets for every platform —
    and the cost of the error was staging two binaries onto the subject by hand, twice,
    while believing that was the designed arrangement.

    The archive is a tarball containing `octo`, which subject.sh unpacks. Note that the
    platform is baked into this string: a subject outside the x86 default needs the
    matching URL, and getting it wrong stages a binary that cannot execute.
  EOT
  type        = string
  default     = "https://github.com/juancavallotti/octo/releases/download/v%s/octo_linux_amd64.tar.gz"
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
  description = <<-EOT
    k6 release installed on the runner. Recorded with every series it produces.

    2.x and not 1.x, because that is what the parsers are tested against: the harness
    suite runs a real k6 through LookPath rather than a fixture, so whatever an operator
    has installed is what k6_e2e_test.go and campaign_test.go actually exercise — 2.1.0
    at the time of writing. A runner on 1.3.0 would be the only place the summary format
    was never checked.

    That value was this variable's default from the beginning and was never a measured
    choice. It was also never installed: the template passed it to a script that ran
    `apt-get install -y k6` and took whatever was newest, so 1.3.0 has no runtime history
    in this lab at all and pinning to it would move the runner backwards across a major
    version onto untested ground.

    2.0.0 rather than 2.1.0 because Grafana's deb repository has not published the latter
    — `apt-cache madison k6` on the runner is the list this must be chosen from, and an
    absent version now fails the boot rather than silently installing another one. The
    laptop and the runner are then a minor version apart, which is worth knowing and is
    not the same order of risk as a major.
  EOT
  type        = string
  default     = "2.0.0"
}

variable "labels" {
  description = "Labels applied to every resource, for cost attribution and cleanup."
  type        = map(string)
  default     = { purpose = "octo-performance" }
}
