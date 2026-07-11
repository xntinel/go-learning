# Exercise 18: Probabilistic Sampler Decorator for Observability

**Nivel: Intermedio** — validacion rapida (un test corto).

Tracing every request in a high-throughput service is expensive: the
storage and CPU cost of a trace for every call can dwarf the cost of the
call itself. A probabilistic sampler decorator solves this by tracing only
a configurable fraction of calls — enough to see trends and catch
anomalies, without the overhead of tracing everything.

## What you'll build

```text
sampler/                     independent module: example.com/sampler
  go.mod                     go 1.24
  sampler.go                 type Source; func New
  sampler_test.go            boundary behavior, exact scripted sequence, one draw per call
```

- Files: `sampler.go`, `sampler_test.go`.
- Implement: `Source func() float64` and `New(rate float64, src Source, onSample func()) func()`.
- Test: a draw strictly below `rate` fires `onSample`, a draw at or above it does not; `rate == 0` never fires and `rate == 1` fires for any draw below 1; the returned closure consults `src` exactly once per call.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/sampler
cd ~/go-exercises/sampler
go mod init example.com/sampler
go mod edit -go=1.24
```

### Inject the randomness, don't call math/rand directly

A sampler that calls `rand.Float64()` inline is exactly as untestable as a
retry loop that calls `time.Sleep` inline: the outcome depends on a hidden
global generator, so a test can assert only a statistical trend across
thousands of runs, never an exact decision. `Source` is the seam:
production passes `rand.Float64` from a properly seeded `*rand.Rand`
(entropy-seeded, so real traffic samples unpredictably); a test passes a
scripted function that plays back an exact sequence of values, so the
sample/skip decision at each call is asserted precisely instead of
statistically.

`New` itself is three lines: draw one value from `src`, compare it against
`rate`, and call `onSample` if the draw is strictly less than `rate`. The
strict less-than is what makes `rate == 0` sample nothing (no draw in
`[0, 1)` is less than `0`) and `rate == 1` sample everything (every draw in
`[0, 1)` is less than `1`) — both useful escape hatches for "trace nothing"
and "trace everything" without a separate code path.

`onSample` is injected the same way `Counter` was injected in Exercise 13:
it is whatever the caller wants done with a sampled call — start a real
trace span, log a verbose line, copy the request to a debug queue — and
the decorator itself never imports a tracing package.

Create `sampler.go`:

```go
package sampler

// Source draws a value uniformly distributed in [0, 1). Production wires
// this to a real generator (rand.Float64); tests wire it to a scripted
// sequence so the sample/skip decision is exact and repeatable.
type Source func() float64

// New returns a closure that calls onSample a fraction of the time equal
// to rate: each invocation draws one value from src and fires onSample
// only if that value is strictly less than rate. Deciding via an injected
// Source instead of calling math/rand directly is what lets a test assert
// an exact sequence of sample/skip decisions instead of a statistical
// approximation.
func New(rate float64, src Source, onSample func()) func() {
	return func() {
		if src() < rate {
			onSample()
		}
	}
}
```

### Tests

`TestSamplerFiresBelowRate` is a table over the boundary cases: a draw
comfortably below the rate samples, comfortably above skips, and a draw
exactly equal to the rate skips because the comparison is strict; `rate ==
0` and `rate == 1` are the all-or-nothing edges. `TestSamplerExactSequence`
scripts a fixed sequence of draws and asserts the exact fire count — the
assertion that would be impossible without an injected source.
`TestSamplerDrawsExactlyOncePerCall` guards against a subtle bug: calling
`src()` more than once per invocation (say, to retry on some condition)
would silently change the effective sampling rate.

Create `sampler_test.go`:

```go
package sampler

import "testing"

// scripted returns a Source that plays back values in order, panicking if
// called more times than there are scripted values.
func scripted(values ...float64) Source {
	i := 0
	return func() float64 {
		v := values[i]
		i++
		return v
	}
}

func TestSamplerFiresBelowRate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rate float64
		draw float64
		want bool
	}{
		{"well below rate samples", 0.5, 0.1, true},
		{"well above rate skips", 0.5, 0.9, false},
		{"exactly at rate skips (strict less-than)", 0.5, 0.5, false},
		{"just below rate samples", 0.5, 0.499999, true},
		{"rate zero never samples", 0, 0.0, false},
		{"rate one always samples for any draw below 1", 1, 0.999999, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fired := false
			trace := New(tc.rate, scripted(tc.draw), func() { fired = true })
			trace()

			if fired != tc.want {
				t.Fatalf("fired = %v, want %v (rate=%v draw=%v)", fired, tc.want, tc.rate, tc.draw)
			}
		})
	}
}

func TestSamplerExactSequence(t *testing.T) {
	t.Parallel()

	draws := []float64{0.05, 0.5, 0.09, 0.11, 0.099999}
	src := scripted(draws...)

	var fires int
	trace := New(0.1, src, func() { fires++ })
	for range draws {
		trace()
	}

	// draws below 0.1: 0.05, 0.09, and 0.099999 -> 3 fires (0.5 and 0.11 do not)
	if fires != 3 {
		t.Fatalf("fires = %d, want 3", fires)
	}
}

func TestSamplerDrawsExactlyOncePerCall(t *testing.T) {
	t.Parallel()

	calls := 0
	src := func() float64 {
		calls++
		return 1 // never samples, but proves the source is consulted once
	}
	trace := New(0.5, src, func() {})

	for range 5 {
		trace()
	}

	if calls != 5 {
		t.Fatalf("source called %d times, want 5 (once per trace() call)", calls)
	}
}
```

## Review

The sampler is correct when it draws exactly one value per call and
compares it with strict less-than against `rate` — that single comparison
is what makes `0` and `1` behave as clean all-or-nothing edges rather than
needing special-cased branches. Injecting `Source` is the load-bearing
decision: it turns "assert the sampler fires roughly 10% of the time" into
"assert the sampler fires on this exact scripted sequence," which is a
much stronger and faster test than any statistical check over thousands of
calls could be. The same decorator wraps a trace-start callback, a debug
logger, or a queue publisher without ever importing an observability
library itself.

## Resources

- [math/rand/v2 package](https://pkg.go.dev/math/rand/v2) — `Rand`, `New`, `NewPCG`, `Float64`.
- [OpenTelemetry: Sampling](https://opentelemetry.io/docs/concepts/sampling/) — how probabilistic sampling is used in real distributed tracing systems.
- [Google Dapper paper (Sigelman et al., 2010)](https://research.google/pubs/pub36356/) — the origin of trace sampling at scale in production tracing systems.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-batch-collector-flush-callback.md](17-batch-collector-flush-callback.md) | Next: [19-broadcast-tee-multiple-subscribers.md](19-broadcast-tee-multiple-subscribers.md)
