# Exercise 5: Detect Optional Capabilities With Interface Assertions

Assertions extract concrete types, but they also probe for *capabilities*:
`v.(http.Flusher)` succeeds when the dynamic value implements `Flush()`,
whatever its concrete type. This optional-interface pattern is how the standard
library composes behavior. This exercise builds two instances of it: a renderer
that prefers `encoding.TextMarshaler`, then `fmt.Stringer`, then a fallback; and
a streaming writer that flushes only when the `ResponseWriter` supports it.

This module is fully self-contained: its own module, all code inline, its own
demo and tests.

## What you'll build

```text
capcheck/                    independent module: example.com/capcheck
  go.mod                     go 1.26
  capcheck.go                Render(any) string; StreamLines(http.ResponseWriter, []string) int
  cmd/
    demo/
      main.go                runnable demo: render several values, stream with flush
  capcheck_test.go           render table (marshaler/stringer/both/neither) + flush-count test
```

- Files: `capcheck.go`, `cmd/demo/main.go`, `capcheck_test.go`.
- Implement: `Render(v any) string` asserting to `encoding.TextMarshaler`, else `fmt.Stringer`, else `fmt.Sprintf`; and `StreamLines(w, lines) int` asserting `w` to `http.Flusher`, returning the number of flushes performed.
- Test: a table of values implementing marshaler-only, stringer-only, both, and neither; a fake writer with and without `Flush` proving flushes happen only when supported.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Assertion to an interface is a capability probe

`v.(T)` where `T` is an interface asks whether the dynamic value *implements* `T`,
not whether it equals a specific concrete type. That is what lets a single `any`
be routed by the behaviors it supports. `Render` tries the capabilities in
priority order: `encoding.TextMarshaler` first because a type that defines a
canonical textual encoding (a `net.IP`, a `time.Time`, a UUID) should use it;
then `fmt.Stringer` for a human `String()`; then `fmt.Sprintf("%v", v)` as the
universal fallback that always works. Each step is a comma-ok interface
assertion, and a value implementing *both* interfaces takes the first branch —
priority is encoded purely by the order of the checks.

`StreamLines` shows the same pattern in the exact place the standard library uses
it. An `http.ResponseWriter` is the minimal write interface, but many concrete
writers (the real server's, `httptest.ResponseRecorder`) also implement
`http.Flusher`. A streaming or server-sent-events handler wants to flush after
each chunk so the client sees partial output, but must not assume the capability:
it asserts `w.(http.Flusher)` and flushes only on success, counting the flushes
so the behavior is observable. A writer that is not a `Flusher` still receives
every byte; it just does not get the incremental flush.

Create `capcheck.go`:

```go
// capcheck.go
package capcheck

import (
	"encoding"
	"fmt"
	"net/http"
)

// Render turns any value into text, preferring a canonical TextMarshaler
// encoding, then a Stringer, then the universal fmt fallback.
func Render(v any) string {
	if tm, ok := v.(encoding.TextMarshaler); ok {
		if b, err := tm.MarshalText(); err == nil {
			return string(b)
		}
	}
	if s, ok := v.(fmt.Stringer); ok {
		return s.String()
	}
	return fmt.Sprintf("%v", v)
}

// StreamLines writes each line to w and flushes after each one only if w
// supports flushing. It returns the number of flushes actually performed.
func StreamLines(w http.ResponseWriter, lines []string) int {
	flusher, canFlush := w.(http.Flusher)
	flushes := 0
	for _, line := range lines {
		fmt.Fprintln(w, line)
		if canFlush {
			flusher.Flush()
			flushes++
		}
	}
	return flushes
}
```

### The runnable demo

Create `cmd/demo/main.go`. It defines a `TextMarshaler` type and a `Stringer`
type, renders several values, and streams to an `httptest.ResponseRecorder`
(which implements `http.Flusher`):

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"net/http/httptest"

	"example.com/capcheck"
)

// ipMask marshals to canonical text.
type ipMask struct{ bits int }

func (m ipMask) MarshalText() ([]byte, error) {
	return fmt.Appendf(nil, "/%d", m.bits), nil
}

// level has a human String().
type level int

func (l level) String() string { return fmt.Sprintf("level(%d)", int(l)) }

func main() {
	fmt.Println(capcheck.Render(ipMask{bits: 24})) // TextMarshaler
	fmt.Println(capcheck.Render(level(3)))         // Stringer
	fmt.Println(capcheck.Render(42))               // fallback

	rec := httptest.NewRecorder() // implements http.Flusher
	n := capcheck.StreamLines(rec, []string{"event: ping", "event: pong"})
	fmt.Printf("flushes=%d body=%q\n", n, rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/24
level(3)
42
flushes=2 body="event: ping\nevent: pong\n"
```

### Tests

The render table covers all four capability combinations. The stream test uses
two fake writers — one implementing `http.Flusher`, one not — to prove the flush
path is taken only when the capability is present.

Create `capcheck_test.go`:

```go
// capcheck_test.go
package capcheck

import (
	"fmt"
	"net/http"
	"testing"
)

// marshalOnly implements encoding.TextMarshaler but not fmt.Stringer.
type marshalOnly struct{}

func (marshalOnly) MarshalText() ([]byte, error) { return []byte("marshaled"), nil }

// stringOnly implements fmt.Stringer but not encoding.TextMarshaler.
type stringOnly struct{}

func (stringOnly) String() string { return "stringed" }

// both implements both; the marshaler must win.
type both struct{}

func (both) MarshalText() ([]byte, error) { return []byte("marshaled"), nil }
func (both) String() string               { return "stringed" }

func TestRender(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"marshaler only", marshalOnly{}, "marshaled"},
		{"stringer only", stringOnly{}, "stringed"},
		{"both prefers marshaler", both{}, "marshaled"},
		{"neither falls back", 42, "42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Render(tt.in); got != tt.want {
				t.Fatalf("Render(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// flushRecorder implements http.ResponseWriter and http.Flusher.
type flushRecorder struct {
	header  http.Header
	flushes int
}

func newFlushRecorder() *flushRecorder { return &flushRecorder{header: http.Header{}} }

func (f *flushRecorder) Header() http.Header         { return f.header }
func (f *flushRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (f *flushRecorder) WriteHeader(int)             {}
func (f *flushRecorder) Flush()                      { f.flushes++ }

// plainWriter implements only http.ResponseWriter (no Flush).
type plainWriter struct{ header http.Header }

func newPlainWriter() *plainWriter { return &plainWriter{header: http.Header{}} }

func (p *plainWriter) Header() http.Header         { return p.header }
func (p *plainWriter) Write(b []byte) (int, error) { return len(b), nil }
func (p *plainWriter) WriteHeader(int)             {}

func TestStreamLinesFlush(t *testing.T) {
	t.Parallel()
	lines := []string{"a", "b", "c"}

	fr := newFlushRecorder()
	if n := StreamLines(fr, lines); n != 3 || fr.flushes != 3 {
		t.Fatalf("flusher: StreamLines=%d recorded=%d, want 3 and 3", n, fr.flushes)
	}

	if n := StreamLines(newPlainWriter(), lines); n != 0 {
		t.Fatalf("non-flusher: StreamLines=%d, want 0", n)
	}
}

func ExampleRender() {
	fmt.Println(Render(stringOnly{}))
	// Output: stringed
}
```

## Review

The renderer is correct when priority is exactly TextMarshaler > Stringer >
fallback, verified by the `both` case choosing the marshaled form, and when the
fallback never fails because `fmt.Sprintf` accepts anything. The streamer is
correct when a `Flusher` gets one flush per line and a non-`Flusher` gets zero
while still receiving every byte — that is the whole point of the optional
interface: the capability is used when present and gracefully skipped when
absent, never assumed. The one subtlety is the marshaler branch swallowing a
`MarshalText` error and falling through to the next capability; for a renderer
that is the right call (produce *something*), but a serializer that must not lose
data would return the error instead.

## Resources

- [encoding.TextMarshaler](https://pkg.go.dev/encoding#TextMarshaler) — the `MarshalText() ([]byte, error)` capability.
- [fmt.Stringer](https://pkg.go.dev/fmt#Stringer) — the `String() string` capability.
- [http.Flusher](https://pkg.go.dev/net/http#Flusher) — the optional interface an `http.ResponseWriter` may also implement.
- [Go Specification: Type assertions](https://go.dev/ref/spec#Type_assertions) — asserting to an interface type.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-utf8-safe-truncation.md](06-utf8-safe-truncation.md)
