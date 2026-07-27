# Infrastructure

Ephemeral GCP hardware for one campaign: `terraform apply`, run, collect, `terraform
destroy`. Nothing here is meant to outlive a campaign, which is why the host fingerprint
captured into each result has to be complete enough that the numbers stay interpretable
after the machines are gone.

| Machine | Type | Runs | External IP |
|---|---|---|---|
| runner | `c4-standard-16` | `perf`, `k6`, and a sampler watching the runner itself | yes |
| subject | `c4-standard-8` | `octo`, and nothing else | no |
| deps | `c4-standard-4` | Postgres and the slow backend, for scenarios 003 and 005 | no |

The runner is deliberately twice the subject, and it samples its own CPU so that "the
generator had headroom" is evidence rather than an assumption. That is the whole reason
this repository is moving off a laptop: the same binary produced 15,996 req/s and
6,939 req/s on consecutive days, with `vus_max` at 1,600 and 7,113. See
[docs/LEARNINGS.md](../../docs/LEARNINGS.md) (L1).

## Using it

```sh
cd infra/terraform
terraform init
terraform apply \
  -var project=my-project \
  -var 'ssh_public_key=<paste ~/.ssh/id_ed25519.pub>'

terraform output -json inventory > ../out/inventory.json
terraform output run_command      # the invocation these machines were built for
```

Stage the release binaries on the subject as `/srv/perf/octo-versions/octo-<version>`,
then from the runner:

```sh
perf run --campaign campaigns/regression-050-vs-060.yaml \
  --subject-ssh perf@10.20.0.3 --subject 10.20.0.3 \
  --subject-dir /srv/perf --versions /srv/perf/octo-versions \
  --deps-ssh perf@10.20.0.4 --deps 10.20.0.4
```

And when it is finished, `terraform destroy`. The campaign directory is the artifact; the
machines are not.

**Prerequisites.** `gcloud auth application-default login` and a project id. Neither is
configured in this checkout — the first `apply` will say so.

## The subject runs nothing but the subject

No harness, no agent, no load generator. `perf` drives it over SSH and samples its
`/proc` through the same multiplexed connection, one round trip per second.

That is a change from the original plan, which had a `perf-agent` on the subject speaking
a versioned NDJSON protocol. The agent buys one thing the SSH path cannot — exact
whole-lifetime `rusage`, by being octo's parent — and costs a second binary whose version
can disagree with the runner's. A mismatched sampler does not fail; it produces plausible
data. The cost denominator comes from differencing octo's cumulative CPU counter instead,
which is the same arithmetic the local sampler does and is accurate to the sampling
interval.

## Networking

- custom VPC; runner, subject and deps reach each other on internal addresses only, so
  load never leaves the subnet
- subject and deps get no external IP; operator access is via IAP TCP forwarding, and
  Cloud NAT gives them egress for `apt` at boot
- firewall opens 8080 (the workload), 39999 (the runtime's admin port), 5432 and 9090
  (dependencies) and 22 to the subnet only, and SSH from the IAP range only
- ports are listed individually rather than opened as a range, because "what else was
  talking to the subject" is not a question a finished campaign can answer afterwards

Loopback is not a network hop and understates the cost of one. A VPC subnet is an
ordinary hop, which is what a real deployment has and what a report can describe without a
caveat.

## Arms share a subject

Interleaving requires it. Giving each arm its own VM would make the rotation impossible
and reintroduce exactly the confound the rotation exists to remove — every arm would then
carry its machine's idiosyncrasies as if they were properties of the version.

## What the startup scripts do, and why

- **Both load-bearing machines raise the ephemeral port range and the descriptor limit.**
  A generator at ten thousand requests a second opens ten thousand sockets a second; the
  defaults run out, and the resulting connection errors read in a k6 summary as a server
  refusing load.
- **The subject disables unattended upgrades.** A package upgrade partway through a
  campaign changes the machine under measurement, and the interleaved ordering cannot
  balance out a confound that happens once.
- **Nothing is preemptible.** An instance that disappears at cell forty does not cost a
  cell, it costs the comparison: the remaining arms would have run on a different machine.
- **`block-project-ssh-keys` is on.** The key in `ssh_public_key` is the only way in. A
  stray project key is a second door and a second thing to account for when a campaign's
  numbers are questioned.
