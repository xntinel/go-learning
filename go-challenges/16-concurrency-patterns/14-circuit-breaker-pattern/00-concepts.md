# Circuit Breaker Pattern — Concepts

A circuit breaker sits between a caller and a remote dependency — an HTTP service, a database, a queue — and watches it fail. When the dependency fails often enough, the breaker "trips": it stops forwarding calls and instead fails them instantly, locally, without touching the network. After a cooldown it lets a single probe through; if the probe succeeds the breaker closes and traffic resumes, and if it fails the breaker re-trips. The pattern comes from Michael Nygard's *Release It!* and is the load-bearing reliability primitive in `sony/gobreaker`, resilience4j, Hystrix, Envoy, and every service mesh. This file is the conceptual foundation: read it once and you have everything you need to reason through the exercises, which build the pattern up from a consecutive-failure baseline to a failure-rate window with an injectable clock, and finally to a breaker composed with retry and per-attempt timeout.

## Concepts

### The Failure You Are Defending Against

A remote dependency that has started failing is not a neutral event you can simply retry your way through. A slow or dead service ties up a caller's goroutines, connections, and memory while they wait for timeouts. Under load those resources are finite, so a single failing downstream can exhaust the caller's pool and take the caller down too — the failure propagates *upstream*, the opposite direction from the request. This is a cascading failure, and the classic accelerant is the retry: every caller, seeing an error, immediately tries again, multiplying the load on a dependency that is already on its knees. The breaker's job is to convert a slow, resource-consuming, retry-amplified failure into a fast, cheap, local one, and to do it automatically the instant the dependency's error rate crosses a line.

### Three States

A breaker is a small state machine with exactly three states.

```text
            failures cross threshold
   CLOSED ───────────────────────────▶ OPEN
     ▲                                   │
     │ probe succeeds                    │ cooldown elapses
     │                                   ▼
     └──────────────────────────── HALF-OPEN
              probe fails (back to OPEN)
```

Closed is normal operation: every call passes through to the dependency, and the breaker merely counts outcomes. Open is the tripped state: every call fails fast with a sentinel error and never reaches the dependency, which is what sheds the load. Half-open is the recovery probe: after the cooldown the breaker permits exactly one call through to test whether the dependency has recovered. Success closes the breaker; failure sends it straight back to open and restarts the cooldown. The half-open state is what distinguishes a circuit breaker from a dumb timeout: it never assumes recovery, it verifies it with a single, controlled request rather than reopening the floodgates.

### State Transitions Must Be Atomic

Every concurrent caller reads the breaker's state on the way in and may write it on the way out, so the state, the failure counters, and the half-open flag are shared mutable data touched from many goroutines at once. A torn read would let two callers observe different states for the same logical instant; an unsynchronized write is a data race that the `-race` detector reports as a real bug, not a flake. The fix is to guard the whole decision — read state, decide, update counters, possibly transition — under a single `sync.Mutex`, and to hold it across both the pre-call admission check and the post-call bookkeeping. An `atomic.Uint32` packing state and counters is an equivalent lock-free design; the exercises use a mutex for clarity, and both are correct. The non-negotiable rule is that no field of the breaker is ever read or written outside the lock.

### Consecutive Failures versus a Failure-Rate Window

There are two common policies for deciding when to trip, and the difference is more than cosmetic. The simplest is a consecutive-failure count: increment a counter on each failure, reset it to zero on each success, and trip when the counter reaches a threshold. Its defining property — and the reason it is safe — is that a single success anywhere in the stream resets the count, so transient blips never accumulate into a trip. It is the right default and it is what the baseline breaker uses.

The consecutive-failure policy has a blind spot, though: a dependency that fails half its calls but never twice in a row will never trip, even though a 50% error rate is a clear outage. Production breakers therefore prefer a failure-rate window: record the outcome of the last N calls (a count-based ring buffer) or of all calls in the last T seconds (a time-based window), and trip when the failure *fraction* across the window crosses a threshold — for example, 50% failures over the last 20 calls. A rate window needs a minimum-calls gate so a single early failure (1 of 1 = 100%) cannot trip it before there is enough evidence. resilience4j and gobreaker both expose exactly this: a sliding window, a minimum number of calls, and a failure-rate threshold. The rate window is strictly more expressive than the consecutive count and is what a senior implementation reaches for.

### The Injectable Clock

A breaker is defined by time: it opens, waits a cooldown, then half-opens. If that timing reads the wall clock directly through `time.Now` and `time.Since`, the only way to test the open-to-half-open transition is to call `time.Sleep` and hope the scheduler cooperates — which makes the test slow, and worse, flaky under the load the `-race` build adds. The senior fix is to make time an injected dependency: the breaker holds a small `Clock` interface (a `Now() time.Time` method, sometimes a `Sleep`), the production path supplies a real clock backed by the standard library, and tests supply a fake clock whose time only moves when the test calls `Advance`. With a fake clock the cooldown boundary is crossed by a function call, not a sleep, so the test is instantaneous, deterministic, and reproducible byte for byte. A fake clock shared across goroutines must itself be safe to call concurrently, so it carries its own mutex. Treat "can I test every time-based transition without sleeping" as the line between a toy breaker and a real one.

### Half-Open Admits Exactly One Probe

When the cooldown elapses, the breaker must let a request through to learn whether the dependency recovered — but exactly one. If two callers race into the half-open state and both fire probes, the breaker has reopened the floodgates it just closed, and a dependency that is still down gets hit by every waiting goroutine at once. The implementation guards the half-open transition with the same mutex and a single `halfOpenInFlight` flag: the first caller to arrive flips the flag and is admitted as the probe, and every other caller that arrives while a probe is outstanding is rejected with a distinct sentinel error. Only when the probe returns and resolves the state does the breaker admit traffic again. The `-race` detector is what pins this contract honest: any path that reads or writes the in-flight flag outside the lock is a real race.

### Composing the Breaker with Retry and Timeout

A breaker is rarely deployed alone; it lives inside a resilience stack with a per-attempt timeout and a retry-with-backoff loop, and the order of composition is what makes the stack safe rather than dangerous. The per-attempt timeout bounds how long any single call may block, so a hung dependency cannot pin a caller's goroutine indefinitely — each attempt runs under its own `context.WithTimeout`, and a deadline expiry counts as a failure the breaker observes. The retry loop turns a transient failure into a success by trying again with exponential backoff.

The trap is retrying *through* the breaker the wrong way. If the retry loop sits outside the breaker and blindly retries every error, then when the breaker is open it will receive the open sentinel, treat it as a retryable error, back off, and try again — turning the breaker's fail-fast into a fail-slow and re-creating the retry storm the breaker exists to suppress. The correct composition makes the retry loop *breaker-aware*: each attempt goes through the breaker, and when the breaker reports it is open, the loop stops retrying immediately and returns the open error rather than backing off and trying again. The result is the property that defines a healthy resilience stack under a downstream outage: the first few calls fail, the breaker opens, and from that instant every caller's retry loop short-circuits on its first attempt — so N callers with M retries each generate a bounded burst of real calls, not N times M of them. The breaker is what makes the difference between a retry policy that helps and one that organizes a denial-of-service attack against your own dependency.

## Common Mistakes

### Counting Cumulative Lifetime Failures

Wrong: increment a single failure counter forever and trip when it reaches a threshold. A long-lived breaker accumulates failures across hours of mostly-healthy traffic and eventually trips on a dependency that is fine. Fix: either reset the consecutive counter to zero on every success, or scope the count to a bounded window. The breaker must trip on *recent* failure density, never on a lifetime total.

### Allowing Concurrent Half-Open Probes

Wrong: read the state in the admission check and write it in the post-call bookkeeping without holding a lock across both, so two callers both observe half-open and both probe. The dependency you just protected gets hit by every waiting goroutine the moment the cooldown ends. Fix: guard the half-open transition with the mutex and a single in-flight flag; admit the first caller as the probe and reject the rest with a distinct sentinel.

### Skipping the Half-Open State

Wrong: jump straight from open back to closed when the cooldown elapses. The breaker reopens to full traffic before it has any evidence the dependency recovered, so a still-broken dependency is immediately flooded and the breaker oscillates. Fix: the cooldown leads to half-open, which admits exactly one probe; only a successful probe closes the breaker.

### Reading or Writing State Outside the Lock

Wrong: peek at the current state without locking — for a metric, a log line, or a fast-path check. The `-race` detector reports it, and the read can tear. Fix: every state and counter access goes through the mutex, including the public method that reports the current state.

### Using the Wall Clock in the Breaker

Wrong: call `time.Now` and `time.Since` directly inside the breaker. Every test of a time-based transition then needs a real `time.Sleep`, which is slow and goes flaky under `-race`. Fix: inject a clock; let production pass a real one and tests pass a fake one whose time advances only on demand.

### Retrying Through an Open Breaker

Wrong: wrap a blind retry-with-backoff loop around the breaker so that the open sentinel is treated as just another retryable error. The loop backs off and retries an open circuit, converting fail-fast into fail-slow and rebuilding the retry storm. Fix: make the retry loop breaker-aware — when the breaker reports open, stop retrying at once and propagate the open error.

Next: [01-circuit-breaker-core.md](01-circuit-breaker-core.md)
