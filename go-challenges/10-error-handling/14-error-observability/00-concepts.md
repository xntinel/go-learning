# Error Observability: Logging, Metrics, and Tracing — Concepts

An error that returns cleanly up the stack and is never observed is, from
operations' point of view, an error that did not happen — until 3am, when it
happens ten thousand times and nobody can find it. Observability is where error
handling stops being a language topic and becomes an operations topic. A senior
backend engineer does not just wrap the sentinel with `%w`; they make sure that
when the error fires, the on-call has a correlated log line to read, a category
counter to graph, and a trace span to show where in the request path it broke.
This file is the conceptual foundation. Read it once and you have the model you
need for each of the ten independent exercises that follow.

## Concepts

### Three pillars, three questions

Logs, metrics, and traces are not three views of the same thing; they answer
three different questions, and an error is not observed until it lands in the
pillar that answers the question you will actually ask.

- Logs answer "what exactly happened to this one request." High detail, low
  aggregation, relatively expensive per line. This is where the op, the id, the
  wrapped error chain, and the correlation IDs live.
- Metrics answer "how often, and in which category, aggregated across all
  requests." Cheap to store, cheap to alert on, but they carry no per-request
  detail: a counter that says `errors_not_found=4021` cannot tell you which
  request failed. This is what dashboards graph and alerts fire on.
- Traces answer "where in the call graph did it break, and how long did each hop
  take." A span tree spanning services shows that the 500 the user saw came from
  a timeout three hops down in the inventory service, which a single log line on
  the edge service could never reveal.

The design consequence: emit an error into all three, each shaped for its
question. A bare `log.Println(err)` answers none of them well.

### slog is the structured logger; log the error as an attribute

`log/slog` is the standard library's structured logger. Two rules matter for
error observability. First, prefer the `*Context` methods (`ErrorContext`,
`InfoContext`) so a custom `slog.Handler` can enrich the record from the
`context.Context` — this is how correlation IDs get onto every line without
threading them through every function signature. Use a JSON handler in
production (machine-parseable) and a text handler in development (human-readable).

Second, and this is the trap that silently destroys error handling: log the
error as an *attribute*, `logger.ErrorContext(ctx, "op failed", "err", err)`,
never as a pre-formatted string, `logger.Error(err.Error())`. The moment you
call `err.Error()` you have flattened the error into an opaque message: the
`errors.Is`/`errors.As` chain is gone, `%w` wrapping is gone, and any
`slog.LogValuer` structured enrichment the error type offered is gone. Passing
the error as an attribute keeps all of it alive for the handler to render.

### Correlation is the whole point

Without a `request_id` and (in a distributed system) a `trace_id` on every line,
structured logs are just verbose unstructured logs: you cannot reassemble the
story of one request out of the interleaved output of a thousand concurrent ones.
The disciplined way to guarantee correlation is a custom `slog.Handler` that
reads the IDs out of `context.Context` in its `Handle` method and stamps them on
every `slog.Record`. Inbound HTTP middleware puts the IDs into the context once;
the handler adds them to every line automatically, so no call site has to
remember. A handler that wraps another handler must delegate `Enabled`,
`WithAttrs`, and `WithGroup` to the inner one so field chaining still works.

### slog.LogValuer lets a type own its structured shape

A domain error is more than a message: it has a code, a kind, a retryable flag.
`slog.LogValuer` lets the type control its own log representation by implementing
`LogValue() slog.Value` and returning a group, so `logger.Error("...", "err",
domainErr)` emits `err.code`, `err.kind`, `err.retryable` as separately
queryable fields instead of one string you have to regex in the log backend. The
value is resolved lazily by the handler, so when the level is disabled the cost
is never paid — enrichment stays off the hot path.

### Metric cardinality is a production hazard

A time-series metrics backend (Prometheus, and every hosted equivalent) stores
one series per unique combination of label values. Label a request counter by the
raw URL path (`/users/8a3f...`), or the user id, or the full error message, and
you mint a new series for every distinct value — millions of them — and the
metrics backend runs out of memory and falls over. This is "cardinality
explosion," and it is a self-inflicted outage senior engineers learn to fear.
Bucket by *bounded* dimensions instead: the route *template* (`/users/{id}`, not
the concrete path), the status *class* (`5xx`, not `503` per endpoint if you must
bound it further), and the error *category* (`not_found`, `dependency`), never
the unbounded raw value.

### RED and the four golden signals

RED — Rate, Errors, Duration — is the default metric set for request-driven
services: how many requests, how many failed, how long they took, per route. USE
— Utilization, Saturation, Errors — is its counterpart for resources (a queue, a
connection pool, a disk). Errors appear in both, which is exactly why error
handling and observability are one discipline. Google's SRE "four golden
signals" (latency, traffic, errors, saturation) is the same idea. The practical
takeaway: instrument every request path with rate, error, and duration bucketed
by route and status class, and you have covered the questions an on-call asks
first.

### Panics bypass every error path

Every technique above assumes the error travels a `return`. A panic does not: it
unwinds the stack past every `if err != nil`, and if nothing recovers it, the
goroutine — and often the process — dies silently. There must be a
recover-and-observe boundary at every request edge and every spawned goroutine:
a deferred `recover()` that captures the stack with `runtime/debug.Stack`, logs
it at error level with the request context, increments a panic counter, and
translates the panic into a 500 for HTTP. Without it a panic is an invisible
crash; with it, it is just another observed error.

### Cost and signal-to-noise are engineering constraints

One flapping downstream dependency can make a hot path fail millions of times a
minute, and if each failure logs a line you get three things at once: the real
signal drowned under identical noise, a log pipeline saturated, and a bill (most
hosted log backends charge by volume). The fix is to sample the *log* — emit the
first N occurrences of an error signature, then throttle to one line per interval
carrying a suppressed-count — while keeping the *metric* exact by incrementing the
counter on every single occurrence. Metrics are cheap and aggregate; logs are
expensive and detailed; sample the expensive one, keep the cheap one honest.

### Alert on symptoms and SLO burn, not on every error

Paging a human on every error trains them to ignore the pager. Most client `4xx`
errors are expected (a bad request is the client's problem, not yours); sustained
dependency or internal `5xx` errors crossing an error budget are pageable.
SLO-based alerting classifies errors by severity, feeds a sliding-window
error-rate tracker, and fires only when the burn rate over the window exceeds the
budget. This is why severity classification (`errors.Is`/`errors.As` mapping
categories to `slog.LevelWarn` vs `slog.LevelError`) is part of error handling,
not a separate ops concern.

### OTel records errors distinctly from status

On an OpenTelemetry span, recording an error and marking the span failed are two
separate acts, and you need both. `span.RecordError(err)` attaches an exception
*event* to the span (the error detail: type, message), but it does *not* change
the span's status — the span still looks successful in the trace UI.
`span.SetStatus(codes.Error, msg)` marks the span failed, but on its own it drops
the error detail. Do both on the failure path; on the success path, leave the
status `Unset`. This is the single most common OTel mistake in code review.

### Observability is a cross-cutting concern

The thread tying all of this together: the domain and repository layers emit
plain *facts* — a wrapped error, a category — and know nothing about HTTP,
correlation IDs, or metrics backends. The transport edge (handlers, middleware,
custom slog handlers) attaches correlation, redaction, metrics, and traces. This
separation is what makes each concern independently testable: you test the
category counter by driving errors and reading a snapshot, the redactor by
logging into a buffer, the span recorder with an in-memory exporter — none of it
requires standing up the whole service. Sprinkling `metrics.Inc()` and
`logger.Error()` calls through the domain couples it to transport and makes it
untestable.

### Redaction is not optional

Request payloads reach error logs — you log the request that failed so you can
reproduce it — and payloads contain passwords, tokens, and PII. Centralize
redaction in `slog.HandlerOptions.ReplaceAttr`, which is called for every
attribute at every nesting depth (the `groups []string` argument gives you the
path, so redaction can be depth-aware), rather than trusting every call site to
scrub. A single central redactor is auditable; a hundred scattered scrubs are not.

## Common Mistakes

### Logging err.Error() and losing the chain

Wrong: `logger.Error(err.Error())`. The structured fields, the `errors.Is`/`As`
chain, `%w` wrapping, and any `LogValuer` enrichment are all flattened into one
opaque string.

Fix: pass the error as an attribute — `logger.ErrorContext(ctx, "op failed",
"err", err)` — so wrapping, matching, and enrichment survive to the handler.

### Counting all errors in one counter

Wrong: a single `errors_total` counter. The dashboard cannot tell whether the
spike is harmless `not_found` traffic or a failing database.

Fix: categorize with `errors.Is`/`errors.As` into separate counters
(`not_found`, `invalid`, `dependency`, `internal`).

### Logging with no correlation context

Wrong: `logger.Error("error")` with no op, id, `request_id`, or `trace_id`. The
on-call has a line that says something failed and no way to find which request.

Fix: attach correlation IDs through a context-aware handler so every line from a
request is stamped automatically.

### High-cardinality metric labels

Wrong: labeling a metric by the raw URL path, user id, or full error message.
Each distinct value is a new series; the count explodes and the metrics backend
crashes.

Fix: bucket by route template, status class, and error category — bounded
dimensions only.

### No panic boundary

Wrong: assuming `if err != nil` covers everything. A panic in a handler or a
spawned goroutine unwinds past every error check and crashes silently.

Fix: a deferred recover-and-observe boundary at every request and goroutine edge
that logs the stack, counts it, and returns a 5xx.

### Leaking secrets and PII into logs

Wrong: `logger.Error("bad request", "req", requestStruct)` where the struct holds
a password or email.

Fix: redact sensitive keys centrally in `ReplaceAttr` (depth-aware via the
`groups` path); never rely on each call site to remember.

### Unbounded logging on a hot failure path

Wrong: logging every occurrence when one dependency is flapping — millions of
identical lines that drown the signal and inflate the bill.

Fix: sample the log (first N, then throttle with a suppressed-count) while
keeping the metric counter exact.

### OTel: RecordError without SetStatus (or vice versa)

Wrong: `span.RecordError(err)` alone leaves the span status Unset — it looks
successful. `span.SetStatus(codes.Error, ...)` alone marks it failed but with no
error detail.

Fix: do both on the failure path; leave the status Unset on success.

### Alerting on every error including expected 4xx

Wrong: paging on every error, including client `4xx`. The on-call learns to
ignore the pager.

Fix: classify severity and alert on SLO burn over a window; client errors warn,
sustained dependency errors page.

### Observability inside business logic

Wrong: metrics and log calls sprinkled through the domain, coupling it to
transport and making it untestable.

Fix: keep observability in handlers, middleware, and custom slog handlers; let
the domain emit plain wrapped errors and categories.

Next: [01-structured-error-logging-slog.md](01-structured-error-logging-slog.md)
