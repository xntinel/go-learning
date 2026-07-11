# 6. Traffic Management — Concepts

Traffic management is the hardest piece of a service-mesh data plane to get right because several independent mechanisms — a circuit breaker, retries with backoff, a retry budget, and per-route timeouts — must compose without creating new failure modes. Naive retries amplify failures during outages (retry storms). Timeouts applied per-attempt instead of per-call waste upstream capacity on work the caller has already abandoned. Circuit breakers that open too aggressively starve healthy upstreams; those that recover without a probe window extend outages. This file is the conceptual foundation. Read it once and you will have everything you need to reason through each exercise, which builds the policies piece by piece as independent, self-contained Go modules: a three-state circuit breaker, a retry policy with exponential backoff and full jitter, a sliding-window retry budget, a per-route timeout with deadline propagation, and finally the `http.RoundTripper` that composes all four in the correct order.

## Concepts

### The Three-State Circuit Breaker

The circuit breaker is a state machine with three states:

- Closed (normal): every request passes through. Failures are counted. When the failure count reaches the configured threshold, the circuit trips to Open.
- Open (failing): requests are rejected immediately with a 503-equivalent error, without contacting the upstream. The circuit stays Open for a configurable cooldown.
- Half-Open (recovery probe): after the cooldown expires, exactly one probe request is allowed through. If the probe succeeds, the circuit closes. If it fails, the circuit re-opens and the cooldown restarts.

The half-open state is critical. Without it, a circuit that opens during an outage stays open forever. With it, the circuit tests recovery automatically — the probe is the circuit's heartbeat. Allowing more than one probe in half-open creates a thundering-herd problem: concurrent probes can all succeed against a partially recovered upstream and cause a second overload. The implementation guards this with a single `halfOpenSent` boolean that admits exactly one caller until `Record` reports the probe's outcome.

A subtlety in the failure counter is that it counts *consecutive* failures in the closed state: a single success resets the count to zero. This is deliberate. A circuit should trip only on a sustained run of failures, not on a failure rate accumulated over a long healthy period. An upstream that fails one request in fifty is not broken; an upstream that fails five in a row almost certainly is.

### Retry Storms and Retry Budgets

When an upstream fails, every client that sees a 503 immediately retries. If each client allows three retries, the upstream receives up to 4× its normal load — exactly when it can least handle it. This is a retry storm, and it is the primary cause of cascading failures in distributed systems.

A retry budget caps the fraction of total traffic that may be retries, measured over a sliding time window. For example, a 20% budget on a service receiving 100 req/s allows at most 20 retry req/s. When the budget is exhausted, further retries are blocked and the caller receives the error immediately — which is better than amplifying an ongoing outage.

The budget is a traffic-level concept, not a per-request concept. For individual requests in isolation the budget approaches 0% (one original immediately followed by one retry pushes the ratio to 100%), so budgets are effective only at steady traffic rates. At low traffic, the budget acts as no-limit, returning nil whenever there are no originals to measure against.

### Exponential Backoff with Full Jitter

The naive retry loop "wait 100ms, retry" causes synchronized retries: every client that got a 503 at time T waits 100ms and retries at T+100ms, causing a second spike. Exponential backoff — wait `base * 2^attempt` — spreads retries over time, but synchronized clients still produce spikes at each power-of-two boundary.

Full jitter adds a uniformly random component. The AWS analysis (see Resources) shows this minimises total work under load because clients back off into different windows. The formula used here is `d + uniform(0, d * jitterFactor)` where `d` is the capped exponential, giving a range of `[d, d*(1+jitterFactor)]`. Each backoff waits in a `select` against `ctx.Done()` so a cancelled context (from a deadline or explicit cancel) aborts the wait immediately rather than sleeping out the full duration.

Only idempotent methods are retriable. A GET, HEAD, or OPTIONS that returns 503 can be retried safely because repeating it has no side effect. A POST that returns 503 may already have committed its write before the response was lost; retrying it duplicates the effect. The retry policy encodes this as an allow-list of methods, and `IsRetriable` requires both a retriable method and a retriable status code.

### Deadline Propagation Across Service Calls

A per-route timeout on the proxy — "forward this request with a 2s deadline" — is not the same as propagating the original caller's deadline. If a client issues a request with a 500ms deadline and the proxy applies a 2s timeout, the proxy will still attempt the upstream call for 2s after the client has given up. This wastes upstream capacity and upstream database connections.

The correct pattern is deadline propagation: the proxy reads the caller's deadline from a header (for example `X-Request-Deadline` as a Unix nanosecond timestamp), computes the remaining time, and uses `min(remaining, routeTimeout)` as the effective timeout for the upstream call. If the caller's deadline has already passed, the header is ignored and the configured timeout (if any) applies, so a stale or malformed header never produces a zero or negative timeout.

### Composition Order

The policies must be applied in this exact order:

1. Circuit breaker check: reject immediately if the circuit is open. No context is created, no upstream connection is opened.
2. Timeout wrapping: create a `context.WithTimeout` that governs the entire retry loop, not individual attempts. If a timeout wraps each attempt independently, the last attempt can still consume a full timeout duration after earlier ones have already consumed most of the budget.
3. Retry loop: attempt the upstream call; if the response is retriable, check the budget, wait for backoff, and attempt again — but always re-check `ctx.Done()` before each attempt.

A context that wraps the entire retry loop also means the backoff waits contribute to the deadline. This is the correct behaviour: a 2s deadline should not allow three retries of 500ms each plus three backoff waits. The transport records every response outcome in the circuit breaker, including retriable ones, so the circuit can trip even while the retry loop is still working — the two policies run independently and each must see every outcome.

## Common Mistakes

### Wrapping the Timeout Around Each Attempt Instead of the Loop

Wrong: creating `context.WithTimeout` inside the retry loop, giving each attempt its own full timeout budget. Three retries each with a 2s timeout can consume 6s total plus backoff waits, so a caller with a 3s deadline receives a response after their deadline has long passed. Fix: create one `context.WithTimeout` before the loop. The timeout governs the entire call including all backoff waits.

### Skipping the Circuit Breaker Record on a Retriable Failure

Wrong: skipping `cb.Record(false)` when the response is retriable, reasoning that "the upstream will be retried, so it shouldn't count yet". The circuit breaker then never opens during a partial outage. The retry policy and the circuit breaker run independently; each should see every outcome so the circuit can trip when repeated retriable responses indicate a real problem. Fix: always call `cb.Record(resp.StatusCode < 500)` for every response, including retriable ones.

### Applying a Retry Budget Per-Request Instead of Per-Window

Wrong: resetting the budget counters after each request, so each request sees a fresh 20% budget. Every request then gets up to one retry, which is equivalent to having no budget at all during a widespread outage. Fix: use a sliding time window. The window accumulates originals and retries across many requests; the cap only becomes meaningful at sustained traffic rates.

### Non-Idempotent Methods in the Retriable Set

Wrong: adding POST to the retriable methods because "the upstream might just be temporarily busy". A POST that creates a resource and returns 503 because it timed out internally may have already committed the write; retrying it creates a duplicate. Fix: only idempotent methods (GET, HEAD, OPTIONS, and with care PUT and DELETE) belong in the retriable set. For non-idempotent methods, the caller must handle retries with idempotency keys.

### Using a Retriable Response Body Without Closing It

Wrong: discarding the retriable response and issuing another `RoundTrip` without calling `resp.Body.Close()`. The connection is not returned to the pool; each retry leaks a connection, and under load this exhausts file descriptors. Fix: always close a retriable response body before issuing the next attempt.

### Tripping on a Cumulative Failure Rate Instead of a Consecutive Run

Wrong: incrementing the failure counter on every failure and never resetting it on success, so the circuit trips after N failures spread across thousands of healthy requests. Fix: reset the counter to zero on every success in the closed state, so the threshold measures a sustained run of failures, which is the signal an upstream is actually down.

---

Next: [01-circuit-breaker.md](01-circuit-breaker.md)
