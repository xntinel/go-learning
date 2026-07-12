# Exercise 13: An Instrumentation Decorator with Injected Counters

**Nivel: Intermedio** — validacion rapida (un test corto).

Wrapping an operation to count how often it runs and how often it fails is
the same decorator shape as retry or timeout: `Operation -> Operation`.
The senior move is injecting the counters as plain functions instead of
reaching for a concrete metrics client inside the decorator, so the same
`Instrument` works whether the backend is StatsD, Prometheus, or — in this
exercise — a map a test can assert on directly.

## What you'll build

```text
instrument/                 independent module: example.com/instrument
  go.mod                    go 1.24
  instrument.go             type Operation, Counter; func Instrument
  instrument_test.go        call/error counts across success and failure mixes
```

- Files: `instrument.go`, `instrument_test.go`.
- Implement: `Operation func() error`, `Counter func(name string)`, and `Instrument(name string, op Operation, incCalls, incErrors Counter) Operation`.
- Test: every invocation increments the call counter regardless of outcome; only failing invocations increment the error counter; a mixed sequence of successes and failures produces the exact expected totals; the wrapped operation's return value is passed through unchanged.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/13-instrumentation-decorator
cd go-solutions/04-functions/10-higher-order-functions/13-instrumentation-decorator
go mod edit -go=1.24
```

### Inject the metrics sink, don't import one

A decorator that calls `prometheus.CounterVec.WithLabelValues(name).Inc()`
directly is coupled to Prometheus, untestable without standing up a
registry, and unusable in a project that ships StatsD instead. `Counter`
is `func(name string)` — the smallest possible seam. Production wires it to
whatever client is in use with a one-line adapter; the test wires it to a
map. `Instrument` itself never imports a metrics package at all.

Create `instrument.go`:

```go
package instrument

// Operation is a unit of work with no result beyond success or failure —
// the shape of a repository call, a message handler, or a use case.
type Operation func() error

// Counter records one occurrence for the named metric. Production wires
// this to whatever metrics client is in use (a StatsD client, a
// Prometheus CounterVec's Inc); tests wire it to a plain map so counts are
// assertable without standing up a metrics backend.
type Counter func(name string)

// Instrument decorates op with call and error counting. Every invocation
// increments name via incCalls before running op; if op returns a non-nil
// error, name is also passed to incErrors after op returns. Both counters
// are injected rather than reaching for a package-global metrics client,
// so the decorator has no dependency on any particular metrics library and
// is fully testable with plain functions.
func Instrument(name string, op Operation, incCalls, incErrors Counter) Operation {
	return func() error {
		incCalls(name)
		err := op()
		if err != nil {
			incErrors(name)
		}
		return err
	}
}
```

### Tests

`counters` is a two-map stand-in for a real metrics backend — just enough
to assert on. `TestInstrument` drives three shapes through the same
harness: all successes, a single failure, and a mixed sequence, checking
that calls always increments and errors increments only on failure.
`TestInstrumentPropagatesResult` checks the wrapped function's return value
still reaches the caller unchanged.

Create `instrument_test.go`:

```go
package instrument

import (
	"errors"
	"testing"
)

// counters is a tiny in-memory stand-in for a metrics backend: enough to
// assert on without pulling in any real metrics library.
type counters struct {
	calls  map[string]int
	errors map[string]int
}

func newCounters() *counters {
	return &counters{calls: make(map[string]int), errors: make(map[string]int)}
}

func (c *counters) incCalls(name string)  { c.calls[name]++ }
func (c *counters) incErrors(name string) { c.errors[name]++ }

func TestInstrument(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("insert failed")

	tests := []struct {
		name        string
		invocations []error // one entry per call to the wrapped op, nil = success
		wantCalls   int
		wantErrors  int
	}{
		{
			name:        "every success still counts as a call, never an error",
			invocations: []error{nil, nil, nil},
			wantCalls:   3,
			wantErrors:  0,
		},
		{
			name:        "a failing call counts as both a call and an error",
			invocations: []error{errBoom},
			wantCalls:   1,
			wantErrors:  1,
		},
		{
			name:        "mixed results count calls and errors independently",
			invocations: []error{nil, errBoom, nil, errBoom, errBoom},
			wantCalls:   5,
			wantErrors:  3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := newCounters()
			i := 0
			op := Instrument("CreateOrder", func() error {
				err := tc.invocations[i]
				i++
				return err
			}, c.incCalls, c.incErrors)

			for range tc.invocations {
				_ = op()
			}

			if got := c.calls["CreateOrder"]; got != tc.wantCalls {
				t.Errorf("calls[CreateOrder] = %d, want %d", got, tc.wantCalls)
			}
			if got := c.errors["CreateOrder"]; got != tc.wantErrors {
				t.Errorf("errors[CreateOrder] = %d, want %d", got, tc.wantErrors)
			}
		})
	}
}

func TestInstrumentPropagatesResult(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	c := newCounters()

	ok := Instrument("Ping", func() error { return nil }, c.incCalls, c.incErrors)
	if err := ok(); err != nil {
		t.Errorf("ok() = %v, want nil", err)
	}

	fail := Instrument("Ping", func() error { return errBoom }, c.incCalls, c.incErrors)
	if err := fail(); !errors.Is(err, errBoom) {
		t.Errorf("fail() = %v, want errBoom", err)
	}
}
```

## Review

`Instrument` is correct when `incCalls` runs unconditionally and
`incErrors` runs only behind the `err != nil` check — the entire contract
is those two lines. The `counters` helper is the reason the test needs
nothing beyond the standard library: because the decorator depends on
`Counter`, a `func(string)`, rather than a concrete metrics type, the test
can substitute the cheapest possible implementation and assert on exact
integers instead of scraping a real metrics registry. The same
`Instrument` value drops around any `Operation` — a repository call, a
queue consumer, a scheduled job — without changing a line of the decorator
itself.

## Resources

- [Prometheus client_golang: CounterVec](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus#CounterVec) — the real backend `Counter` would be adapted to in production.
- [Effective Go: Interfaces and other types](https://go.dev/doc/effective_go#interfaces_and_types) — why depending on a function type instead of a concrete client keeps a decorator swappable.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-predicate-combinators-access-rules.md](12-predicate-combinators-access-rules.md) | Next: [14-fallback-lookup-combinator.md](14-fallback-lookup-combinator.md)
