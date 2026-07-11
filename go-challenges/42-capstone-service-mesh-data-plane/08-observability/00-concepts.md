# 8. Observability — Concepts

A service mesh data plane is only as operable as it is observable. When a request slows down or starts failing, the on-call engineer has three questions: how many, how slow, and which one — and a fourth that ties them together: where did this single request go as it fanned out across services. Observability answers those with three complementary signals, and this chapter builds each from scratch as an independent Go module. The first is metrics: cheap, aggregated counters and distributions updated on the hot path of every request, scraped as Prometheus text. The second is structured access logs: one machine-parseable record per request carrying the dimensions a metric had to drop to stay cheap. The third is distributed tracing context propagation: the small header dance that lets every hop of one logical request share a trace identifier, so a log line, a metric sample, and a span all point back to the same journey. Read this file once and you will have the conceptual frame for all three exercises.

## Concepts

### The Three Pillars and Why a Proxy Needs All Three

Metrics, logs, and traces are often called the three pillars of observability, and the reason all three exist is that each is cheap exactly where the others are expensive. A metric is an aggregate: incrementing a counter costs one atomic instruction and answers "how many 5xx in the last minute" for a million requests with a handful of numbers, but it cannot tell you which request failed. A log is per-event: one structured record per request preserves every dimension — path, client IP, upstream, latency — but writing and storing a line per request is orders of magnitude more expensive than a counter increment, so logs are sampled or filtered under load. A trace is per-request-across-services: it stitches the hops of one logical request into a causal tree, which is the only thing that explains why a request that touched six upstreams took 900 ms, but it requires every service to propagate a shared context. A proxy sits on the request path for every service in the mesh, so it is the natural place to emit all three: it counts (metrics), it records (access logs), and it propagates (trace context). The glue is a single trace identifier that appears in the access log line and the trace header alike, so a high-latency metric leads to a log query leads to a trace.

### The Four Golden Signals

Google's SRE practice distills monitoring to four signals that together give complete visibility into a service's health: latency (how long requests take), traffic (how many arrive), errors (which fail and why), and saturation (how full the resource is). In a proxy data plane those map directly onto four metric families: a latency histogram labeled by upstream/method/status, a request counter with the same labels (traffic), an error counter labeled by upstream/error-type, and an active-connections gauge labeled by upstream (saturation). Every metric in the first exercise exists to capture one of those four signals; if a metric does not map to a golden signal, question whether it earns its cardinality.

### Metric Types and Their Semantics

A counter is monotonically non-decreasing; it resets only on process restart. Counters fit totals: requests processed, bytes sent, errors encountered. A gauge moves up and down freely: active connections, queue depth, memory in use. A histogram records a distribution by bucketing each observation into a pre-configured upper-bound slot and accumulating a total count and sum; it enables percentile estimation without storing individual samples and is the standard tool for latency. Prometheus identifies metric families by name and discriminates members of one family by label set: each unique combination of label values is a distinct time series. The wire format emits `# HELP` and `# TYPE` headers per family, then one line per series, and for histograms three synthetic suffixes (`_bucket`, `_count`, `_sum`).

### Atomic Float64 Updates via CAS Loops

The hot-path constraint is that a counter increment must not take a lock — at thousands of concurrent goroutines, a mutex per metric serializes the whole proxy down to single-goroutine throughput. Go's `sync/atomic` provides `atomic.Uint64` with `Load`, `Store`, `Add`, and `CompareAndSwap`. Counters and gauges store their value as the IEEE 754 bit pattern of a `float64` inside an `atomic.Uint64`. Because there is no native atomic float64 add, addition uses a compare-and-swap loop: read the current bits, compute the new bits, attempt the swap, retry if another goroutine raced in between. Under typical load the loop succeeds on the first try; contention only adds iterations, never blocks. Gauge subtraction is the same loop with a negative delta. Histogram bucket counts are simpler still: each bucket is an `atomic.Uint64` incremented with a native `Add(1)`, no CAS loop, so an observation is one atomic increment on the bucket plus one CAS loop on the running sum.

### Histogram Buckets: Non-Cumulative Storage, Cumulative Exposition

A histogram stores one count per bucket plus a total count and a sum. The subtlety is that Prometheus uses cumulative `le` ("less than or equal") buckets on the wire: the value reported for `le="0.1"` must include every observation at or below 0.1, not just those that landed strictly in the 0.05–0.1 band. Storing cumulative counts directly would make every observation touch every higher bucket; instead each observation increments exactly one bucket (found with binary search under `le` semantics, which `sort.SearchFloat64s` provides exactly), and the cumulative values are computed by a running sum at collection time. A value exactly equal to a bucket bound must land in that bound's bucket — that is what `le` means — and `SearchFloat64s`, returning the first index whose bound is `>= v`, gets the boundary case right.

### Label Cardinality and Memory Bounds

Every unique label combination creates a new child metric, allocated and registered in a `sync.Map`. If a label dimension is the request path (`/api/users/42`, `/api/users/43`, …) the number of combinations grows without bound and the registry consumes arbitrary memory — the failure mode known as a cardinality explosion. The fix is a cardinality limit: a labeled metric tracks how many distinct label sets it has admitted with an `atomic.Int64`, and once a new combination would exceed the limit the caller receives a no-op child that silently absorbs operations. Exposing the breach as a sentinel error (`ErrCardinalityLimit`) lets callers that care detect and alert on it while the hot path stays branch-free. The discipline that prevents the breach in the first place is to label only by stable, low-cardinality dimensions: upstream name, HTTP method, status class. High-cardinality data — the exact path, the user id, the trace id — belongs in a log line or a trace, not a metric label.

### Structured Access Logs

Where a metric drops dimensions to stay cheap, an access log keeps them. The right shape for machine consumption is one JSON object per line — the JSON Lines (newline-delimited JSON, ndjson) format — because a log aggregator can split on newlines and parse each line independently, and Unix tools (`grep`, `jq`) work line by line. Each record carries the dimensions a metric cannot afford as labels: method, full path, upstream, client IP, status, byte count, latency in milliseconds, and crucially the trace id. Two properties matter for correctness. First, field order and types must be stable so downstream schemas do not drift; encoding a fixed struct (whose fields `encoding/json` emits in declaration order) gives that for free. Second, writes from many request goroutines must not interleave on a single line — a half-written object from goroutine A spliced into goroutine B's line is unparseable garbage — so a single mutex serializes the encode-and-write, which is cheap relative to the network I/O the proxy is already doing. The trace id in the log line is the join key: it is what turns "p99 latency spiked on svc-b" (a metric) into "here are the exact requests" (a log query) into "here is where each one spent its time" (a trace).

### Distributed Tracing and Context Propagation

A distributed trace is the causal tree of one logical request as it fans out across services. Each unit of work is a span; spans share a trace id and form a parent/child tree via span ids. The only thing a proxy must do to participate is propagate the context: read the incoming trace identifiers from a header, continue the same trace, allocate a fresh span id for its own hop, and write the updated context into the outgoing request's header. The standard that makes this interoperable across vendors is W3C Trace Context, whose `traceparent` header has four hyphen-separated fields: `version-traceid-spanid-traceflags`, for example `00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`. The version is `00`; the trace id is 16 bytes (32 hex chars) shared by every hop; the parent/span id is 8 bytes (16 hex chars) unique to this hop; the trace flags are one byte whose low bit is the sampled flag. The spec forbids an all-zero trace id or span id. The propagation rule for a proxy is exactly: if the inbound request carries a valid `traceparent`, keep its trace id and mint a new span id for the hop; otherwise start a fresh root trace. In Go the natural carrier for the resulting span context inside the process is `context.Context` — store the span on the request's context with `context.WithValue` and it rides along the call chain without threading through every signature, while the header is what carries it across the process boundary to the next service.

### Why These Three Are Co-Designed

The trace id is the thread that ties the pillars together, which is why this chapter builds them in one place. A request enters the proxy; the proxy extracts or starts a trace context (tracing), increments the request counter and observes the latency histogram (metrics), and on completion writes one access-log line stamped with the same trace id (logs). When an alert fires on the error counter, the trace id carried in the matching access-log lines is the handle that opens the corresponding traces. Build each pillar to stand alone — each exercise here is its own module — but design their identifiers to meet.

## Common Mistakes

### Using a Mutex for Every Counter Update

Wrong: wrapping each counter update in a `sync.Mutex`. At a thousand concurrent goroutines the increment serializes and proxy throughput collapses to what a single goroutine sustains. Fix: store the value as `atomic.Uint64` bits and update with a CAS loop (counters/gauges) or a native `Add` (histogram buckets). The CAS loop retries only on write-write contention; reads never block.

### Emitting Raw Bucket Counts Instead of Cumulative Ones

Wrong: writing each bucket's local count to the Prometheus text. A scraper treats `latency_bucket{le="0.1"}` as inclusive of everything at or below 0.1; if you emit the non-cumulative local count, a lower bucket can report a higher number than a higher bucket and Prometheus rejects the non-monotonic series. Fix: store non-cumulative counts and accumulate a running total at collection time so the emitted buckets are monotonically cumulative.

### Cardinality Explosion From High-Dimensional Labels

Wrong: labeling a metric by raw path, user id, or trace id. Every distinct value mints a new child and the registry grows without bound. Fix: enforce a cardinality limit and label only by stable, low-cardinality dimensions (upstream, method, status class). Put the high-cardinality data in an access log or a trace, where one record per request is the expected cost.

### Interleaved Concurrent Log Writes

Wrong: letting many request goroutines write to the same log writer without serialization. Two `Encode` calls can interleave their bytes on one line and produce an object that no JSON parser will accept. Fix: hold a mutex across the encode-and-write so each line is emitted atomically. The lock is negligible next to the network work the proxy already performs.

### Generating a New Trace ID at Every Hop

Wrong: minting a fresh trace id in the proxy on every request, even when the inbound request already carries a `traceparent`. This severs the trace: the upstream caller's trace id and the proxy's no longer match, and the request appears as several unrelated traces. Fix: extract the inbound `traceparent`; if it is valid, keep its trace id and only allocate a new span id for this hop. Start a new root trace only when there is no valid inbound context.

### Trusting a traceparent Without Validation

Wrong: splitting the header on `-` and copying the fields without checking widths, hex validity, version, or the all-zero prohibition. A malformed inbound header then propagates corruption downstream or panics on a short slice. Fix: validate exactly four fields, the supported version, the precise hex widths (32 / 16 / 2), valid hex, and non-zero trace and span ids before trusting any field; on any failure, treat the request as having no inbound context and start a fresh root.

---

Next: [01-metrics-pipeline.md](01-metrics-pipeline.md)
