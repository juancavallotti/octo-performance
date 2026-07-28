# Infrastructure

Ephemeral GCP hardware for one campaign: `terraform apply`, run, collect, `terraform
destroy`. Nothing here is meant to outlive a campaign, which is why the host fingerprint
captured into each result has to be complete enough that the numbers stay interpretable
after the machines are gone.

| Machine | Type | Runs | External IP |
|---|---|---|---|
| runner | `c4-standard-16` | `perf`, `k6`, and a sampler watching the runner itself | yes |
| subject | `c4-standard-8` | `octo`, and nothing else | no |
| deps | `n2-standard-4` | Postgres and the slow backend, for scenarios 003 and 005 | no |

The deps host is not a C4, and that is on purpose. GCP meters a `CPUS_PER_VM_FAMILY`
quota per family per region, and a new project gets 24 for C4 — which the runner and
the subject consume exactly, between them. A C4 dependency host makes the request 28 and
`terraform apply` fails partway through, after building the two machines that fit.
Holding the dependencies in another family costs nothing methodologically: that machine
is identical across every arm, so it cannot confound a comparison. Only its *colocation
with the subject* ever could, and moving it off the subject is what fixed that.

The runner is deliberately twice the subject, and it samples its own CPU so that "the
generator had headroom" is evidence rather than an assumption. That is the whole reason
this repository is moving off a laptop: the same binary produced 15,996 req/s and
6,939 req/s on consecutive days, with `vus_max` at 1,600 and 7,113. See
[docs/LEARNINGS.md](../../docs/LEARNINGS.md) (L1).

## Using it

```sh
cd infra/terraform
cp terraform.tfvars.example terraform.tfvars   # set project and ssh_public_key
terraform init
terraform apply

terraform output -json inventory > ../out/inventory.json
terraform output run_command      # the invocation these machines were built for
```

`terraform.tfvars` is gitignored, so the project id and key stay local; the `.example`
alongside it is the shared template and documents every knob.

The key is named by path, not pasted: `ssh_public_key_file = "~/.ssh/octo-perf-lab.pub"`.
Note that this is a bare string and not `file("~/.ssh/octo-perf-lab.pub")`, which is the
natural thing to write and fails before any variable is read — a `.tfvars` is data rather
than configuration and may call no function:

```
Error: Function calls not allowed
  on terraform.tfvars line 19:
  ssh_public_key = file("~/.ssh/octo-perf-lab.pub")
```

So the tfvars supplies the path and the module does the reading, which is also where `~`
gets expanded. Pointing at the private key by leaving off `.pub` is rejected at plan time
rather than published into instance metadata. `ssh_public_key` still takes a literal key
for the case where there is no file to point at; exactly one of the two is set.

## When a zone will not give you the machines

Two failures look alike here and only one is fixed by moving zone.

The family may be absent from the region entirely, which GCP reports as a quota of zero —
reading like a limits problem while actually being an availability one, so an increase
request will not help:

```
Error: Quota 'CPUS_PER_VM_FAMILY' exceeded. Limit: 0.0 in region us-west1
       dimensions = map[region:us-west1 vm_family:C4]
```

Or the family is there and the zone is simply out of capacity, which surfaces only at
apply time:

```
Error: The zone 'projects/.../zones/us-central1-a' does not have enough resources
       available to fulfil the request.
```

The second is not a configuration problem. Move `zone` to another one in the same region
and leave `region` alone.

Read which machine failed before moving, though, because one zone has to satisfy two
families at once and they run out independently:

```
A n2-standard-4 VM instance is currently unavailable in the us-central1-b zone.
  with google_compute_instance.deps[0],
```

That is the deps host, and moving `zone` for it would relocate a runner and subject that
were placeable — for a machine whose shape is the one thing here that cannot confound a
comparison. It is identical across every arm, so change `deps_machine_type` instead and
stay put: `n2d-standard-4` keeps a fixed AMD platform, `e2-standard-4` is the widest
availability. Both take `pd-balanced` and neither touches the C4 quota, which is the pair
of constraints that matters. (`n4-standard-4` does not — N4 requires hyperdisk, so it
means changing `deps_boot_disk_type` too.) Move `zone` when it is the runner or the
subject that cannot be placed.

The first failure needs region and zone to move together, because the subnet is regional
and an instance cannot use a subnet from another region — a mismatched pair is rejected
at plan time rather than halfway through an apply:

```
Error: Invalid value for variable
  var.region is "us-central1"
  var.zone is "us-east4-c"
```

Before moving, confirm the target carries **both** families — the runner and subject are
C4, the deps host deliberately is not:

```sh
gcloud compute machine-types list \
  --filter="name=(c4-standard-8,c4-standard-16,n2-standard-4) AND zone~us-central1" \
  --format="value(zone,name)" | sort
```

That listing answers the first failure and not the second: the catalog will list a type in
a zone that cannot currently build one. It is a necessary check, not a sufficient one. All
three types are presently offered in `us-central1-a`, `-b`, `-c` and `-f`.

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
