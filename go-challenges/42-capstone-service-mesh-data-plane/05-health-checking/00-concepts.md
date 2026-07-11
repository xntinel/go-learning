# 5. Health Checking — Concepts

A data plane that cannot tell a working backend from a broken one routes traffic into black holes. Envoy answers this with two complementary detectors, and this lesson rebuilds both as independent Go modules: an active health checker that probes each backend on a fixed interval, and a passive outlier detector that watches the outcomes of real requests and ejects a backend whose error rate spikes. The hard parts are not the probes themselves but the state machine that prevents flapping, the never-eject-the-last-backend invariant, the exponential cooldown that throttles repeated ejections, and the lock ordering that keeps both detectors correct under concurrent access. Read this file once and you will have every idea you need to work through the exercises, each of which builds one of these pieces in isolation.

## Concepts

### Active vs. Passive Detection

Active health checks send synthetic probes — a TCP connect, or an HTTP GET to `/healthz` — at a configurable interval, whether or not real traffic is flowing. They catch hard failures (a crashed process, a closed port) within one interval. Their blind spot is the backend that answers `/healthz` with 200 while returning 500 on the application path: the probe sees health the users do not.

Passive outlier detection closes that blind spot by observing real traffic. Every request the data plane forwards reports its outcome — success or failure — back to the detector. When the error rate inside a sliding time window crosses a threshold, the backend is ejected from the routing pool for a cooldown period. Passive detection catches the application-layer failures active probes miss; active probes, in turn, are what bring an ejected backend back once it actually recovers.

The two are designed to cooperate, not to compete. The integration seam with the load balancer (lesson 04) is two methods and one channel: the balancer asks `IsHealthy(addr)` before routing and subscribes to a `Notifications` channel that emits a `StateChange` on every transition, so routing decisions update within one check interval.

### The Three-State Machine

Each backend carries one of three states:

```text
         consecutiveFailures >= UnhealthyThreshold (active probe)
Healthy  ────────────────────────────────────────>  Unhealthy
         <────────────────────────────────────────
         consecutiveSuccesses >= HealthyThreshold (active probe)

         error rate > ErrorRateThreshold (passive)
Healthy/Unhealthy ──────────────────────────────>  Ejected
                  <──────────────────────────────
                   cooldown expires (-> Unhealthy), then active probes heal
```

`Healthy` means in the pool with passing probes. `Unhealthy` means removed from the pool because active probes are failing. `Ejected` means passively removed because the error rate spiked, and on a timed cooldown. Only the healthy state takes traffic.

The thresholds exist to prevent *flapping*. A single dropped packet must not mark a backend unhealthy; `UnhealthyThreshold` consecutive probe failures must. Symmetrically, a recovering backend must show `HealthyThreshold` consecutive successes before it re-enters the pool. Without these counters a backend with intermittent loss would oscillate in and out of rotation many times a second, and every oscillation is a burst of connection churn and reordered traffic.

### Passive Detection: the Sliding Error-Rate Window

The passive path keeps, per backend, a slice of recent timestamped outcomes. On each new outcome it appends the result, trims everything older than `ErrorRateWindow`, and computes `failures / total` over what remains. If that rate exceeds `ErrorRateThreshold` the backend is a candidate for ejection.

Two guards keep the rate meaningful. First, a minimum sample count: ejecting on one failure out of one request (rate = 1.0) would punish a cold backend for a single blip, so the detector waits until it has at least `MinSamples` observations before trusting the rate. Envoy's equivalent default is five. Second, after an ejection the window is reset to empty; carrying the pre-ejection failures across the cooldown would re-eject the backend on its very first new request and make recovery nearly impossible. The cooldown is the punishment; the stale window must not compound it.

### The Never-Eject-the-Last-Backend Invariant

If every backend but one is already unhealthy or ejected, the detector must not eject that survivor — even if its error rate is 1.0. Routing to a degraded backend is bad; routing to *nothing* is worse, because the caller then has no target at all. So before committing an ejection the detector counts healthy peers and refuses if there are none.

This check is inherently racy. Two goroutines recording outcomes for two different backends could each see the *other* as still healthy and both decide to eject, leaving zero healthy backends. The defense is to hold the map-level read lock while counting peers, so the set of states being counted cannot change underneath the count, and to re-check the backend's own state after re-acquiring its lock to commit (a concurrent path may have ejected it in between).

### Exponential Ejection Cooldown

A backend that is ejected, restored, and immediately ejected again should not bounce on a fixed timer. Each consecutive ejection lengthens the cooldown:

```text
cooldown = min(BaseEjectionTime * 2^(n-1), MaxEjectionTime)
```

The first ejection holds the backend out for `BaseEjectionTime`; the second for twice that; and so on until the cap `MaxEjectionTime`. The cap matters for two reasons: it bounds how long a recovered backend stays sidelined, and it keeps the doubling from overflowing the duration. A correct implementation never doubles a value that is already past half the cap, which makes the computation overflow-safe for any ejection count.

The exponential schedule is deterministic, which is fine for a single detector but causes a thundering herd when many independent clients back off in lockstep against the same recovering backend. Production backoff therefore adds *jitter*: pick a random duration in a band below the computed cooldown so retries spread out in time rather than synchronizing. AWS's "Exponential Backoff And Jitter" is the canonical treatment; the cooldown exercise builds both the deterministic schedule and a jittered variant.

### Restoration After Cooldown

Ejection is timed, so something must fire when the timer expires. The detector schedules a restoration callback with `time.AfterFunc(cooldown, ...)`; when it runs, it moves the backend from `Ejected` to `Unhealthy` (not straight to `Healthy`). The deliberate choice of `Unhealthy` as the landing state means a restored backend does not immediately take traffic — it must first earn its way back through `HealthyThreshold` consecutive *active* probe successes. Passive ejection lets the active checker, the system that can actually confirm the backend works, make the final call on readmission.

### Lock Ordering

Two locks protect the state: a `sync.RWMutex` on the `Checker` guarding the address-to-entry map, and a `sync.Mutex` per entry guarding that entry's counters and outcome slice. Reads of the map vastly outnumber writes to it, and most operations need only the map read lock plus a single entry lock, so no global lock becomes a bottleneck.

The invariant that keeps this deadlock-free is a fixed acquisition order: always take the map lock before any entry lock, never the reverse. The danger spot is the never-eject-last check, which itself iterates the map and locks each entry. If it were called while already holding one entry's lock, two goroutines could each hold one entry lock and block trying to acquire the other's — a classic deadlock. The fix is to release the entry lock before the peer count, then re-acquire and re-check before committing the ejection. The window this opens is narrow and its worst outcome — two backends ejected at once — is self-correcting through the restoration timers.

## Common Mistakes

### Holding an Entry Lock While Counting Healthy Peers

Wrong: calling the peer-counting helper while still holding `e.mu`. That helper walks the entries map and locks each entry in turn; if two goroutines do it simultaneously, one holding `e_a.mu` reaching for `e_b.mu` and the other the reverse, the program deadlocks. Fix: release `e.mu` first, count peers, then re-acquire and re-check `e.state` before committing — the re-check prevents a double ejection from a path that ejected the backend in the gap.

### Using a Single Mutex for Everything

Wrong: one `sync.Mutex` covering both the map and every entry's counters. Under load `RecordOutcome` runs thousands of times per second across all backends, and a global lock serializes every one of them. Fix: a read-write lock on the map plus a per-entry mutex, so concurrent outcomes on different backends never contend.

### Not Resetting the Outcome Window After Ejection

Wrong: leaving the outcome slice intact when a backend is ejected. When the cooldown ends, the pre-ejection failures are still in the window and the very first new request can push the rate back over the threshold, re-ejecting instantly. Fix: clear the window at ejection time so the recovered backend starts with a clean slate.

### Ejecting on Too Few Samples

Wrong: computing the error rate from one or two observations, so a single early failure reads as a 100% error rate and ejects a cold backend. Fix: require at least `MinSamples` observations before the rate is trusted.

### Restoring Straight to Healthy

Wrong: moving an ejected backend directly back to `Healthy` when the cooldown expires, putting it back in rotation before anything has confirmed it works. Fix: restore to `Unhealthy` and let the active probes promote it only after `HealthyThreshold` consecutive successes.

---

Next: [01-active-state-machine.md](01-active-state-machine.md)
