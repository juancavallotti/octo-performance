# Infrastructure

Ephemeral GCP hardware for one campaign: `terraform apply`, run, collect, `terraform
destroy`. Nothing here is meant to outlive a campaign, which is why the host fingerprint
captured into each result has to be complete enough that the numbers stay interpretable
after the machines are gone.

Not yet written — this lands in Phase 3, after `perf` can run a campaign locally against
a fake subject. The shape it will take:

| Machine | Type | Runs |
|---|---|---|
| runner | `c4-standard-16` | `perf`, `k6`, and a sampler watching the runner itself |
| subject | `c4-standard-8` | `octo`, supervised by `perf-agent` |
| deps | `c4-standard-4` | Postgres and the slow backend, for the scenarios that need them |

The runner is deliberately twice the subject, and it samples its own CPU so that
"the generator had headroom" is evidence rather than an assumption. That is the whole
reason this repository is moving off a laptop: see
[docs/LEARNINGS.md](../../docs/LEARNINGS.md) (L1).

## How the binaries arrive

Both startup scripts download the same pinned release and verify it against the
published checksums:

```hcl
variable "harness_version" {
  description = "Harness release to install on both machines, e.g. 0.3.1."
  type        = string
}
```

- runner installs `perf` plus k6
- subject installs `perf-agent`
- `perf` refuses to drive an agent whose protocol version differs from its own, so a
  half-applied upgrade fails at the handshake rather than producing plausible data

There is no pushing binaries over SSH and no embedding one in the other. The version
under test lives in the campaign spec; the version of the *harness* lives here, in the
same file that decides the machine shape — which is the right place for it, because
changing either one changes what the numbers mean.

## Networking

- custom VPC; runner, subject and deps reach each other on internal addresses only, so
  load never leaves the subnet
- subject and deps get no external IP; operator access is via IAP TCP forwarding
- firewall opens 8080 (the workload), 39999 (the runtime's admin port), 5432 and 9090
  (dependencies) to the subnet only, and SSH to the IAP range only

## Arms share a subject

Interleaving requires it. Giving each arm its own VM would make the rotation impossible
and reintroduce exactly the confound the rotation exists to remove — every arm would
then carry its machine's idiosyncrasies as if they were properties of the version.
