# LLM Resilience: Retries, Timeouts, and Caching — Concepts

An LLM provider is the flakiest dependency a backend team has added in a decade.
Its p99 latency is tens of seconds, not tens of milliseconds. It rate-limits you
aggressively with 429s that carry a `Retry-After` header. It emits sporadic 5xx
and drops connections mid-stream. And it charges per token, so every duplicate
call costs real money. The senior job is not "call the SDK" — it is "wrap a slow,
expensive, rate-limited, non-deterministic dependency so that when it degrades it
does not take down your own service." This file is the conceptual foundation for
the three independent exercises that follow; read it once and you have the model
you need to reason through each.

Two facts change the entire design and are the load-bearing insight of this
chapter. First, the official Anthropic and OpenAI Go SDKs *already* retry: by
default two retries (three total attempts) with exponential backoff, full jitter,
an 8-second per-step cap, and they honor `Retry-After`/`Retry-After-Ms` up to
roughly one minute. A naive custom retry loop layered on top double-retries and
multiplies both tail latency and cost. Second, the value a backend team adds is
the stuff the SDK cannot know: an end-to-end deadline budget shared across
attempts, a circuit breaker so an outage does not turn N retries per request into
a retry storm, and a response cache with request coalescing so that 500 identical
concurrent prompts collapse to one upstream call. That resilience logic is
provider-agnostic middleware. Every exercise here wraps a `Completer` interface,
never the live network, so the whole thing is unit-testable offline with a fake
client, an injected clock, and a seeded RNG.

## Concepts

### Where retries belong: idempotency

A retry re-sends a request you believe failed. That belief is only safe when the
request is idempotent or when you *know* the server never processed it. A
non-streaming chat completion that failed with a connection error before any
response arrived produced no output and (for the major providers) was not billed,
so re-sending it is safe. But a request that timed out *after* the bytes left your
process may have succeeded server-side: the provider generated (and billed) the
completion, and if the model called a tool with a side effect, that side effect
already happened. Retrying it double-bills and double-executes. So the rule is:
retry transport/connection failures and responses you never received; do not
blindly retry an ambiguous post-send timeout unless the operation is idempotent or
carries an idempotency key that lets the server deduplicate.

### Error classification is the core of a retry policy

A retry policy is mostly a function from an error to a boolean. Retry connection
errors, 408 (request timeout), 409 (conflict), 429 (too many requests), and any
5xx — these are transient and a later attempt may succeed. Never retry 400, 401,
403, 404, or 422: a malformed request or a bad API key will fail identically
forever, so retrying only burns the deadline budget and the money. With the
official SDK you classify by unwrapping to the concrete error type — for Anthropic
that is `*anthropic.Error`, obtained with `errors.As(err, &apiErr)`, whose
`StatusCode` field carries the HTTP status. `context.DeadlineExceeded` and
`context.Canceled` are terminal, never retryable: the caller has already given up,
so another attempt is pure waste.

### Full jitter, and why lockstep retries re-trigger the outage

Backoff without jitter is a trap. If a fleet of clients all fail at the same
instant (a provider blip), and they all compute the same `base * 2^attempt` delay,
they all retry at the same instant — a synchronized thundering herd that spikes the
provider exactly when it is trying to recover, re-triggering the outage. Jitter
decorrelates them. The AWS "exponential backoff and jitter" study compared
strategies and found *full jitter* — sleep a uniform random value in
`[0, min(cap, base*2^attempt))` — minimizes both contention and total completion
time. This is why the SDKs jitter and why any custom layer must too. For tests,
the randomness must be injectable: build a `*rand.Rand` from
`rand.New(rand.NewPCG(seed1, seed2))` so the sequence is deterministic, rather
than calling the package-level `rand.N`, which draws from an unseedable global
source.

### Backoff must be bounded twice

One bound is not enough. A *per-step cap* (8-30s is typical) stops late attempts
from sleeping for minutes as `2^attempt` explodes. A *total* bound — a maximum
attempt count or, better, a total-elapsed deadline — stops a doomed request from
retrying forever and makes it fail fast so the caller gets an answer (even a
failure) within its SLA. A policy with only a per-step cap will happily retry a
permanently-broken dependency until the heat death of the connection pool.

### Honoring Retry-After, but clamping it

A 429 or 503 often carries `Retry-After` (seconds) or a provider-specific
`Retry-After-Ms`: the server is telling you exactly when to come back. A correct
policy prefers that value over its own computed backoff — the server knows more
than your formula does. But it must still clamp it. A hostile or misconfigured
header of `Retry-After: 3600` must not pin a request open for an hour past its
deadline; the SDKs cap a "reasonable" `Retry-After` at about one minute for
exactly this reason. In a budget-aware layer the natural clamp is the remaining
deadline: never sleep longer than the time left before the caller's SLA expires.

### Timeout layering: per-attempt versus the whole budget

There are two distinct timeouts and conflating them breaks the design. A
*per-attempt* timeout bounds one HTTP call. An *overall deadline budget* bounds the
entire retry sequence, including the sleeps between attempts. If you slap a single
`context.WithTimeout` around the whole loop *and* give each attempt its own copy of
the same duration, the first attempt can consume the entire budget and leave
nothing for retries. The fix is to derive each attempt's timeout from the
*remaining* budget: `ctx.Deadline()` minus now, capped by a per-attempt maximum.
That is what stops a retry sequence from silently blowing past the SLA.
`context.WithTimeoutCause` lets you attach a sentinel error to the per-attempt
timeout so the caller can later distinguish, via `context.Cause`, "this one attempt
was slow" from "we ran out of the overall budget."

### Streaming needs an idle timeout, not a total-duration timeout

A single overall timeout is wrong for a streamed completion. A long but perfectly
healthy stream — the model is producing a 4000-token answer — would be killed by a
30-second total cap even though bytes are arriving steadily. The right control for
server-sent events is an *idle* (inactivity) timeout that resets on every token or
event received. The stream is aborted only if nothing arrives for, say, 15
seconds. This is precisely why you do not just wrap a stream in
`context.WithTimeout`.

### The circuit breaker: retry-storm insurance

Retries assume failures are independent and transient. During a real provider
outage that assumption is false: *every* request fails, *every* request retries N
times, and you amplify load on an already-down dependency by a factor of `1+N`
while your own goroutines and connection pool pile up waiting. A circuit breaker
converts that cascading failure into a bounded, cheap rejection. It has three
states. *Closed*: requests flow, consecutive failures are counted. When the count
crosses a threshold the breaker trips to *open*: every request fails fast with a
sentinel error, doing zero upstream work and shedding load. After a cooldown it
moves to *half-open* and lets exactly one trial request through; if that succeeds
it closes, if it fails it re-opens. The breaker is shared mutable state read and
written by many goroutines, so its transitions must be guarded by a mutex, and its
clock should be injectable so the half-open cooldown is testable without real
sleeps.

### Caching semantics for a non-deterministic dependency

Identical `(model, params, messages)` inputs are cacheable — but only as an
explicit product decision, because `temperature > 0` makes the model's output
non-deterministic. Caching then trades exact reproduction for cost and latency:
two users with the same prompt get the same answer instead of two independently
sampled ones. The cache key must be a stable hash — `sha256` over a *canonical*
serialization of every field that affects the output: model, system prompt,
messages, temperature, max tokens, tools. Omit a field that changes the output and
you serve wrong answers from the cache. Include a volatile field like a request ID
or a timestamp and the key changes every time, so you never hit. The canonical
serialization must have a fixed field order — a `struct` marshaled with
`encoding/json` does, a `map` does not, because Go randomizes map iteration.

### Cache stampede and request coalescing

A cache handles *repeats over time*. It does nothing for *simultaneity*: on a cold
key, if 500 goroutines ask for it at the same instant, all 500 miss and all 500
call upstream — a stampede that is arguably worse than no cache, because it arrives
in a burst. `singleflight.Group.Do` solves the orthogonal problem: it collapses
concurrent calls for the same key into a single in-flight execution and hands the
one result to every caller. Its returned `shared bool` reports that the result was
delivered to more than one caller. Caching and singleflight are complementary: the
cache absorbs repeats across time, singleflight absorbs the simultaneous burst on
a miss.

### Eviction: TTL and LRU together

An unbounded `map` cache is a memory leak — every distinct prompt ever seen stays
resident forever. An LRU size cap bounds memory by evicting the least-recently-used
entry. A TTL bounds staleness by expiring entries after a duration; without it, an
LRU can serve an hours-old completion. You need both, and
`hashicorp/golang-lru/v2/expirable.NewLRU(size, onEvict, ttl)` combines them in one
thread-safe structure. Negative caching — briefly caching a *failure* — stops a
permanently-broken prompt (a 400) from hammering upstream on every retry, but it
must use a much shorter TTL than success caching, because the underlying condition
may be fixed at any moment.

### Composition order

The layers must stack in one specific order, outermost first: cache, then
singleflight, then circuit breaker, then retry, then per-attempt timeout, then the
client. A cache hit should skip everything below it — no breaker check, no
attempt. The breaker must wrap the retrier, so that when the breaker is open a
request costs *zero* attempts, and so that the retries for one logical request are
not each counted as separate failures against the breaker. Get this backwards —
put the breaker *inside* the retry loop — and a single request's own retries trip
the breaker by themselves, or an open breaker still consumes attempts. The
per-attempt timeout is innermost, bounding exactly one HTTP call.

## Common Mistakes

### Double-retrying on top of the SDK

The SDK already retries twice by default. Adding a hand-rolled retry loop around it
turns one logical call into up to 3x3 = 9 attempts and multiplies the effective
timeout. Pick exactly one layer: either set `option.WithMaxRetries(0)` and own the
retry logic yourself, or lean on the SDK's retries and add only cache, breaker, and
budget around it. Never both.

### Sleeping without watching the context

`time.Sleep(backoff)` blocks straight through cancellation, so a request whose
context was cancelled keeps burning backoff sleeps for nothing. Sleep by selecting
on `ctx.Done()` versus a `time.NewTimer(backoff).C`, return
`context.Cause(ctx)` on cancellation, and always `Stop` the timer.

### Retrying non-retryable errors

Looping on a 400, 401, or 422 re-sends a request that can never succeed and wastes
the entire budget doing it. Classify with `errors.As(err, &apiErr)` on
`apiErr.StatusCode` against a retryable set, and treat `context.Canceled` and
`context.DeadlineExceeded` as terminal.

### No jitter, or additive-only jitter

`base * 2^n` with a fixed sleep re-synchronizes the fleet on the next failure. Use
full jitter — a uniform draw in `[0, min(cap, base*2^n))`. For deterministic tests
inject a `*rand.Rand` seeded with `rand.NewPCG(seed1, seed2)` instead of the
unseedable package-level functions.

### Ignoring Retry-After, or trusting it unbounded

Computing your own backoff when the server sent `Retry-After` wastes attempts;
blindly obeying a huge `Retry-After` pins the request open past its deadline.
Prefer the header, but clamp it to the remaining budget.

### One `context.WithTimeout` for the whole loop and each attempt

If the overall timeout and the per-attempt timeout are the same duration, the first
attempt can eat the whole budget and leave nothing for retries; used on a stream,
it kills a healthy long response. Derive each attempt's timeout from the remaining
budget, and use an idle timeout for streams.

### An unstable cache key

Building the key from a `struct` via `fmt.Sprintf("%v")` or from a `map` gives
non-deterministic ordering (and pointer/time fields vary), so keys never hit. Hash
a canonical, field-ordered `json.Marshal` of only the output-affecting inputs with
`sha256.Sum256` and hex-encode it.

### Caching without coalescing

A TTL cache still lets a burst of concurrent misses stampede upstream. Wrap the
miss path in `singleflight.Group.Do` keyed by the same cache key.

### An unbounded cache, or an LRU with no TTL

A plain `map` grows forever; a plain LRU serves hours-stale completions. Use
`expirable.NewLRU(size, onEvict, ttl)` to bound both memory and staleness, and give
negative-cache entries a much shorter TTL than success entries.

### The circuit breaker inside the retry loop

If the breaker wraps a single attempt rather than the whole retrier, one request's
retries trip it by themselves, or an open breaker still consumes attempts. The
breaker wraps the retrier; when open it returns `ErrCircuitOpen` immediately with
zero attempts.

### Racing on breaker or cache state

Breaker state (the current state, the failure count, the opened-at time) is read
and written from many goroutines. Guard every transition with a `sync.Mutex` and
inject a `func() time.Time` clock so the half-open cooldown is testable without
real sleeps. Run the tests with `-race` to prove it.

Next: [01-retry-backoff-jitter.md](01-retry-backoff-jitter.md)
