# The inventory is the only coupling between Terraform and Go.
#
# One file, written by `terraform output -json inventory`, read by the operator to build
# a `perf run` invocation. Nothing in the harness parses Terraform state and nothing here
# knows what a campaign is — which is what lets a campaign run against hardware that was
# not provisioned by this module at all.

output "runner_external_ip" {
  description = "Where an operator connects to drive the campaign."
  value       = google_compute_instance.runner.network_interface[0].access_config[0].nat_ip
}

output "subject_internal_ip" {
  description = "The address the runner offers load to. Internal only, by design."
  value       = google_compute_instance.subject.network_interface[0].network_ip
}

output "deps_internal_ip" {
  description = "The address the subject reaches Postgres and the backend on."
  value       = var.enable_deps ? google_compute_instance.deps[0].network_interface[0].network_ip : ""
}

output "inventory" {
  description = <<-EOT
    Everything a `perf run` invocation needs, plus the machine shapes.

    The shapes are here and not only in the Terraform state because every number a
    campaign publishes is a property of them, and the state is destroyed with the
    infrastructure. A result directory that outlives its hardware has to carry enough
    to stay interpretable.
  EOT
  value = {
    project = var.project
    zone    = var.zone

    runner = {
      name         = google_compute_instance.runner.name
      machine_type = var.runner_machine_type
      external_ip  = google_compute_instance.runner.network_interface[0].access_config[0].nat_ip
      internal_ip  = google_compute_instance.runner.network_interface[0].network_ip
    }
    subject = {
      name         = google_compute_instance.subject.name
      machine_type = var.subject_machine_type
      internal_ip  = google_compute_instance.subject.network_interface[0].network_ip
      ssh          = "${var.ssh_user}@${google_compute_instance.subject.network_interface[0].network_ip}"
      dir          = "/srv/perf"
      versions_dir = "/srv/perf/octo-versions"
    }
    deps = var.enable_deps ? {
      name         = google_compute_instance.deps[0].name
      machine_type = var.deps_machine_type
      internal_ip  = google_compute_instance.deps[0].network_interface[0].network_ip
      ssh          = "${var.ssh_user}@${google_compute_instance.deps[0].network_interface[0].network_ip}"
    } : null

    image           = var.image
    boot_disk_gb    = var.boot_disk_gb
    k6_version      = var.k6_version
    harness_version = var.harness_version
  }
}

output "run_command" {
  description = "The invocation these machines were built for, ready to paste on the runner."
  value = join(" ", compact([
    "perf run --campaign campaigns/<name>.yaml",
    "--subject-ssh ${var.ssh_user}@${google_compute_instance.subject.network_interface[0].network_ip}",
    "--subject ${google_compute_instance.subject.network_interface[0].network_ip}",
    "--subject-dir /srv/perf",
    "--versions /srv/perf/octo-versions",
    var.enable_deps ? "--deps-ssh ${var.ssh_user}@${google_compute_instance.deps[0].network_interface[0].network_ip}" : "",
    var.enable_deps ? "--deps ${google_compute_instance.deps[0].network_interface[0].network_ip}" : "",
  ]))
}
