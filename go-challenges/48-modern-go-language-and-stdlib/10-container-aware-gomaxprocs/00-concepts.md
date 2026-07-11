# Container-Aware GOMAXPROCS — Concepts

Every Go service you have ever shipped to Kubernetes or ECS was, until Go 1.25,
silently mis-sized. `runtime.NumCPU()` reports the host's logical CPUs (intersected
with the process CPU affinity mask), sampled once at startup. It knows nothing
about the cgroup CFS quota that actually limits how much CPU time your pod may
consume. A binary with a 2-CPU limit that lands on a 64-core node saw
`NumCPU() == 64`, so the runtime set `GOMAXPROCS = 64`, spun up 64 Ps, burned its
entire per-period quota in the first few milliseconds of each 100 ms window, and
then got CFS-throttled — frozen — for the rest of every period. That shows up as
p99/p999 latency cliffs, GC-assist storms, and stop-the-world-adjacent stalls that
look like a GC bug but are really scheduler starvation. Go 1.25 makes the runtime
cgroup-aware by default on Linux, re-reads the quota periodically, and adds
`runtime.SetDefaultGOMAXPROCS`. This file is the model you need to own that
behavior in production and to reason through the three exercises that follow.

## Concepts

### Why NumCPU is the wrong sizing signal in a container

`runtime.NumCPU()` returns the number of logical CPUs *usable* by the process: the
host cores intersected with the CPU affinity mask, sampled once at startup and
never updated. That is a hardware-and-affinity fact. It has nothing to do with the
cgroup CPU *bandwidth* limit, which is the mechanism Kubernetes and ECS actually
use to cap a container. The two numbers routinely disagree by an order of
magnitude: a pod with a `limits.cpu: "2"` on a 64-core node has `NumCPU() == 64`
but is only ever allowed two cores' worth of CPU time. Sizing anything —
`GOMAXPROCS`, a worker pool, a batch fan-out, GC buffers — off `NumCPU()` in that
pod over-provisions parallelism by 32x.

### CFS throttling: how the freeze happens

The Linux Completely Fair Scheduler enforces a cgroup CPU limit with two numbers:
a `quota` and a `period` (default period 100 ms). The cgroup may use `quota`
microseconds of CPU time per `period`. A `limits.cpu: "2"` becomes
`quota = 200000, period = 100000` — 200 ms of CPU per 100 ms of wall time, i.e.
two cores. If `GOMAXPROCS` is far larger than the quota allows, the runtime keeps
many Ps runnable, they collectively consume the whole 200 ms quota in the first
few milliseconds of the period, and then *every* runnable goroutine is throttled
until the next period boundary. The application is frozen for the remainder of each
100 ms window. Symptoms: latency cliffs at p99/p999, apparent multi-hundred-
millisecond GC pauses, GC-assist storms, and timer/mutex jitter that reads like a
leak but is the scheduler being starved of quota.

### The Go 1.25 default formula

On Linux, the default `GOMAXPROCS` is the minimum of three constraints:

```text
GOMAXPROCS = min( logical CPU count,
                  CPU affinity mask count,
                  adjusted cgroup limit )

adjusted cgroup limit = max(2, ceil(cgroup quota / cgroup period))
```

The runtime traverses the whole cgroup hierarchy and takes the tightest effective
limit. Three details matter and are exactly what a hand-rolled parser gets wrong:

- Fractional quotas **round up**. A 2.5-core limit (`250000 100000`) yields
  `ceil(2.5) = 3`, not 2. Truncating under-provisions parallelism.
- There is a **floor of 2** on the cgroup component. A 0.5-core limit
  (`50000 100000`) computes `ceil(0.5) = 1`, which is raised to 2, because
  `GOMAXPROCS = 1` disables scheduler parallelism entirely (GC workers would then
  pause the application). The floor applies to the cgroup limit only — it is taken
  *before* the `min` with the logical count, so a genuine single-CPU machine
  (`logical == 1`) still yields `GOMAXPROCS = 1`.
- It is a **min**, so the cgroup limit can only lower parallelism, never raise it
  above what the hardware/affinity actually provides.

### GOMAXPROCS keys off the limit, not the request

In Kubernetes a pod may declare `requests.cpu` and `limits.cpu`. Only the *limit*
creates a CFS quota. The *request* influences scheduling and the CPU weight
(`cpu.weight` / shares) but sets no quota at all. So a pod with requests but no
limits has no cgroup bandwidth cap, and the runtime falls back to the node's
logical CPU count — exactly the pre-1.25 over-provisioning. This is the single
most common production misconfiguration the feature exposes: teams set requests
"for the scheduler" and expect `GOMAXPROCS` to follow, but it does not.

### Dynamic updates and the cached file descriptors

The runtime re-reads the logical CPU count, the affinity mask, and the cgroup
quota up to once per second (less often when the process is idle) and adjusts
`GOMAXPROCS` live. A mid-life CPU-limit change — a Vertical Pod Autoscaler
resize, a manual `kubectl edit`, an ECS task update — now takes effect without a
restart. To read updated cgroup limits cheaply, the runtime keeps cached file
descriptors to the cgroup files open for the whole process lifetime. The practical
consequence for your own code: capacity is *dynamic*. Reading `GOMAXPROCS(0)` (or
`NumCPU()`) once at startup and caching it forever drifts after a live limit
change.

### The override trap and SetDefaultGOMAXPROCS

Setting the `GOMAXPROCS` environment variable to a positive value, or calling
`runtime.GOMAXPROCS(n)` with `n >= 1`, disables **both** cgroup-awareness and the
periodic auto-updates — the value is pinned and the runtime stops re-reading
anything. This is easy to do by accident: `GOMAXPROCS` in a Deployment manifest,
or a stray `runtime.GOMAXPROCS(runtime.NumCPU())` in `main`, quietly defeats the
whole 1.25 feature. `runtime.SetDefaultGOMAXPROCS()` (added in Go 1.25) is the
only in-process way to re-enable the auto-updating container-aware default after
it was disabled, and it doubles as a way to force an immediate refresh when you
know the quota just changed. Note also the query/mutate asymmetry:
`runtime.GOMAXPROCS(0)` (any `n < 1`) reads the current value without changing it;
`runtime.GOMAXPROCS(n)` with `n >= 1` mutates it *and* disables auto-updates.
Using a positive argument to "check" the value is a bug.

### cgroup v1 vs v2 file layout

Portable code (and the runtime) must read both cgroup versions:

- **cgroup v2** exposes a single `cpu.max` file containing two space-separated
  fields, `"<quota> <period>"` in microseconds, e.g. `200000 100000`. The literal
  `max` in the quota field means "no limit".
- **cgroup v1** splits it across `cpu.cfs_quota_us` and `cpu.cfs_period_us`. A
  quota of `-1` means "no limit". The period defaults to `100000` when absent.

Both layouts still exist in the field — many managed and older-kernel hosts run
v1. A parser that handles only `cpu.max` and mis-defaults on a v1 host is a real
bug, and so is one that panics on a missing file: a missing file simply means "no
limit here", which is a fall-back to the logical CPU count, not an error.

### GODEBUG gating tied to the go.mod language version

Two GODEBUG settings control the feature: `containermaxprocs` (cgroup awareness)
and `updatemaxprocs` (periodic updates). Both default **on** for language version
1.25+ and **off** (`=0`) for 1.24 and below. The gate is the `go` directive in
`go.mod`, not the toolchain: bumping `go 1.24` to `go 1.25` silently flips the
defaults and can change `GOMAXPROCS` in a container. That makes the language
bump a *runtime behavior* change to review during an upgrade, not merely a
compiler version bump. To keep the old behavior explicitly, set
`GODEBUG=containermaxprocs=0,updatemaxprocs=0`.

### Relationship to go.uber.org/automaxprocs

Before 1.25, teams got this behavior from `go.uber.org/automaxprocs`, whose blank
import called `runtime.GOMAXPROCS` at init to match the cgroup quota. The runtime
feature is the upstreamed equivalent. On Go 1.25+ that library is largely
redundant, and worse: its `runtime.GOMAXPROCS` call now trips the override trap
and *disables* the auto-updating default it was meant to emulate. Migration to
1.25 means *removing* automaxprocs, not stacking it on top of the runtime default.

### GC pacing interaction

The garbage collector's background and assist CPU budget is proportional to
`GOMAXPROCS`. An inflated `GOMAXPROCS` therefore told the GC to schedule more
assist work than the quota could ever pay for, compounding the throttling. Getting
`GOMAXPROCS` right also right-sizes GC CPU usage. (`GOMEMLIMIT` is the memory-side
analogue and is configured separately; `runtime/debug.SetGCPercent` remains the
other GC-pacing knob.)

## Common Mistakes

### Sizing pools off NumCPU instead of GOMAXPROCS(0)

Wrong: `workers := runtime.NumCPU()` for a worker pool, connection pool, batch
fan-out, or GC-sensitive buffer count. `NumCPU` ignores the cgroup quota.

Fix: derive capacity from `runtime.GOMAXPROCS(0)`, which reflects the runtime's
container-aware effective parallelism, and re-read it if you care about live
limit changes.

### Setting the GOMAXPROCS env var "to be safe"

Wrong: adding `GOMAXPROCS` to the Deployment manifest to "pin it down". That pins
the value and disables both cgroup-awareness and the once-per-second updates,
defeating the entire feature.

Fix: leave it unset on Go 1.25+ and let the runtime read the cgroup limit. If you
must override in code, restore the default with `runtime.SetDefaultGOMAXPROCS()`.

### Assuming CPU requests drive GOMAXPROCS

Wrong: expecting `requests.cpu` to size `GOMAXPROCS`. Requests create no quota.

Fix: set a CPU *limit* if you want the runtime to size to it; a requests-only pod
falls back to the node core count.

### Rounding the quota down

Wrong: `int(quota / period)`, truncating 2.5 cores to 2.

Fix: `int(math.Ceil(quota / period))` — the runtime rounds up.

### Forgetting the floor of 2

Wrong: a 500m (0.5-core) limit yielding `GOMAXPROCS = 1`.

Fix: apply `max(2, ceil(...))` to the cgroup component. The floor does not apply
when the machine itself has fewer than two logical CPUs.

### Caching the value forever

Wrong: reading the cgroup file or `GOMAXPROCS(0)` once at startup and never again.
Limits can change; the runtime re-reads every second.

Fix: re-read on an interval (or call `SetDefaultGOMAXPROCS` on a known change) if
your capacity must track the limit.

### Expecting identical behavior after a go.mod bump

Wrong: bumping the `go` directive to 1.25 and assuming runtime CPU sizing is
unchanged. The `containermaxprocs`/`updatemaxprocs` defaults flip with the
language version.

Fix: review `GOMAXPROCS` in containers as part of the upgrade; pin the old
behavior with `GODEBUG` if you are not ready.

### Keeping automaxprocs on Go 1.25+

Wrong: leaving `go.uber.org/automaxprocs` imported. Its `runtime.GOMAXPROCS`
call disables the new auto-updating default.

Fix: remove the dependency and rely on the runtime default.

### Using a positive argument to query GOMAXPROCS

Wrong: `runtime.GOMAXPROCS(runtime.NumCPU())` to "read" the value — it mutates
and disables auto-updates.

Fix: `runtime.GOMAXPROCS(0)` (any `n < 1`) reads without changing.

### Running GOMAXPROCS-mutating tests with t.Parallel

Wrong: marking a test that calls `runtime.GOMAXPROCS(n)` parallel. It is
process-global state; parallel mutation makes assertions flaky.

Fix: snapshot a baseline and restore it with `t.Cleanup`; never call
`t.Parallel` in such a test.

### Handling only cgroup v2

Wrong: reading only `cpu.max` and crashing or mis-defaulting on a v1 host.

Fix: read `cpu.max`, and on `fs.ErrNotExist` fall back to `cpu.cfs_quota_us` /
`cpu.cfs_period_us`, treating any missing file as "no limit".

Next: [01-cgroup-cpu-limit-parser.md](01-cgroup-cpu-limit-parser.md)
