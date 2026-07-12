# Exercise 6: Telemetry Sampler Rate From Constant Division

A tracing sampler keeps a fraction of traces — say 10% — by hashing the trace ID
and sampling when the hash falls under the rate. The rate is a constant, and this
is exactly where the integer-division trap bites: `1 / 10` is `0`, not `0.1`. This
module derives sample rates from constant division done in the floating kind and
implements a deterministic, hash-based `Decide`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
tracesampler/                 independent module: example.com/tracesampler
  go.mod                      go 1.26
  sampler.go                  float-kind rate constants; Percent, Sampler.Decide (fnv hash)
  cmd/
    demo/
      main.go                 samples a batch of trace IDs, prints the kept fraction
  sampler_test.go             1/10 vs 1.0/10 pitfall; distribution within tolerance; float32 fit
```

Files: `sampler.go`, `cmd/demo/main.go`, `sampler_test.go`.
Implement: `defaultRate = 1.0 / 10` and `Percent(n)` helpers as floating-kind
constants; a `Sampler` with `Decide(traceID string) bool` using an FNV hash mapped
to `[0, 1)`; `DefaultRate32()` returning the rate as `float32`.
Test: the documented `1/10 == 0` vs `1.0/10 == 0.1` pitfall; `Decide` over N
deterministic inputs lands within tolerance of the rate; the rate fits `float32`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/08-untyped-constants-and-constant-expressions/06-sampler-rate-float-precision/cmd/demo
cd go-solutions/02-variables-types-and-constants/08-untyped-constants-and-constant-expressions/06-sampler-rate-float-precision
```

### The kind of the operands decides the division

`const defaultRate = 1.0 / 10` is `0.1`: the `1.0` is a floating-kind constant, so
the whole expression is computed in the floating kind. `const bug = 1 / 10` is `0`:
both operands are integer-kind, so it is integer division, and the `0` survives
even if you assign it to a `float64`. The target type does not rescue you — the
operation already happened using the operand kinds. `Percent(n int)` builds a rate
as `float64(n) / 100.0`, forcing floating division so `Percent(10)` is `0.1`, not
`0`. This is the single most common constant bug in a sampler config, and the test
pins both sides of it.

`Decide` must be deterministic: the same trace ID always yields the same
keep/drop decision, so a trace is either sampled everywhere or nowhere. It hashes
the trace ID with FNV-1a (`hash/fnv`), takes the top 53 bits of the 64-bit hash,
and divides by `2^53` to get a fraction uniformly in `[0, 1)` — the same technique
`math/rand` uses to make a `float64`. The trace is sampled when that fraction is
below the rate. Over many distinct trace IDs the kept fraction converges to the
rate, which the distribution test checks within a tolerance.

Create `sampler.go`:

```go
package tracesampler

import "hash/fnv"

// Floating-kind constant division: 1.0/10 is 0.1, NOT 0. The bug variant 1/10
// (integer division, == 0) is asserted against in the test.
const defaultRate = 1.0 / 10

// Percent returns n percent as a ratio in [0,1], using floating division so
// Percent(10) is 0.10, not 0.
func Percent(n int) float64 {
	return float64(n) / 100.0
}

// Sampler decides deterministically whether to keep a trace.
type Sampler struct {
	rate float64
}

// New returns a Sampler at the given keep rate (clamped to [0,1]).
func New(rate float64) *Sampler {
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	return &Sampler{rate: rate}
}

// Default returns a Sampler at the default 10% rate.
func Default() *Sampler {
	return New(defaultRate)
}

// Rate returns the sampler's keep rate.
func (s *Sampler) Rate() float64 {
	return s.rate
}

// frac maps a trace ID to a fraction uniformly in [0,1) via FNV-1a. Same input,
// same fraction, so a trace is sampled consistently.
func frac(traceID string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(traceID))
	return float64(h.Sum64()>>11) / float64(uint64(1)<<53)
}

// Decide reports whether the trace should be kept.
func (s *Sampler) Decide(traceID string) bool {
	return frac(traceID) < s.rate
}

// DefaultRate32 returns the default rate as a float32. The floating constant
// fits float32 without a precision surprise at the boundary.
func DefaultRate32() float32 {
	return defaultRate
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tracesampler"
)

func main() {
	s := tracesampler.Default()
	fmt.Printf("rate=%.2f\n", s.Rate())

	const n = 20000
	kept := 0
	for i := range n {
		id := fmt.Sprintf("trace-%05d", i)
		if s.Decide(id) {
			kept++
		}
	}
	fmt.Printf("kept %d of %d (%.3f)\n", kept, n, float64(kept)/n)
	fmt.Printf("Percent(25)=%.2f bug 1/10=%d ok 1.0/10=%.1f\n", tracesampler.Percent(25), 1/10, 1.0/10)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
rate=0.10
kept 1890 of 20000 (0.095)
Percent(25)=0.25 bug 1/10=0 ok 1.0/10=0.1
```

### Tests

`TestIntegerDivisionPitfall` is the named-constant proof of the trap: `1 / 10` is
`0` and `1.0 / 10` is `0.1`. `TestDistribution` runs `Decide` over 20000
deterministic trace IDs and asserts the kept fraction is within 0.02 of the rate —
deterministic, so it either always passes or always fails, never flakes.
`TestConsistentDecision` proves the same trace ID always decides the same way.
`TestRateFitsFloat32` checks the rate survives the `float32` boundary.

Create `sampler_test.go`:

```go
package tracesampler

import (
	"fmt"
	"math"
	"testing"
)

func TestIntegerDivisionPitfall(t *testing.T) {
	t.Parallel()

	const bug = 1 / 10    // integer division: 0
	const good = 1.0 / 10 // floating division: 0.1

	if bug != 0 {
		t.Fatalf("1/10 = %d, expected the integer-division trap value 0", bug)
	}
	if good != 0.1 {
		t.Fatalf("1.0/10 = %v, want 0.1", good)
	}
}

func TestDistribution(t *testing.T) {
	t.Parallel()

	s := Default()
	const n = 20000
	kept := 0
	for i := range n {
		if s.Decide(fmt.Sprintf("trace-%05d", i)) {
			kept++
		}
	}
	frac := float64(kept) / n
	if math.Abs(frac-s.Rate()) > 0.02 {
		t.Fatalf("kept fraction %.4f differs from rate %.4f by more than 0.02", frac, s.Rate())
	}
}

func TestConsistentDecision(t *testing.T) {
	t.Parallel()

	s := New(Percent(50))
	const id = "trace-consistency-check"
	first := s.Decide(id)
	for range 100 {
		if s.Decide(id) != first {
			t.Fatalf("Decide(%q) not deterministic", id)
		}
	}
}

func TestRateFitsFloat32(t *testing.T) {
	t.Parallel()

	var r32 float32 = DefaultRate32()
	if r32 <= 0 || r32 >= 1 {
		t.Fatalf("DefaultRate32() = %v, want in (0,1)", r32)
	}
	if math.Abs(float64(r32)-0.1) > 1e-6 {
		t.Fatalf("DefaultRate32() = %v, want approx 0.1", r32)
	}
}
```

## Review

The sampler is correct when `defaultRate` is computed in the floating kind (so it
is `0.1`, not `0`), when `Decide` is a pure function of the trace ID's hash (so it
is deterministic and consistent), and when the kept fraction over many inputs
converges to the rate. The trap to internalize is that `1 / 10` and `1.0 / 10`
differ because of the operand kinds, not the variable you assign to — assigning the
integer-division result to a `float64` still gives `0.0`. The distribution test is
deterministic by construction; if it fails it is a real regression, not flake.

## Resources

- [The Go Blog: Constants](https://go.dev/blog/constants) — floating vs integer constant kinds and division.
- [hash/fnv](https://pkg.go.dev/hash/fnv) — the FNV-1a hash used for the deterministic decision.
- [Go Language Specification: Constant expressions](https://go.dev/ref/spec#Constant_expressions) — operand-kind rules for `/`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-overflow-guardrails-math-consts.md](05-overflow-guardrails-math-consts.md) | Next: [07-rate-limiter-capacity-config.md](07-rate-limiter-capacity-config.md)
