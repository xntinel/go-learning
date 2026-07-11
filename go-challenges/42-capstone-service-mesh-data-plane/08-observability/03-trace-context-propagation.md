# Exercise 3: Trace Context Propagation

A distributed trace only holds together if every hop shares one trace identifier, and the proxy's job is to keep that thread unbroken: read the incoming W3C `traceparent`, continue its trace, mint a fresh span id for its own hop, and write the updated context downstream. This exercise builds that propagation from scratch — parsing and rendering the `traceparent` header with full validation, the proxy hop rule, and carrying the span context on a Go `context.Context` through the in-process call chain.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
tracing.go           SpanContext, ParseTraceparent/Traceparent, Inject/Extract, Propagate, context helpers
cmd/
  demo/
    main.go          a request arrives with a traceparent; the proxy propagates a new hop downstream
tracing_test.go      header round-trip, malformed-header rejection, hop trace-id preservation, context round-trip
```

- Files: `tracing.go`, `cmd/demo/main.go`, `tracing_test.go`.
- Implement: `SpanContext`, `ParseTraceparent` / `Traceparent`, `NewSpanContext`, `Inject` / `Extract`, `Propagate`, and `ContextWithSpan` / `SpanFromContext`.
- Test: assert the propagated outbound header preserves the trace id, replaces the span id, and rejects every malformed `traceparent`.
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p trace-context/cmd/demo && cd trace-context
go mod init example.com/trace-context
```

### The traceparent wire format and why each field is validated

W3C Trace Context defines one header, `traceparent`, with four hyphen-separated fields: `version-traceid-spanid-traceflags`. A live example is `00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`. The version is `00` (the only version this code emits); the trace id is 16 bytes rendered as 32 lowercase hex chars and is shared by every hop of one logical request; the parent/span id is 8 bytes as 16 hex chars and is unique to the hop that wrote the header; the trace flags are one byte (2 hex chars) whose low bit is the sampled flag. The spec forbids an all-zero trace id or span id, because all-zero is the sentinel for "no id."

`ParseTraceparent` validates all of this before trusting a single field, and the validation is not pedantry — the header arrives from another service across the network and may be truncated, hand-edited, or hostile. It checks for exactly four fields, the supported version, the precise hex widths (32 / 16 / 2), valid hex decoding, and the non-zero prohibition. Any failure returns an error; a value that fails parsing is treated by `Extract` as no inbound context at all, so a corrupt header degrades to "start a fresh root" rather than propagating corruption or panicking on a short slice. `Traceparent` renders the reverse, choosing `01` or `00` for the flags from the `Sampled` bool, and the round-trip `Parse(Render(sc)) == sc` is what the first test pins.

### The proxy hop rule: keep the trace, change the span

`Propagate` is the whole point of the exercise, and the rule is exact. Extract the inbound `traceparent`. If it is present and valid, keep its trace id and replace only the span id with a fresh one for this hop (`WithSpanID`); the new span becomes a child of the inbound span within the same trace. If there is no valid inbound context, start a brand-new root trace with the supplied new trace id and span id. Either way the resulting span context is injected into the outbound header and returned to the caller. The single most damaging mistake here is generating a new trace id on every request unconditionally: that severs the trace at the proxy, so the upstream caller's trace and everything downstream of the proxy appear as two unrelated traces and the end-to-end view is lost. Keeping the inbound trace id is what makes the proxy a transparent participant rather than a trace boundary.

`NewSpanContext` mints a root from an `io.Reader` of randomness — pass `crypto/rand.Reader` in production, or a fixed reader in a test for a deterministic id — reading 16 bytes for the trace id and 8 for the span id and setting the sampled flag. The ids in `Propagate` are passed in explicitly rather than generated internally so the hop logic stays deterministic and testable; a real proxy would fill them from `NewSpanContext` or a per-hop generator.

### Carrying the span in-process on context.Context

Across the process boundary the span travels in the header; inside the process it travels on `context.Context`. `ContextWithSpan` stashes the `SpanContext` under an unexported key type (`ctxKey{}`, unexported so no other package can collide with it), and `SpanFromContext` retrieves it. This is the idiomatic Go pattern for request-scoped data: the span rides along the call chain — through the handler, the load balancer, the upstream dialer — without being threaded through every function signature, and any layer that wants to annotate a log line or start a child span pulls it back off the context. The header is the cross-service carrier; the context is the in-process carrier; `Propagate` plus `ContextWithSpan` is how a request moves from one to the other at the proxy's ingress.

Create `tracing.go`:

```go
package tracing

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
)

// Version is the only W3C trace-context version this implementation emits.
const Version = "00"

// HeaderName is the W3C trace-context propagation header.
const HeaderName = "traceparent"

// SpanContext identifies one span within a distributed trace. TraceID is shared
// by every hop of a single request; SpanID is unique to this hop. Sampled is the
// low bit of the trace-flags field.
type SpanContext struct {
	TraceID [16]byte
	SpanID  [8]byte
	Sampled bool
}

// Traceparent renders the W3C traceparent header value:
//
//	version "-" trace-id "-" parent-id "-" trace-flags
//	00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
func (sc SpanContext) Traceparent() string {
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return fmt.Sprintf("%s-%s-%s-%s",
		Version,
		hex.EncodeToString(sc.TraceID[:]),
		hex.EncodeToString(sc.SpanID[:]),
		flags)
}

// WithSpanID returns a copy of sc with a new span id and the same trace id and
// flags. This is exactly what a proxy does on each hop: keep the trace id,
// allocate a fresh span id for the work it is about to do.
func (sc SpanContext) WithSpanID(id [8]byte) SpanContext {
	sc.SpanID = id
	return sc
}

// ParseTraceparent parses a W3C traceparent header value. It rejects any value
// that is not exactly four hyphen-separated fields, that uses an unsupported
// version, that has the wrong field widths, that is not valid hex, or whose
// trace id or span id is all zeros (forbidden by the spec).
func ParseTraceparent(s string) (SpanContext, error) {
	var sc SpanContext
	// Manual split avoids importing strings just for one SplitN.
	fields := splitDashes(s)
	if len(fields) != 4 {
		return sc, fmt.Errorf("tracing: traceparent must have 4 fields, got %d", len(fields))
	}
	version, traceHex, spanHex, flagsHex := fields[0], fields[1], fields[2], fields[3]
	if version != Version {
		return sc, fmt.Errorf("tracing: unsupported version %q", version)
	}
	if len(traceHex) != 32 {
		return sc, fmt.Errorf("tracing: trace-id must be 32 hex chars, got %d", len(traceHex))
	}
	if len(spanHex) != 16 {
		return sc, fmt.Errorf("tracing: span-id must be 16 hex chars, got %d", len(spanHex))
	}
	if len(flagsHex) != 2 {
		return sc, fmt.Errorf("tracing: trace-flags must be 2 hex chars, got %d", len(flagsHex))
	}
	tid, err := hex.DecodeString(traceHex)
	if err != nil {
		return sc, fmt.Errorf("tracing: trace-id not hex: %w", err)
	}
	sid, err := hex.DecodeString(spanHex)
	if err != nil {
		return sc, fmt.Errorf("tracing: span-id not hex: %w", err)
	}
	flags, err := hex.DecodeString(flagsHex)
	if err != nil {
		return sc, fmt.Errorf("tracing: trace-flags not hex: %w", err)
	}
	copy(sc.TraceID[:], tid)
	copy(sc.SpanID[:], sid)
	sc.Sampled = flags[0]&0x01 == 0x01
	if isZero(sc.TraceID[:]) {
		return sc, fmt.Errorf("tracing: trace-id is all zeros")
	}
	if isZero(sc.SpanID[:]) {
		return sc, fmt.Errorf("tracing: span-id is all zeros")
	}
	return sc, nil
}

// NewSpanContext reads 24 random bytes from r to mint a fresh root span: 16 for
// the trace id and 8 for the span id, with the sampled flag set. Pass
// crypto/rand.Reader in production; pass a fixed reader in tests for
// determinism.
func NewSpanContext(r io.Reader) (SpanContext, error) {
	var sc SpanContext
	if _, err := io.ReadFull(r, sc.TraceID[:]); err != nil {
		return sc, fmt.Errorf("tracing: read trace-id: %w", err)
	}
	if _, err := io.ReadFull(r, sc.SpanID[:]); err != nil {
		return sc, fmt.Errorf("tracing: read span-id: %w", err)
	}
	sc.Sampled = true
	return sc, nil
}

// Inject writes sc into h as a traceparent header, replacing any existing value.
func Inject(h http.Header, sc SpanContext) {
	h.Set(HeaderName, sc.Traceparent())
}

// Extract parses the traceparent header from h. The bool is false when the
// header is absent or malformed.
func Extract(h http.Header) (SpanContext, bool) {
	v := h.Get(HeaderName)
	if v == "" {
		return SpanContext{}, false
	}
	sc, err := ParseTraceparent(v)
	if err != nil {
		return SpanContext{}, false
	}
	return sc, true
}

// Propagate performs one proxy hop. If in carries a valid traceparent, the
// trace id is continued and a new span id (newSpanID) identifies this hop;
// otherwise a brand-new root trace is started with newTraceID/newSpanID. The
// resulting span context is injected into out and returned so the caller can
// store it on a context or in an access log.
func Propagate(in, out http.Header, newTraceID [16]byte, newSpanID [8]byte) SpanContext {
	parent, ok := Extract(in)
	var child SpanContext
	if ok {
		child = parent.WithSpanID(newSpanID)
	} else {
		child = SpanContext{TraceID: newTraceID, SpanID: newSpanID, Sampled: true}
	}
	Inject(out, child)
	return child
}

type ctxKey struct{}

// ContextWithSpan returns a child context carrying sc. This is how the span
// rides along the in-process call chain without threading it through every
// function signature.
func ContextWithSpan(ctx context.Context, sc SpanContext) context.Context {
	return context.WithValue(ctx, ctxKey{}, sc)
}

// SpanFromContext returns the span carried by ctx, if any.
func SpanFromContext(ctx context.Context) (SpanContext, bool) {
	sc, ok := ctx.Value(ctxKey{}).(SpanContext)
	return sc, ok
}

// splitDashes splits on '-' without pulling in the strings package.
func splitDashes(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
```

Read `Propagate` as the hinge of the exercise: `Extract` decides whether there is a trace to continue, `WithSpanID` continues it while swapping in this hop's span, and `Inject` writes the result downstream. `ParseTraceparent` is deliberately strict because it sits at the network boundary; the width and hex and zero checks are what stop a malformed header from becoming a downstream corruption.

### The runnable demo

The demo plays the proxy's role: a request arrives already carrying a `traceparent` from an edge gateway, the proxy allocates a fresh span id for its own hop and propagates the context to the upstream, and the output shows the trace id preserved while the span id changes. Fixed ids make the output exact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"

	"example.com/trace-context"
)

func main() {
	// A request arrives at the proxy already carrying a trace context from an
	// upstream caller (an edge gateway, say).
	inbound := http.Header{}
	inbound.Set(tracing.HeaderName, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	fmt.Println("inbound  traceparent:", inbound.Get(tracing.HeaderName))

	// The proxy allocates a fresh span id for its own hop and propagates the
	// context downstream. The trace id is preserved; the span id changes.
	hopSpanID := [8]byte{0xb7, 0xad, 0x6b, 0x71, 0x69, 0x20, 0x33, 0x31}
	outbound := http.Header{}
	child := tracing.Propagate(inbound, outbound, [16]byte{}, hopSpanID)

	fmt.Println("outbound traceparent:", outbound.Get(tracing.HeaderName))
	fmt.Printf("trace id preserved:   %t\n", child.TraceID == mustTrace(inbound))
	fmt.Printf("span id is this hop:  %x\n", child.SpanID)
}

func mustTrace(h http.Header) [16]byte {
	sc, _ := tracing.Extract(h)
	return sc.TraceID
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
inbound  traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
outbound traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-b7ad6b7169203331-01
trace id preserved:   true
span id is this hop:  b7ad6b7169203331
```

### Tests

The tests pin the wire format, the validation, and the hop rule. `TestTraceparentRoundTrip` parses a known header and renders it back unchanged. `TestParseTraceparentRejects` sweeps a table of malformed headers — wrong field count, bad version, wrong widths, non-hex, all-zero ids — and asserts each is rejected. `TestPropagateContinuesTrace` is the core assertion on propagated headers: the trace id is preserved across the hop, the span id is replaced with the new one, and the outbound header carries the child context. `TestPropagateStartsRootWhenNoInbound` covers the fresh-root branch, `TestNewSpanContextIsDeterministicWithFixedReader` pins generation against a fixed reader, and `TestContextRoundTrip` checks the in-process carrier.

Create `tracing_test.go`:

```go
package tracing

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

func TestTraceparentRoundTrip(t *testing.T) {
	t.Parallel()
	want := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sc, err := ParseTraceparent(want)
	if err != nil {
		t.Fatalf("ParseTraceparent: %v", err)
	}
	if !sc.Sampled {
		t.Error("Sampled = false, want true (flags=01)")
	}
	if got := sc.Traceparent(); got != want {
		t.Fatalf("round-trip = %q, want %q", got, want)
	}
}

func TestParseTraceparentRejects(t *testing.T) {
	t.Parallel()
	bad := []string{
		"",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",    // 3 fields
		"99-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", // bad version
		"00-tooShort-00f067aa0ba902b7-01",                         // trace-id width
		"00-4bf92f3577b34da6a3ce929d0e0e4736-shrt-01",             // span-id width
		"00-zzzz2f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", // non-hex
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01", // zero trace-id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", // zero span-id
	}
	for _, s := range bad {
		if _, err := ParseTraceparent(s); err == nil {
			t.Errorf("ParseTraceparent(%q) = nil error, want rejection", s)
		}
	}
}

func TestPropagateContinuesTrace(t *testing.T) {
	t.Parallel()
	in := http.Header{}
	in.Set(HeaderName, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	out := http.Header{}

	newSpan := [8]byte{0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8}
	child := Propagate(in, out, [16]byte{}, newSpan)

	parent, _ := Extract(in)
	// Trace id must be preserved across the hop.
	if child.TraceID != parent.TraceID {
		t.Fatalf("trace id changed across hop: %x -> %x", parent.TraceID, child.TraceID)
	}
	// Span id must be the new one, not the parent's.
	if child.SpanID == parent.SpanID {
		t.Fatal("span id was not replaced for this hop")
	}
	if child.SpanID != newSpan {
		t.Fatalf("span id = %x, want %x", child.SpanID, newSpan)
	}
	// The outbound header must carry the child context.
	want := child.Traceparent()
	if got := out.Get(HeaderName); got != want {
		t.Fatalf("injected header = %q, want %q", got, want)
	}
}

func TestPropagateStartsRootWhenNoInbound(t *testing.T) {
	t.Parallel()
	in := http.Header{}
	out := http.Header{}
	newTrace := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	newSpan := [8]byte{9, 9, 9, 9, 9, 9, 9, 9}

	child := Propagate(in, out, newTrace, newSpan)
	if child.TraceID != newTrace {
		t.Fatalf("root trace id = %x, want %x", child.TraceID, newTrace)
	}
	if out.Get(HeaderName) == "" {
		t.Fatal("no traceparent injected for a fresh root span")
	}
}

func TestNewSpanContextIsDeterministicWithFixedReader(t *testing.T) {
	t.Parallel()
	// 24 bytes: 16 for the trace id, 8 for the span id.
	seed := bytes.Repeat([]byte{0xAB}, 24)
	sc, err := NewSpanContext(bytes.NewReader(seed))
	if err != nil {
		t.Fatal(err)
	}
	want := "00-abababababababababababababababab-abababababababab-01"
	if got := sc.Traceparent(); got != want {
		t.Fatalf("traceparent = %q, want %q", got, want)
	}
}

func TestContextRoundTrip(t *testing.T) {
	t.Parallel()
	sc := SpanContext{TraceID: [16]byte{1}, SpanID: [8]byte{2}, Sampled: true}
	ctx := ContextWithSpan(context.Background(), sc)
	got, ok := SpanFromContext(ctx)
	if !ok {
		t.Fatal("SpanFromContext: no span")
	}
	if got != sc {
		t.Fatalf("span = %+v, want %+v", got, sc)
	}
	if _, ok := SpanFromContext(context.Background()); ok {
		t.Fatal("empty context should carry no span")
	}
}
```

## Review

Propagation is correct when the trace id survives every hop and only the span id changes. The defining test is `TestPropagateContinuesTrace`: it asserts `child.TraceID == parent.TraceID` (the thread is unbroken), `child.SpanID != parent.SpanID` and equals the new hop id (this hop is a distinct span), and that the outbound header carries the rendered child. The most consequential mistake is minting a new trace id unconditionally — that severs the trace at the proxy and the request fragments into unrelated traces; keep the inbound trace id whenever a valid `traceparent` is present. The second is trusting the header without validation: `ParseTraceparent` must check the field count, version, exact hex widths, hex validity, and the all-zero prohibition before copying any bytes, and `TestParseTraceparentRejects` pins each of those. Note that `Extract` deliberately swallows a parse error into "no inbound context," so a corrupt inbound header degrades gracefully to a fresh root rather than propagating garbage. The in-process side is the `context.Context` pair: the unexported `ctxKey` type prevents collisions with other packages' context values, and `TestContextRoundTrip` confirms a stored span comes back and an empty context yields none.

## Resources

- [W3C Trace Context Recommendation](https://www.w3.org/TR/trace-context/) — the authoritative specification for the `traceparent` header, its four fields, hex widths, and the all-zero prohibition this code enforces.
- [`context`](https://pkg.go.dev/context) — `context.WithValue` and the unexported-key pattern used to carry the span context through the in-process call chain.
- [`encoding/hex`](https://pkg.go.dev/encoding/hex) — the hex encode/decode used to render and parse the trace and span ids.
- [OpenTelemetry: Context propagation](https://opentelemetry.io/docs/concepts/context-propagation/) — how production tracing systems propagate context across services, the standard this minimal implementation is a slice of.

---

Back to [02-structured-access-logs.md](02-structured-access-logs.md) | Next: [../09-control-plane-grpc/00-concepts.md](../09-control-plane-grpc/00-concepts.md)
