# Retry Patterns With Backoff, Jitter, and Retry Budgets — Concepts

A retry loop is three lines of code and one of the most dangerous things a
backend engineer can ship. The naive version — "on error, try again a few times"
— is not a reliability feature; it is a load multiplier whose default behavior is
to make an outage worse. The moment a dependency starts failing because it is
overloaded, every client that "just retries" triples the traffic aimed at the
thing that is already on fire. That is how a brief latency blip turns into a
metastable outage that does not recover even after the original trigger is gone.

A senior engineer therefore does not "add a retry"; they design a *retry policy*
as a system-safety mechanism, and they can name, for every guardrail in that
policy, the specific failure mode it prevents. The through-line of this lesson is
a single uncomfortable question you must answer before every retry: *who pays for
this retry?* The failing server pays in extra load. The customer pays in a
duplicate charge if the operation was not idempotent. The caller pays in blown
latency if the retries outlast their deadline. Every module here forces you to
reason about who pays, and contrasts the naive version against the production
version that bounds the cost.

## Concepts

### A retry is a load multiplier, not a correctness fix

If a request failed because the network dropped a packet, retrying is close to
free and almost always right. If a request failed because the dependency is
saturated, retrying is the worst possible response: you have just added load to a
system whose problem *is* load. Under sustained overload, N clients each retrying
R times present N·R times the offered load precisely when the dependency has the
least capacity to serve it. Systems in this state exhibit *metastable failure*:
even after the initial trigger disappears, the retry-generated load is enough to
keep the system down. It has found a stable bad equilibrium. The entire
discipline of this lesson exists because a retry that helps one request can, in
aggregate, prevent the whole fleet from recovering.

### Retry only on genuinely transient failures

Classification is the first guardrail, and it must be precise. Retryable: network
timeouts (`net.Error` with `Timeout() == true`), connection resets, and the HTTP
status codes that mean "the server is temporarily unable" — 429 (rate limited),
502, 503, 504. Not retryable: 4xx client errors (400, 401, 403, 404 — the request
is wrong and will be wrong again), validation failures, `context.Canceled`, and
any programmer bug. A 409 Conflict is usually *not* retryable because retrying the
same conflicting write will conflict again. Retrying a permanent error does not
just waste effort; it burns the retry budget that a genuinely transient failure
later in the same call might have needed, and it delays the inevitable error the
caller must eventually see.

One specific trap: `net.Error.Temporary()` is deprecated and must not be used to
decide retryability. Its semantics were never well defined — most "temporary"
errors are really timeouts, and the genuine exceptions are surprising. Use
`Timeout()` plus explicit status-code and sentinel classification. `errors.As`
is the tool that unwraps a deeply wrapped `*net.OpError` or `*url.Error` back to
the `net.Error` interface so you can ask `Timeout()`; `errors.Is` is the tool for
matching a sentinel like a caller-defined `ErrPermanent`.

### Idempotency is a precondition for retrying a write

This is the guardrail whose absence costs real money. When a request fails
*ambiguously* — you sent it, then the connection dropped before the response
arrived — you do not know whether the server processed it. Retrying a read
(GET), or an idempotent write (PUT to a specific key, DELETE) is safe: doing it
twice has the same effect as doing it once. Retrying a *non-idempotent* write —
a POST that charges a card, creates an order, sends an email — after an ambiguous
failure is how systems double-charge customers. The request may have succeeded;
the retry does it again.

The fix is not "never retry writes"; it is to make the write idempotent with a
stable *idempotency key*. The client generates one key per logical operation,
sends the same key on every attempt, and the server deduplicates: the first
request with a given key is processed, and any later request with the same key
returns the stored result of the first. Now the retry is safe because the server,
not the client, guarantees at-most-once execution. The key must be generated
*once* and reused across attempts — regenerating it per attempt defeats the entire
mechanism.

### Exponential backoff, clamped

Backoff spreads the load of a retry over time. The delay before attempt `n` is
`base · factor^n`, clamped to a `cap`. Without exponential growth, retries hammer
the server at a fixed high rate. Without a cap, the delays explode into absurd
multi-minute waits. The clamp is not optional; it is what makes the growth safe.
A typical policy: base 100ms, factor 2, cap a few seconds.

### Jitter breaks synchronization

Exponential backoff alone is not enough, because it is *deterministic*. Picture a
thousand clients that all failed at the same instant because the dependency
briefly went away. With plain exponential backoff they all wait exactly 100ms,
then all retry at exactly the same moment, then all wait exactly 200ms, and retry
together again — a *thundering herd* that slams the dependency in synchronized
waves at exactly the moments it is trying to recover. Jitter adds randomness to
de-synchronize the clients. AWS's well-known analysis ("Exponential Backoff And
Jitter") measured three strategies and found that *full jitter* (sleep uniformly
in `[0, min(cap, base·2^n)]`) and *decorrelated jitter* (`sleep = min(cap, random
in [base, prev·3])`) both reduce total server work and client completion time
versus plain backoff. The counterintuitive result is that adding randomness and
sometimes sleeping *less* still reduces total contention, because it spreads the
load smoothly instead of in spikes.

### The retry budget must be an overall deadline, not just a count

`MaxAttempts` alone is a weak budget. Five attempts with growing backoff against a
slow-but-not-failing dependency can silently consume many seconds and blow the
caller's SLO — the caller wanted an answer in 200ms and your retry loop spent two
seconds getting one. The real budget is the caller's *deadline*, carried on the
`context`. Before every sleep, check `time.Until(deadline)`: if the backoff would
overrun the deadline, do not sleep — return now. And give each individual attempt
a *sub-deadline* with `context.WithTimeout`, so one hung attempt cannot swallow
the entire budget while the others get none. On expiry, `context.Cause` tells you
*why* the deadline fired, which is far more useful in logs than a bare
`context deadline exceeded`.

### Client-side retry budgets cap the storm systemically

A per-request `MaxAttempts` bounds one call, but it does nothing to stop a *fleet*
from amplifying an outage: a million requests each retrying three times is still
three million requests. The systemic defense is a *client-side retry budget* in
the Google SRE style: a token bucket where each *retry* costs a token and
successful *requests* refill it, sized so retries can never exceed a fixed
fraction of traffic (say +10%). When the bucket is empty — which happens exactly
when almost everything is failing, i.e. during an outage — additional retries are
suppressed and the original error is returned immediately. This caps the extra
load a total outage can generate at a bounded ratio, no matter how many clients
are involved. It is the one guardrail that `MaxAttempts` fundamentally cannot
provide.

### Circuit breakers handle sustained outages

Retries and circuit breakers are complementary, not redundant. Retries handle
brief, isolated blips — a single dropped packet. A *circuit breaker* handles a
sustained outage: after a threshold of consecutive failures it trips *open* and
fast-fails every call (returning `ErrCircuitOpen` without even invoking the
operation, so the down dependency receives near-zero traffic), waits a cooldown,
then goes *half-open* and lets a limited number of probe calls through to decide
whether the dependency has recovered (close) or is still down (re-open). Without a
breaker, aggressive retries against a hard-down dependency deliver multiples of
normal load to a service that most needs to be left alone to recover.

### Honor server backpressure: Retry-After

When a server returns 429 or 503 with a `Retry-After` header, it is telling you
*exactly* how long to wait — either an integer number of seconds or an HTTP-date
(parse it with `http.ParseTime`). This is authoritative and should override your
locally computed backoff. Computing your own backoff and ignoring `Retry-After` is
fighting the server's explicit instruction.

### Retried request bodies must be replayable

An HTTP request body is an `io.Reader`, and a reader is drained after the first
send. A retried request built from the same drained reader is sent with an *empty
body* — a subtle, data-corrupting bug. `net/http` solves this with
`Request.GetBody func() (io.ReadCloser, error)`, which produces a fresh copy of
the body for each attempt; the standard client sets it automatically for bodies
created from `bytes.Buffer`, `bytes.Reader`, or `strings.Reader`. A retrying
`RoundTripper` must call `GetBody` to rebuild the body before each resend. It must
also drain and close the response body of every *discarded* attempt
(`io.Copy(io.Discard, resp.Body)` then `resp.Body.Close()`), or the connection is
not returned to the pool and keep-alive is defeated.

### Retries must be observable

A retry that no one can see is a latency and cost problem hiding in plain sight.
"p50 is fine but 30% of calls now retry twice" is a leading indicator that a
dependency is degrading — and it is completely invisible without instrumentation.
Emit, at minimum: which attempt finally succeeded, a counter of retries versus
first-try successes, a counter of final failures, and a structured per-attempt log
line (`slog`) carrying the attempt number, the classified retryability, and the
delay. An `OnRetry(attempt, err, delay)` hook is the clean extension point.

### Retry timing must be tested deterministically

Testing retry logic with real `time.Sleep` gives you slow, flaky tests that cannot
even assert the backoff sequence — a test that sleeps for real to check a 1-second
`Retry-After` is a 1-second test, and one that asserts "returned within 5ms of the
deadline" flakes under load. The fix is to inject the two sources of nondeterminism:
the *clock* (a `Clock` interface with a real implementation and a controllable fake
that advances virtual time on demand) and the *RNG* (a seeded `*math/rand/v2.Rand`
via `rand.NewPCG`, so jitter is reproducible). With both injected, the whole suite
runs in microseconds, asserts exact sleep sequences, and never flakes.

## Common Mistakes

### Retrying on every error

Wrong: `for i := 0; i < 3; i++ { if err = op(); err == nil { break } }` — retries
on 4xx, on validation failures, on programmer bugs, turning one bad request into
several.

Fix: classify first. Retry only when the error is a genuine transient (timeout,
5xx, 429, an explicit retryable sentinel). A 400 or a `context.Canceled` returns
immediately.

### Using the deprecated Temporary()

Wrong: `if ne, ok := err.(net.Error); ok && ne.Temporary() { retry() }`. The
`Temporary()` bit is deprecated and its meaning is unreliable.

Fix: use `Timeout()` plus explicit status-code and sentinel classification via
`errors.As` and `errors.Is`.

### No backoff, or backoff without a cap

Wrong: a tight retry loop with no delay (hammers the server), or exponential
backoff with no clamp (delays explode to minutes).

Fix: `delay = min(cap, base·factor^attempt)`. Both the growth and the clamp are
required.

### No jitter

Wrong: deterministic exponential backoff, so all clients that failed together
retry in lockstep and form a thundering herd.

Fix: add jitter (full or decorrelated) so clients de-synchronize.

### Bounding retries by count only

Wrong: `MaxAttempts: 5` with growing backoff and no deadline check — a slow
dependency silently blows the caller's SLO.

Fix: derive the budget from the caller's context deadline; check
`time.Until(deadline)` before sleeping and give each attempt a sub-deadline.

### Retrying a non-idempotent write without a key

Wrong: retrying a POST that charges a card after an ambiguous failure — double
charge.

Fix: attach a stable idempotency key generated once per logical operation and
resent on every attempt; the server deduplicates on it.

### Retrying without GetBody

Wrong: retrying an `http.Request` whose body is a one-shot reader, so the retried
request is sent with an empty body.

Fix: rely on / set `Request.GetBody` to rebuild the body per attempt.

### Leaking discarded response bodies

Wrong: on a retryable response, immediately looping without reading/closing the
body, leaking the connection.

Fix: `io.Copy(io.Discard, resp.Body); resp.Body.Close()` before the next attempt.

### Ignoring Retry-After

Wrong: computing your own backoff on a 429/503 that carried a `Retry-After`
header, fighting the server's explicit backpressure.

Fix: parse `Retry-After` (integer seconds or `http.ParseTime`) and let it override
the computed delay.

### Aggressive retries with no breaker or budget

Wrong: high `MaxAttempts` everywhere, no circuit breaker, no client-side retry
budget — a sustained outage receives multiples of normal load and becomes a
self-sustaining metastable failure.

Fix: compose retries with a circuit breaker (for sustained outages) and a
token-bucket retry budget (to cap the storm as a fraction of traffic).

### Sharing an unsynchronized RNG or reseeding per call

Wrong: calling a shared `*rand.Rand` from many goroutines without synchronization
(data race), or reseeding it every call (non-random jitter).

Fix: use the concurrency-safe top-level `math/rand/v2` functions in production, or
a per-caller seeded `*rand.Rand` guarded by a mutex in tests.

### Testing with real sleeps

Wrong: `time.Sleep` in tests, producing slow, flaky suites that cannot assert the
backoff sequence.

Fix: inject a `Clock` and a seeded RNG; advance virtual time and assert exact
delays.

### Swallowing the last error

Wrong: after exhausting retries, returning a generic `errors.New("failed")`,
destroying the diagnostic signal.

Fix: wrap and return the last error (or `errors.Join` the attempts) so
`errors.Is`/`errors.As` still work upstream.

Next: [01-backoff-retry-client.md](01-backoff-retry-client.md)
