# Exercise 15: OpenTelemetry Span Collector With Sampling and Baggage Options

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A distributed-trace span collector decides three independent things: whether
to keep a span at all (sampling), which baggage keys are allowed to travel
with it, and which attribute values must never leave the process
(redaction). This module builds that collector through options, and it
checks the one combination that per-option validation cannot: a sample ratio
is only meaningful for one of the three sampling strategies.

## What you'll build

```text
tracespan/                       independent module: example.com/tracespan
  go.mod                         go 1.24
  tracespan.go                   SampleStrategy, Span, Collector, Option, NewCollector,
                                  WithSampleStrategy, WithSampleRatio, WithBaggageKeys,
                                  WithRedactedAttributes, WithRandSource, ShouldSample, StartSpan
  cmd/
    demo/
      main.go                    deterministic sampling sequence, redaction, baggage filtering
  tracespan_test.go               table test over strategy/ratio combos plus behavior tests
```

- Files: `tracespan.go`, `cmd/demo/main.go`, `tracespan_test.go`.
- Implement: `NewCollector(opts ...Option) (*Collector, error)` whose `ShouldSample` picks a span under `AlwaysOn`/`AlwaysOff`/`RatioBased`, and whose `StartSpan` redacts configured attributes and filters baggage to an allow-list.
- Test: every strategy/ratio combination (valid and invalid), the injected-source sampling decision, and attribute redaction plus baggage filtering.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/12-functional-options-pattern/15-distributed-trace-span-options/cmd/demo
cd go-solutions/04-functions/12-functional-options-pattern/15-distributed-trace-span-options
go mod edit -go=1.24
```

### The ratio is only valid for one strategy

`WithSampleRatio` and `WithSampleStrategy` are independent options — either
can run before the other, and neither sees the other's argument. But a
sample ratio only makes sense under `RatioBased`: setting it under
`AlwaysOn` or `AlwaysOff` is a caller mistake worth catching, not a value to
silently ignore. So `NewCollector` tracks whether a ratio was ever set with
a `ratioSet` bool, and after every option has run it checks both directions
of the mismatch: a ratio set under the wrong strategy, and `RatioBased`
chosen without ever supplying one. Neither half of that check could live
inside `WithSampleRatio` or `WithSampleStrategy` alone.

### Injecting the sampling source

`ShouldSample` under `RatioBased` needs a `float64` in `[0,1)` to compare
against the ratio. Reading `math/rand` directly would make the sampling
decision untestable — a real random source can't be forced to prove a
`0.5` ratio keeps roughly half of a fixed, known sequence. `WithRandSource`
injects that source instead, so a test (and the demo) can supply an exact
sequence and assert the exact decision it produces, the same clock-injection
technique used for time elsewhere in this chapter.

Create `tracespan.go`:

```go
package tracespan

import (
	"fmt"
	"sort"
)

// SampleStrategy selects how the Collector decides whether a span is kept.
type SampleStrategy int

const (
	// AlwaysOn samples every span.
	AlwaysOn SampleStrategy = iota
	// AlwaysOff samples no span.
	AlwaysOff
	// RatioBased samples a fraction of spans decided by an injected source.
	RatioBased
)

func (s SampleStrategy) String() string {
	switch s {
	case AlwaysOn:
		return "AlwaysOn"
	case AlwaysOff:
		return "AlwaysOff"
	case RatioBased:
		return "RatioBased"
	default:
		return fmt.Sprintf("SampleStrategy(%d)", int(s))
	}
}

// Span is the redacted, baggage-filtered result of Collector.StartSpan.
type Span struct {
	Name       string
	Attributes map[string]string
	Baggage    map[string]string
}

// Collector decides sampling and shapes spans according to baggage and
// redaction policy.
type Collector struct {
	strategy    SampleStrategy
	ratio       float64
	ratioSet    bool
	baggageKeys map[string]struct{}
	redactAttrs map[string]struct{}
	rand        func() float64
}

// Option configures a Collector and may reject invalid input.
type Option func(*Collector) error

// NewCollector seeds defaults, applies opts in order, then validates the
// cross-field invariant: a sample ratio only makes sense for RatioBased, and
// RatioBased requires one to be set. Neither option alone can see both the
// strategy and whether a ratio was supplied.
func NewCollector(opts ...Option) (*Collector, error) {
	c := &Collector{
		strategy:    AlwaysOn,
		baggageKeys: make(map[string]struct{}),
		redactAttrs: make(map[string]struct{}),
		rand:        defaultRand,
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	if c.ratioSet && c.strategy != RatioBased {
		return nil, fmt.Errorf("sample ratio was set but strategy is %s, not RatioBased", c.strategy)
	}
	if c.strategy == RatioBased && !c.ratioSet {
		return nil, fmt.Errorf("RatioBased strategy requires WithSampleRatio")
	}
	return c, nil
}

// WithSampleStrategy sets the sampling strategy from the closed set of
// named constants.
func WithSampleStrategy(s SampleStrategy) Option {
	return func(c *Collector) error {
		switch s {
		case AlwaysOn, AlwaysOff, RatioBased:
			c.strategy = s
			return nil
		default:
			return fmt.Errorf("unknown sample strategy: %d", int(s))
		}
	}
}

// WithSampleRatio sets the fraction of spans kept under RatioBased, in [0,1].
func WithSampleRatio(r float64) Option {
	return func(c *Collector) error {
		if r < 0 || r > 1 {
			return fmt.Errorf("sample ratio must be in [0,1], got %v", r)
		}
		c.ratio = r
		c.ratioSet = true
		return nil
	}
}

// WithBaggageKeys lists the baggage keys the collector propagates onto spans;
// any key not listed here is dropped.
func WithBaggageKeys(keys ...string) Option {
	return func(c *Collector) error {
		for _, k := range keys {
			if k == "" {
				return fmt.Errorf("baggage key must not be empty")
			}
			c.baggageKeys[k] = struct{}{}
		}
		return nil
	}
}

// WithRedactedAttributes lists attribute names whose values are replaced with
// "REDACTED" on every span.
func WithRedactedAttributes(names ...string) Option {
	return func(c *Collector) error {
		for _, a := range names {
			if a == "" {
				return fmt.Errorf("redacted attribute name must not be empty")
			}
			c.redactAttrs[a] = struct{}{}
		}
		return nil
	}
}

// WithRandSource injects the source used by RatioBased sampling; tests supply
// a deterministic sequence instead of math/rand.
func WithRandSource(fn func() float64) Option {
	return func(c *Collector) error {
		if fn == nil {
			return fmt.Errorf("rand source is nil")
		}
		c.rand = fn
		return nil
	}
}

func defaultRand() float64 { return 0 }

// ShouldSample reports whether the next span should be kept.
func (c *Collector) ShouldSample() bool {
	switch c.strategy {
	case AlwaysOn:
		return true
	case AlwaysOff:
		return false
	default: // RatioBased
		return c.rand() < c.ratio
	}
}

// StartSpan builds a Span, redacting configured attribute names and dropping
// any baggage key not in the allow-list.
func (c *Collector) StartSpan(name string, attrs, baggage map[string]string) *Span {
	outAttrs := make(map[string]string, len(attrs))
	for k, v := range attrs {
		if _, redacted := c.redactAttrs[k]; redacted {
			outAttrs[k] = "REDACTED"
		} else {
			outAttrs[k] = v
		}
	}
	outBaggage := make(map[string]string)
	for k, v := range baggage {
		if _, ok := c.baggageKeys[k]; ok {
			outBaggage[k] = v
		}
	}
	return &Span{Name: name, Attributes: outAttrs, Baggage: outBaggage}
}

// SortedKeys is a small test/demo helper so map output is deterministic.
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
```

### The runnable demo

The demo drives sampling from a fixed two-value sequence instead of
`math/rand`, so the two "sample kept" lines are always the same regardless
of when or how often the demo runs.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tracespan"
)

func main() {
	// A deterministic sequence stands in for math/rand: 0.2, 0.6, 0.2 ...
	seq := []float64{0.2, 0.6}
	i := 0
	source := func() float64 {
		v := seq[i%len(seq)]
		i++
		return v
	}

	c, err := tracespan.NewCollector(
		tracespan.WithSampleStrategy(tracespan.RatioBased),
		tracespan.WithSampleRatio(0.5),
		tracespan.WithRandSource(source),
		tracespan.WithBaggageKeys("tenant.id", "request.id"),
		tracespan.WithRedactedAttributes("password", "authorization"),
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf("sample 1 kept: %t\n", c.ShouldSample())
	fmt.Printf("sample 2 kept: %t\n", c.ShouldSample())

	span := c.StartSpan("checkout",
		map[string]string{"password": "hunter2", "amount": "42"},
		map[string]string{"tenant.id": "acme", "session.secret": "nope"},
	)
	fmt.Printf("attr password: %s\n", span.Attributes["password"])
	fmt.Printf("attr amount: %s\n", span.Attributes["amount"])
	fmt.Printf("baggage keys: %v\n", tracespan.SortedKeys(span.Baggage))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sample 1 kept: true
sample 2 kept: false
attr password: REDACTED
attr amount: 42
baggage keys: [tenant.id]
```

### Tests

`TestNewCollector` tables every strategy/ratio combination, including both
directions of the mismatch (`WithSampleRatio` under `AlwaysOn`/`AlwaysOff`,
and `RatioBased` without a ratio). `TestShouldSampleRatioBased` proves the
`0.5` ratio keeps a value below it and drops one above it, using an injected
two-value sequence instead of real randomness. `TestStartSpanRedactsAndFiltersBaggage`
proves redaction replaces only the configured attribute and baggage
filtering drops any key not on the allow-list.

Create `tracespan_test.go`:

```go
package tracespan

import "testing"

func TestNewCollector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only, AlwaysOn"},
		{name: "ratio with RatioBased is valid", opts: []Option{
			WithSampleStrategy(RatioBased), WithSampleRatio(0.5),
		}},
		{name: "ratio set but AlwaysOn strategy", opts: []Option{
			WithSampleRatio(0.5),
		}, wantErr: true},
		{name: "ratio set but AlwaysOff strategy", opts: []Option{
			WithSampleStrategy(AlwaysOff), WithSampleRatio(0.5),
		}, wantErr: true},
		{name: "RatioBased without a ratio", opts: []Option{
			WithSampleStrategy(RatioBased),
		}, wantErr: true},
		{name: "ratio out of range", opts: []Option{
			WithSampleStrategy(RatioBased), WithSampleRatio(1.5),
		}, wantErr: true},
		{name: "unknown strategy", opts: []Option{
			WithSampleStrategy(SampleStrategy(99)),
		}, wantErr: true},
		{name: "empty baggage key rejected", opts: []Option{
			WithBaggageKeys("ok", ""),
		}, wantErr: true},
		{name: "empty redacted attribute rejected", opts: []Option{
			WithRedactedAttributes(""),
		}, wantErr: true},
		{name: "nil rand source rejected", opts: []Option{
			WithRandSource(nil),
		}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewCollector(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestShouldSampleRatioBased(t *testing.T) {
	t.Parallel()

	seq := []float64{0.1, 0.9}
	i := 0
	c, err := NewCollector(
		WithSampleStrategy(RatioBased),
		WithSampleRatio(0.5),
		WithRandSource(func() float64 {
			v := seq[i]
			i++
			return v
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	if !c.ShouldSample() {
		t.Fatal("expected sample kept when rand() < ratio")
	}
	if c.ShouldSample() {
		t.Fatal("expected sample dropped when rand() >= ratio")
	}
}

func TestStartSpanRedactsAndFiltersBaggage(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(
		WithBaggageKeys("tenant.id"),
		WithRedactedAttributes("password"),
	)
	if err != nil {
		t.Fatal(err)
	}

	span := c.StartSpan("op",
		map[string]string{"password": "secret", "amount": "10"},
		map[string]string{"tenant.id": "acme", "other": "drop-me"},
	)

	if span.Attributes["password"] != "REDACTED" {
		t.Fatalf("password = %q, want REDACTED", span.Attributes["password"])
	}
	if span.Attributes["amount"] != "10" {
		t.Fatalf("amount = %q, want 10", span.Attributes["amount"])
	}
	if _, ok := span.Baggage["other"]; ok {
		t.Fatal("baggage key not in allow-list leaked through")
	}
	if span.Baggage["tenant.id"] != "acme" {
		t.Fatalf("tenant.id = %q, want acme", span.Baggage["tenant.id"])
	}
}
```

## Review

The collector is correct when a sample ratio can only exist alongside the
one strategy that reads it, and when redaction and baggage filtering never
leak a value they were configured to hide. The `ratioSet` bool is the
general technique for catching "this option was given, but a different
option makes it meaningless" — a mismatch neither option's closure can see
by itself, only the constructor after every option has run. Injecting the
sampling source rather than calling `math/rand` directly is what makes a
probabilistic decision assertable at all: the test fixes the sequence and
proves the exact boundary the ratio is supposed to draw.

## Resources

- [OpenTelemetry: Sampling](https://opentelemetry.io/docs/concepts/sampling/)
- [W3C Baggage specification](https://www.w3.org/TR/baggage/)
- [pkg.go.dev: math/rand](https://pkg.go.dev/math/rand)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-paginated-lister-options.md](14-paginated-lister-options.md) | Next: [16-circuit-breaker-state-machine.md](16-circuit-breaker-state-machine.md)
