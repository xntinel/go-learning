# 7. Rate Limiting — Concepts

Rate limiting in a service mesh data plane is far harder than a counter per IP address. A real engine must run several algorithms at once — a token bucket to smooth bursts, a sliding window to enforce strict per-period quotas — classify each request on multiple dimensions simultaneously, apply the most restrictive rule when several match, bound its own memory so a proxy that sees millions of unique clients a day does not exhaust the heap, and emit the standard `X-RateLimit-*` and `Retry-After` headers so well-behaved clients self-regulate instead of hammering the proxy with retried 429s. This file is the conceptual foundation. Read it once and the four exercises that follow — the token bucket backend, the sliding window backend, the descriptor rule engine, and the HTTP middleware — each become a self-contained module you can build and gate on its own.

## Concepts

### Token Bucket: Burst Then Throttle

A token bucket holds at most `burst` tokens and refills at `rate` tokens per second. Each request consumes one token; a request that finds the bucket empty is rejected. The bucket starts full, so a fresh client may fire a burst of up to `burst` requests at line rate before the sustained throughput settles to `rate` requests per second. That burst tolerance is the defining property: a token bucket is the right tool when short spikes are acceptable and you only need to bound the long-run average.

The Go ecosystem ships a production-quality token bucket in `golang.org/x/time/rate`. Its `Limiter` is safe for concurrent use, does its arithmetic on the monotonic clock, and needs no background goroutine: tokens are not added by a ticker but computed lazily from the elapsed time at each `AllowN` or `ReserveN` call. That lazy model is why every method takes a `time.Time` — you tell the limiter what "now" is, which also makes the limiter trivial to drive under virtual time in a test.

`ReserveN(t, n)` is the call to reach for when you need response headers. It atomically consumes `n` tokens and returns a `*Reservation` whose `DelayFrom(t)` reports exactly how long the caller would have to wait for those tokens to be available. If the answer is "zero", the request is servable now; if it is positive, the tokens are in the future and you can deny instead of delay. Crucially, when you deny you must hand the tokens back with `r.CancelAt(t)`, because `ReserveN` has already committed them to the limiter's internal state. The plain `Allow()` method is simpler but throws away the timing metadata you need to populate `X-RateLimit-Reset`, and a separate `TokensAt` call to recover it races the next consumer.

### Sliding Window: Strict Per-Period Counting

A sliding window counts how many requests arrived in the last `window` duration and rejects the next one once that count reaches `maxReqs`. There is no burst allowance: a client that already sent `maxReqs` requests in the past second is denied even if the last of them landed 900 ms ago. This is the algorithm for a contract that reads "no more than N requests in any T-second period" — billing quotas, abuse thresholds, anything where a token bucket's tolerance of `2N` requests across a window boundary is unacceptable.

An exact sliding window must remember every timestamp, which costs O(maxReqs) memory per client. The approximation used here divides the window into `slots` equal-duration buckets arranged as a ring buffer; each bucket holds a count and the start instant of the sub-period it represents. On every call, buckets older than `window` are evicted and excluded from the total, the live counts are summed, and if there is room the current bucket is incremented. Accuracy rises with the slot count at the cost of memory; ten slots is a good default for most traffic.

### Descriptor-Based Rule Matching

A `Descriptor` is one key-value attribute pulled out of a request: `{route: /api/v1/orders}`, `{client: tenant-42}`, `{plan: free}`. A `Rule` carries a list of descriptors that must *all* be present for it to fire. Matching is exact-string equality on each pair, never a prefix or glob test — a rule keyed on `{route: /api/v1}` does not match a request whose extracted route descriptor is `/api/v1/orders`. That exactness is what makes multi-dimensional limiting work: one rule can cap `/api/v1/orders` globally, a second can throttle free-plan clients on the same path more tightly, and both fire on the same request whenever its descriptor set satisfies each rule's match list.

Descriptor extraction is deliberately separated from rule evaluation. The middleware calls user-supplied functions that map an `*http.Request` to a client key and to a descriptor slice, so the same engine drives IP-based limits, JWT-claim limits, and mTLS peer-identity limits without the engine ever touching HTTP internals.

### Most-Restrictive Rule Wins

When several rules match, the engine evaluates all of them and every matched limiter consumes one unit, regardless of the final verdict. Charging every limiter even on a denied request is the conservative choice and is what protects upstreams from a request that one dimension would have allowed. The decision rule is:

- `Allow` is true only when every matched limiter allows.
- When any limiter denies, the response reports the rule with the longest `RetryAfter` — the client must wait out the most restrictive limit.
- When all allow, the headers report the rule with the lowest remaining budget — the dimension closest to its ceiling.

### Memory Bounding: Per-Client State and TTL Eviction

Per-client limiters live in a `sync.Map` keyed by `ruleName + "\x00" + clientKey`. `sync.Map` fits this access pattern: the key set grows monotonically in the short term as new clients arrive, reads dominate, and deletions come in infrequent batch sweeps — exactly the workload its internal read-mostly optimization targets. A background goroutine wakes on a configurable interval, walks every entry with `Range`, and deletes those whose `lastSeen` timestamp is older than the TTL. It is stopped cleanly through a `chan struct{}` closed under a `sync.Once`, so `Stop` is idempotent. Set the TTL to at least the longest rate-limit window so an active client is never evicted mid-window.

### Standard Rate Limit Response Headers

The IETF draft `draft-ietf-httpapi-ratelimit-headers` defines three fields the middleware sets on every response, allowed or not:

- `X-RateLimit-Limit` — the ceiling (tokens per second, or requests per window).
- `X-RateLimit-Remaining` — the budget left in the current bucket or window.
- `X-RateLimit-Reset` — the Unix timestamp at which the budget refills.

A 429 additionally carries `Retry-After` (an integer number of seconds, or an HTTP date) so a client without rate-limit-header support still learns when to retry. Setting the metadata on allowed responses too, not only on 429s, is what lets a cooperative client throttle itself before it ever trips the limit.

## Common Mistakes

### Using Allow() Instead of ReserveN() When Headers Are Needed

Calling `lim.Allow()` and then trying to compute `X-RateLimit-Reset` from a separate `TokensAt` call discards the timing context and races the next consumer. Use `lim.ReserveN(now, 1)`, read `r.DelayFrom(now)` for both the allow/deny decision and the reset time, and call `r.CancelAt(now)` when denying. That is one atomic operation with every piece of metadata in hand.

### Unbounded Per-Client State Growth

A plain `map[string]*limiter` behind a `sync.RWMutex` with no eviction keeps every IP or client ID ever seen alive forever; a proxy handling millions of unique clients a day exhausts the heap. Store limiters in a `sync.Map` and run a background goroutine that `Range`s and `Delete`s entries whose `lastSeen` exceeds the TTL.

### Forgetting to Cancel the Reservation on Deny

Calling `ReserveN(now, 1)`, seeing `delay > 0`, and returning early without `r.CancelAt(now)` leaves the tokens committed to the limiter's internal state, so future counts are wrong. Always cancel the reservation before returning a deny.

### Treating Token Bucket and Sliding Window as Equivalent

A token bucket with `rate = N/T` and `burst = N` permits N requests at t=0 and N more at t=T — `2N` across a single window boundary — which violates a strict "no more than N in any T seconds" contract. Use a sliding window when the period count must hold exactly; reserve the token bucket for traffic smoothing where short bursts are acceptable.

---

Next: [01-token-bucket.md](01-token-bucket.md)
