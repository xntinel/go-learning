# Exercise 28: Trace Span Propagation: Extract Parent Context and Decide Sampling

**Nivel: Intermedio** — validacion rapida (un test corto).

Distributed tracing only produces a usable trace if every service in a
request's call graph agrees on the same trace ID and the same sampling
decision; a service that mints its own trace ID whenever a header parse is
slightly off silently fragments the trace into disconnected pieces an
operator can no longer follow end to end. This module extracts a trace
context from inbound headers and decides sampling with a fixed guard order,
so a missing header, an empty header, and an explicit-but-malformed header
are each handled correctly instead of collapsing into the same wrong
behavior. This module is fully self-contained: its own `go mod init`, all
code inline, its own demo and tests.

## What you'll build

```text
trace/                       independent module: example.com/distributed-trace-span-propagation
  go.mod                    go 1.24
  trace.go                  Extract(headers, samplePercent, newID), bucketOf (deterministic hashing)
  cmd/
    demo/
      main.go               five header scenarios: new trace, explicit sample, malformed header
  trace_test.go             Extract table over presence/absence/explicit/malformed; bucketOf determinism
```

- Files: `trace.go`, `cmd/demo/main.go`, `trace_test.go`.
- Implement: `Extract(headers map[string]string, samplePercent int, newID func() string) Decision`, where a comma-ok read of `X-Trace-Id` decides propagation versus minting a new ID, and a comma-ok read of `X-Sample-Decision` decides explicit sampling versus falling back to a deterministic hash-bucket of the trace ID.
- Test: a table covering no headers, an empty trace-id header, explicit sample=true, explicit sample=0, a malformed sample header falling back to probabilistic, and both sides of the probabilistic bucket boundary.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/01-if-else-and-init-statements/28-distributed-trace-span-propagation/cmd/demo
cd go-solutions/03-control-flow/01-if-else-and-init-statements/28-distributed-trace-span-propagation
go mod edit -go=1.24
```

### Why sampling is bucketed from the trace ID, not rolled per service

When no explicit sampling header is present, `Extract` does not flip its own
independent coin — it hashes the trace ID into a bucket in `[0, 100)` and
compares that bucket against `samplePercent`. That determinism is the whole
point: every service the request passes through computes `bucketOf` on the
same trace ID and gets the same bucket, so every service in the call graph
reaches the same sampling verdict independently, with no need to pass an
extra "sampled: yes/no" bit around for the probabilistic case. If sampling
were instead a fresh random roll per service, a trace could end up half
sampled and half not — spans from the services that rolled "yes" with none of
the context from the services that rolled "no," which is close to useless
for reconstructing what actually happened. The comma-ok read of
`X-Trace-Id` also matters on its own: a header present but empty must still
count as "no parent to propagate," exactly like a comma-ok map lookup
distinguishes a missing key from one stored empty.

Create `trace.go`:

```go
// Package trace extracts a distributed-trace context from inbound request
// headers and decides whether the resulting span is sampled, so a child span
// created for an outbound call carries the right trace ID and sampling
// decision instead of starting a brand-new, disconnected trace.
package trace

import (
	"hash/fnv"
	"strings"
)

// Decision is the outcome of extracting trace context from one request.
type Decision struct {
	TraceID string
	Sampled bool
	Reason  string // "explicit" or "probabilistic"
}

// Extract reads trace headers and decides the trace ID and sampling for this
// request. headers uses comma-ok presence semantics: a key absent from the
// map means the header was never sent, distinct from a key present with an
// empty string — only a present, non-empty X-Trace-Id counts as a parent to
// propagate from.
//
// The decision chain runs in a fixed order: first decide the trace ID (reuse
// the parent's if present, else mint one with newID), then decide sampling —
// an explicit, valid X-Sample-Decision header always wins; only when that
// header is absent or unparseable does sampling fall back to a probabilistic
// bucket computed deterministically from the trace ID, so every service in
// the call graph that sees the same trace ID makes the same probabilistic
// call without needing to communicate.
func Extract(headers map[string]string, samplePercent int, newID func() string) Decision {
	traceID, hasParent := headers["X-Trace-Id"]
	if !hasParent || traceID == "" {
		traceID = newID()
	}

	if raw, ok := headers["X-Sample-Decision"]; ok {
		if sampled, valid := parseSampleHeader(raw); valid {
			return Decision{TraceID: traceID, Sampled: sampled, Reason: "explicit"}
		}
		// header present but unparseable: fall through to probabilistic sampling
	}

	return Decision{TraceID: traceID, Sampled: bucketOf(traceID) < samplePercent, Reason: "probabilistic"}
}

// parseSampleHeader interprets an explicit sampling header. valid is false
// for anything other than the two recognized forms, signaling the caller
// should ignore this header rather than trust a malformed value.
func parseSampleHeader(raw string) (sampled, valid bool) {
	switch {
	case raw == "1" || strings.EqualFold(raw, "true"):
		return true, true
	case raw == "0" || strings.EqualFold(raw, "false"):
		return false, true
	default:
		return false, false
	}
}

// bucketOf deterministically maps a trace ID to a bucket in [0, 100), so the
// same trace ID always lands in the same bucket across every service that
// evaluates it independently.
func bucketOf(traceID string) int {
	h := fnv.New32a()
	h.Write([]byte(traceID))
	return int(h.Sum32() % 100)
}
```

### The runnable demo

Five scenarios walk through the decision chain: no headers at all, a
propagated trace with an explicit sample decision in each direction, a
malformed sample header falling back to probabilistic sampling, and a
propagated trace with no sample header at all.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	trace "example.com/distributed-trace-span-propagation"
)

func main() {
	newID := func() string { return "generated-trace-id" }

	scenarios := []struct {
		label   string
		headers map[string]string
	}{
		{
			label:   "no headers at all (new trace)",
			headers: map[string]string{},
		},
		{
			label: "propagated trace, explicit sample=true",
			headers: map[string]string{
				"X-Trace-Id":        "trace-abc123",
				"X-Sample-Decision": "true",
			},
		},
		{
			label: "propagated trace, explicit sample=false",
			headers: map[string]string{
				"X-Trace-Id":        "trace-abc123",
				"X-Sample-Decision": "0",
			},
		},
		{
			label: "propagated trace, malformed sample header falls back",
			headers: map[string]string{
				"X-Trace-Id":        "trace-abc123",
				"X-Sample-Decision": "maybe",
			},
		},
		{
			label: "propagated trace, no sample header at all",
			headers: map[string]string{
				"X-Trace-Id": "trace-abc123",
			},
		},
	}

	for _, s := range scenarios {
		d := trace.Extract(s.headers, 50, newID)
		fmt.Printf("%-48s traceID=%-20s sampled=%-5v reason=%s\n", s.label, d.TraceID, d.Sampled, d.Reason)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
no headers at all (new trace)                    traceID=generated-trace-id   sampled=true  reason=probabilistic
propagated trace, explicit sample=true           traceID=trace-abc123         sampled=true  reason=explicit
propagated trace, explicit sample=false          traceID=trace-abc123         sampled=false reason=explicit
propagated trace, malformed sample header falls back traceID=trace-abc123         sampled=true  reason=probabilistic
propagated trace, no sample header at all        traceID=trace-abc123         sampled=true  reason=probabilistic
```

### Tests

The table drives `Extract` through presence, absence, explicit, and
malformed header combinations, including both sides of the probabilistic
bucket boundary using fixed trace IDs whose bucket values are known ahead of
time. A separate test locks in that `bucketOf` is deterministic.

Create `trace_test.go`:

```go
package trace

import "testing"

func fixedID() string { return "generated-id" }

func TestExtract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		headers       map[string]string
		samplePercent int
		wantTraceID   string
		wantSampled   bool
		wantReason    string
	}{
		{
			name:          "no trace-id header mints a new one",
			headers:       map[string]string{},
			samplePercent: 100,
			wantTraceID:   "generated-id",
			wantSampled:   true,
			wantReason:    "probabilistic",
		},
		{
			name:          "trace-id present but empty is treated as absent",
			headers:       map[string]string{"X-Trace-Id": ""},
			samplePercent: 100,
			wantTraceID:   "generated-id",
			wantSampled:   true,
			wantReason:    "probabilistic",
		},
		{
			name:          "explicit sample=true wins over bucketing",
			headers:       map[string]string{"X-Trace-Id": "trace-high", "X-Sample-Decision": "true"},
			samplePercent: 0,
			wantTraceID:   "trace-high",
			wantSampled:   true,
			wantReason:    "explicit",
		},
		{
			name:          "explicit sample=0 wins over bucketing",
			headers:       map[string]string{"X-Trace-Id": "trace-low", "X-Sample-Decision": "0"},
			samplePercent: 100,
			wantTraceID:   "trace-low",
			wantSampled:   false,
			wantReason:    "explicit",
		},
		{
			name:          "malformed sample header falls back to probabilistic",
			headers:       map[string]string{"X-Trace-Id": "trace-low", "X-Sample-Decision": "maybe"},
			samplePercent: 50,
			wantTraceID:   "trace-low",
			wantSampled:   true, // bucket(trace-low) = 33, < 50
			wantReason:    "probabilistic",
		},
		{
			name:          "propagated trace id, bucket below threshold samples",
			headers:       map[string]string{"X-Trace-Id": "trace-low"},
			samplePercent: 50,
			wantTraceID:   "trace-low",
			wantSampled:   true, // bucket(trace-low) = 33
			wantReason:    "probabilistic",
		},
		{
			name:          "propagated trace id, bucket at or above threshold does not sample",
			headers:       map[string]string{"X-Trace-Id": "trace-high"},
			samplePercent: 50,
			wantTraceID:   "trace-high",
			wantSampled:   false, // bucket(trace-high) = 81
			wantReason:    "probabilistic",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Extract(tc.headers, tc.samplePercent, fixedID)
			if got.TraceID != tc.wantTraceID || got.Sampled != tc.wantSampled || got.Reason != tc.wantReason {
				t.Errorf("Extract(%v, %d) = %+v, want {%q %v %q}",
					tc.headers, tc.samplePercent, got, tc.wantTraceID, tc.wantSampled, tc.wantReason)
			}
		})
	}
}

func TestBucketOfIsDeterministic(t *testing.T) {
	t.Parallel()

	a := bucketOf("trace-low")
	b := bucketOf("trace-low")
	if a != b {
		t.Fatalf("bucketOf is not deterministic: %d != %d", a, b)
	}
	if a < 0 || a >= 100 {
		t.Fatalf("bucketOf returned %d, want a value in [0, 100)", a)
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The explicit-header check runs entirely before the probabilistic fallback,
and both branches return immediately — there is no path where a valid
explicit header is computed and then silently overridden by the bucket
calculation below it. That ordering is what makes an operator's manual
`X-Sample-Decision: true` on one request (to force-capture a trace while
debugging a live incident) reliable: it always wins, regardless of what
bucket that trace ID happens to hash into. Carry this forward: when a
decision has both an explicit override and a computed fallback, structure
the guard so the override always returns before the fallback is even
evaluated, not just before it is applied.

## Resources

- [W3C Trace Context](https://www.w3.org/TR/trace-context/) — the standardized header format this module's `X-Trace-Id` models a simplified version of.
- [OpenTelemetry: Sampling](https://opentelemetry.io/docs/concepts/sampling/) — the head-based probabilistic sampling this module's bucketing implements.
- [hash/fnv](https://pkg.go.dev/hash/fnv) — the deterministic hash used to bucket a trace ID.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-request-coalescing-singleflight.md](27-request-coalescing-singleflight.md) | Next: [29-cron-job-schedule-evaluator.md](29-cron-job-schedule-evaluator.md)
