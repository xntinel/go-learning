# Exercise 6: Guarding Optional Dependencies — Logger, Metrics, Tracer

Cross-cutting collaborators are frequently optional: a logger and a metrics
recorder are present in production but nil in a unit test or a minimal
deployment. This module builds a service that stays safe either way, comparing
two strategies — the nil-receiver guard for metrics and the null-object pattern
for the logger — and showing when each wins.

This module is fully self-contained.

## What you'll build

```text
observ/                   independent module: example.com/observ
  go.mod                  go 1.24
  service.go              Logger iface + nopLogger; Metrics (nil-safe receiver); Service
  cmd/
    demo/
      main.go             runnable demo: nil deps vs real deps, identical output
  service_test.go         identical business output, real records, nop records nothing
```

Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
Implement: a `Metrics` whose methods guard a nil receiver (the nil pointer is the no-op); a `Logger` interface with a `nopLogger` injected by the constructor when nil; a `Service.Handle` that calls both unconditionally.
Test: the service built with nil metrics and with a real recorder produces identical business output; the real recorder captures the expected counter while the nil/nop path records nothing; no panic on the nil-collaborator path.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Two strategies for a possibly-nil collaborator

The naive approach guards at every call site: `if s.metrics != nil {
s.metrics.Inc("requests") }`. It works but it is noisy and one missed guard
panics under load. Two better strategies remove the per-call-site check.

Strategy one, the nil-receiver guard, makes nil itself safe. A pointer-receiver
method that checks `if m == nil` and returns is a no-op when the pointer is nil,
so `Service` can call `s.metrics.Inc(...)` unconditionally even when `metrics` is
nil. The nil `*Metrics` *is* the null-object; no separate no-op type is needed.

Strategy two, the null-object pattern, applies when the dependency is an
interface. You cannot give a nil interface a safe method (calling a method on a
nil interface value panics with no receiver to dispatch on), so instead the
constructor substitutes a no-op implementation when the caller passes nil:

```go
func NewService(m *Metrics, log Logger) *Service {
	if log == nil {
		log = nopLogger{} // null-object: every Log call is now safe
	}
	return &Service{metrics: m, log: log}
}
```

After construction, `Service` calls `s.log.Log(...)` unconditionally; the field
is never a nil interface. The trade-off: the null-object costs one tiny value
(here a zero-size struct) but eliminates every downstream nil check and every
"forgot to guard" panic. For optional cross-cutting dependencies that is almost
always the right call. The nil-receiver guard is the cheaper option when you own
the concrete type and can make its methods nil-safe; the null-object is the
option when the dependency is an interface you dispatch through.

Create `service.go`:

```go
package observ

import "sync"

// Logger is an optional collaborator. Calls go through the interface, so a nil
// Logger must be replaced by a no-op (nopLogger) at construction — you cannot
// call a method on a nil interface value.
type Logger interface {
	Log(msg string)
}

// nopLogger is the null-object: a Logger whose Log does nothing.
type nopLogger struct{}

func (nopLogger) Log(string) {}

// Recorder is a real Logger that captures messages, for tests and demos.
type Recorder struct {
	mu       sync.Mutex
	Messages []string
}

func (r *Recorder) Log(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Messages = append(r.Messages, msg)
}

// Metrics is a concrete collaborator whose methods guard a nil receiver, so a
// nil *Metrics is itself a safe no-op (no separate null-object type needed).
type Metrics struct {
	mu     sync.Mutex
	counts map[string]int
}

// NewMetrics returns a live metrics recorder.
func NewMetrics() *Metrics {
	return &Metrics{counts: make(map[string]int)}
}

// Inc increments name's counter. It is a no-op on a nil *Metrics.
func (m *Metrics) Inc(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[name]++
}

// Get returns name's counter, or 0 on a nil *Metrics.
func (m *Metrics) Get(name string) int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[name]
}

// Service depends on an optional *Metrics and a Logger. Its request path calls
// both unconditionally; safety comes from the nil-receiver guard on Metrics and
// the null-object substitution for Logger.
type Service struct {
	metrics *Metrics
	log     Logger
}

// NewService builds a Service, substituting a no-op logger when log is nil. The
// metrics pointer may be nil; its methods are nil-safe.
func NewService(m *Metrics, log Logger) *Service {
	if log == nil {
		log = nopLogger{}
	}
	return &Service{metrics: m, log: log}
}

// Handle processes a request. Business output does not depend on whether the
// observability collaborators are real or absent.
func (s *Service) Handle(req string) string {
	s.log.Log("handling " + req)
	s.metrics.Inc("requests")
	return "handled:" + req
}
```

### The runnable demo

The demo builds the service twice — once with both collaborators nil, once with
real ones — and shows the business output is identical while only the real
metrics recorded a count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/observ"
)

func main() {
	// Minimal deployment: no metrics, no logger.
	bare := observ.NewService(nil, nil)
	fmt.Println(bare.Handle("ping"))

	// Full deployment: real collaborators.
	m := observ.NewMetrics()
	full := observ.NewService(m, &observ.Recorder{})
	fmt.Println(full.Handle("ping"))
	fmt.Printf("requests counted: %d\n", m.Get("requests"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
handled:ping
handled:ping
requests counted: 1
```

### Tests

The tests prove the central property: the request path yields identical business
output whether the collaborators are nil or real, and the real recorders capture
what is expected while the nil/nop path records nothing and never panics.

Create `service_test.go`:

```go
package observ

import (
	"testing"
)

func TestBusinessOutputIndependentOfObservability(t *testing.T) {
	t.Parallel()

	bare := NewService(nil, nil)
	full := NewService(NewMetrics(), &Recorder{})

	if got, want := bare.Handle("ping"), "handled:ping"; got != want {
		t.Fatalf("bare = %q, want %q", got, want)
	}
	if got, want := full.Handle("ping"), "handled:ping"; got != want {
		t.Fatalf("full = %q, want %q", got, want)
	}
}

func TestNilCollaboratorsRecordNothing(t *testing.T) {
	t.Parallel()

	s := NewService(nil, nil) // nil metrics, nil logger -> nopLogger
	s.Handle("a")
	s.Handle("b")
	// No panic reaching here is the point; nothing was recorded anywhere.
}

func TestRealCollaboratorsRecord(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	rec := &Recorder{}
	s := NewService(m, rec)

	s.Handle("a")
	s.Handle("b")

	if got := m.Get("requests"); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if got := len(rec.Messages); got != 2 {
		t.Fatalf("log messages = %d, want 2", got)
	}
	if rec.Messages[0] != "handling a" {
		t.Fatalf("first message = %q, want %q", rec.Messages[0], "handling a")
	}
}

func TestNilMetricsGetIsZero(t *testing.T) {
	t.Parallel()

	var m *Metrics // nil
	if got := m.Get("requests"); got != 0 {
		t.Fatalf("nil Metrics Get = %d, want 0", got)
	}
	m.Inc("requests") // no-op, must not panic
}
```

## Review

The service is correct when `Handle` produces the same business result and never
panics regardless of whether metrics and logger are present. The decisive test is
`TestBusinessOutputIndependentOfObservability`: identical output from the bare
and full services proves observability is a side channel, not part of the result.
`TestRealCollaboratorsRecord` proves the real path still records, so the no-op
strategy did not silently disable everything.

The mistake avoided: leaving `Handle` to call a nil interface's method (panic) or
sprinkling `if s.metrics != nil` at every call site. The nil-receiver guard for
the concrete `*Metrics` and the null-object substitution for the `Logger`
interface remove both.

## Resources

- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces) — how interface method dispatch works and why a nil interface has no receiver.
- [Uber Go Style Guide](https://github.com/uber-go/guide/blob/master/style.md) — idioms on nil receivers, interfaces, and avoiding unnecessary nil checks.
- [Go Wiki: Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) — guidance on interfaces and receivers.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-nil-map-slice-accumulator-guard.md](05-nil-map-slice-accumulator-guard.md) | Next: [07-atomic-pointer-hot-config-reload.md](07-atomic-pointer-hot-config-reload.md)
