# Exercise 7: Large Config Struct: Value Copy vs Pointer, and When Each Escapes

"Pass a pointer to avoid copying the struct" is folklore, not a rule. A large
config struct is expensive to copy per call, but taking its address to skip the
copy can force a heap escape that costs more. This module runs a config through a
validation pipeline by value and by a *leaking* pointer, and measures which one
allocates.

This module is fully self-contained.

## What you'll build

```text
configval/                    independent module: example.com/configval
  go.mod                      go 1.26
  config.go                   large Config; ValidateVal (by value), Validator.ValidatePtr
                              (leaking param); check + ErrMaxConns/ErrTimeout/ErrRegion
  cmd/
    demo/
      main.go                 validates good/bad configs both ways; shows the alloc gap
  config_test.go              agreement, sentinels, value==0 vs pointer-leak==1 allocs
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: a large `Config`, a shared `check`, `ValidateVal(Config)` (by value),
and `Validator.ValidatePtr(*Config)` which *retains* the pointer (a leaking param),
both `//go:noinline`; sentinel errors wrapped with `%w`.
Test: both validators agree on every config; sentinels are classifiable with
`errors.Is`; and `AllocsPerRun` shows the by-value path does zero allocations while
the leaking-pointer path forces one at the call site.
Verify: `go test -count=1 -race ./...`, then observe the leak with
`go build -gcflags='-m=2' ./... 2>&1 | grep -E 'leaking param|moved to heap'`.

### The crossover, and the leak that decides it

A `Config` here is large — several scalars plus fixed-size arrays of strings and
flags — so passing it by value copies a couple hundred bytes onto the callee's
stack every call. That copy is not free, but it is *stack* work: no heap
allocation, no GC. `ValidateVal` takes the struct by value and validates a copy;
`AllocsPerRun` reports zero heap allocations for it.

Passing `*Config` avoids the copy. Whether that address escapes depends entirely on
what the callee does with it. If `ValidatePtr` only read through the pointer, the
compiler could often keep the config on the caller's stack and the pointer would
be free. But `Validator.ValidatePtr` *retains* the pointer — it stores it in
`v.last` for later diagnostics. That is a leaking param: the pointer flows out of
the function into a heap object that outlives the call, so the compiler must move
the pointed-to `Config` to the heap. Now taking `&cfg` at the call site costs one
allocation per call.

That is the real trade-off. The value path trades a stack copy for zero heap work;
the leaking-pointer path trades the copy for a heap allocation plus a
GC-scannable object. Which wins depends on the struct size and the call frequency —
there is a genuine crossover, and `unsafe.Sizeof(Config{})` (for reasoning, not
runtime use) tells you how big the copy is. Measure both with `-benchmem` for your
actual struct before assuming the pointer is cheaper.

One measurement subtlety the test encodes: to see the *per-call* escape you must
create a fresh `c := cfg` inside the measured closure and pass `&c`. If you pass
`&cfg` where `cfg` is declared once outside the loop, the leak moves `cfg` to the
heap a single time and every subsequent call reports zero — hiding the cost. Fresh
value in, fresh escape out.

`//go:noinline` pins both validators as real call boundaries so inlining does not
merge the escape analysis and mask the leak.

Create `config.go`:

```go
package configval

import (
	"errors"
	"fmt"
)

var (
	ErrMaxConns = errors.New("configval: max_conns must be positive")
	ErrTimeout  = errors.New("configval: timeout must be positive")
	ErrRegion   = errors.New("configval: region required")
)

// Config is a large settings struct: several scalars plus fixed-size arrays.
type Config struct {
	MaxConns    int
	Timeout     int
	RetryBudget int
	Region      string
	Endpoints   [8]string
	FeatureBits [16]bool
	Labels      [8]string
}

// check holds the shared validation rules and returns the first failure wrapped
// with a sentinel.
func check(c Config) error {
	if c.MaxConns <= 0 {
		return fmt.Errorf("invalid config: %w", ErrMaxConns)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("invalid config: %w", ErrTimeout)
	}
	if c.Region == "" {
		return fmt.Errorf("invalid config: %w", ErrRegion)
	}
	return nil
}

// ValidateVal validates a copy of the config. The copy stays on the stack; no
// heap allocation occurs on the valid path.
//
//go:noinline
func ValidateVal(c Config) error {
	return check(c)
}

// Validator retains the last-validated config for diagnostics, which makes its
// pointer parameter leak.
type Validator struct {
	last *Config
}

// ValidatePtr validates through a pointer AND retains it. Retaining the pointer
// is a leaking param: the pointed-to Config is forced to the heap at the caller.
//
//go:noinline
func (v *Validator) ValidatePtr(c *Config) error {
	v.last = c
	return check(*c)
}
```

### The runnable demo

The demo validates a good and a bad config both ways, showing the results are
identical, then prints the allocation counts for the value path (zero) and the
leaking-pointer path (one).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"testing"

	"example.com/configval"
)

func main() {
	good := configval.Config{MaxConns: 100, Timeout: 30, Region: "us-east-1"}
	bad := configval.Config{MaxConns: 100, Timeout: 30} // missing region
	v := &configval.Validator{}

	fmt.Printf("val good: %v\n", configval.ValidateVal(good))
	fmt.Printf("ptr good: %v\n", v.ValidatePtr(&good))
	fmt.Printf("val bad:  %v\n", configval.ValidateVal(bad))
	fmt.Printf("ptr bad:  %v\n", v.ValidatePtr(&bad))

	var sinkErr error
	valA := testing.AllocsPerRun(1000, func() { sinkErr = configval.ValidateVal(good) })
	ptrA := testing.AllocsPerRun(1000, func() {
		c := good
		sinkErr = v.ValidatePtr(&c)
	})
	_ = sinkErr
	fmt.Printf("by-value allocs/op: %.0f\n", valA)
	fmt.Printf("by-pointer (leaking) allocs/op: %.0f\n", ptrA)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
val good: <nil>
ptr good: <nil>
val bad:  invalid config: configval: region required
ptr bad:  invalid config: configval: region required
by-value allocs/op: 0
by-pointer (leaking) allocs/op: 1
```

### Tests

`TestValidatorsAgree` proves the two pass modes are behaviorally identical across a
table of configs — the optimization decision must never change the answer.
`TestSentinels` asserts each failure is classifiable with `errors.Is`.
`TestValueZeroPointerLeaks` pins the allocation contract: zero for the value path,
one for the leaking-pointer path.

Create `config_test.go`:

```go
package configval

import (
	"errors"
	"testing"
)

func TestValidatorsAgree(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
		want error
	}{
		{"valid", Config{MaxConns: 10, Timeout: 5, Region: "eu"}, nil},
		{"no-conns", Config{Timeout: 5, Region: "eu"}, ErrMaxConns},
		{"no-timeout", Config{MaxConns: 10, Region: "eu"}, ErrTimeout},
		{"no-region", Config{MaxConns: 10, Timeout: 5}, ErrRegion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &Validator{}
			c := tc.cfg
			byVal := ValidateVal(tc.cfg)
			byPtr := v.ValidatePtr(&c)
			// errors.Is(nil, nil) is true, so this covers the valid case too.
			if !errors.Is(byVal, tc.want) {
				t.Fatalf("ValidateVal = %v, want %v", byVal, tc.want)
			}
			if !errors.Is(byPtr, tc.want) {
				t.Fatalf("ValidatePtr = %v, want %v", byPtr, tc.want)
			}
		})
	}
}

func TestSentinels(t *testing.T) {
	t.Parallel()
	if err := ValidateVal(Config{Timeout: 1, Region: "x"}); !errors.Is(err, ErrMaxConns) {
		t.Errorf("err = %v, want ErrMaxConns", err)
	}
	if err := ValidateVal(Config{MaxConns: 1, Region: "x"}); !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
	if err := ValidateVal(Config{MaxConns: 1, Timeout: 1}); !errors.Is(err, ErrRegion) {
		t.Errorf("err = %v, want ErrRegion", err)
	}
}

var sinkErr error

func TestValueZeroPointerLeaks(t *testing.T) {
	good := Config{MaxConns: 100, Timeout: 30, Region: "us-east-1"}
	v := &Validator{}
	valA := testing.AllocsPerRun(1000, func() { sinkErr = ValidateVal(good) })
	ptrA := testing.AllocsPerRun(1000, func() {
		c := good
		sinkErr = v.ValidatePtr(&c)
	})
	if valA != 0 {
		t.Errorf("by-value allocs/op = %.1f, want 0", valA)
	}
	if ptrA < 1 {
		t.Errorf("leaking-pointer allocs/op = %.1f, want >= 1", ptrA)
	}
}

func BenchmarkValidateVal(b *testing.B) {
	good := Config{MaxConns: 100, Timeout: 30, Region: "us-east-1"}
	b.ReportAllocs()
	for b.Loop() {
		sinkErr = ValidateVal(good)
	}
}

func BenchmarkValidatePtr(b *testing.B) {
	good := Config{MaxConns: 100, Timeout: 30, Region: "us-east-1"}
	v := &Validator{}
	b.ReportAllocs()
	for b.Loop() {
		c := good
		sinkErr = v.ValidatePtr(&c)
	}
}
```

## Review

The two validators are correct only if they agree on every config;
`TestValidatorsAgree` is what lets the pass-by-value/pass-by-pointer choice be a
pure performance decision. The allocation lesson is that pointer-passing is not
automatically cheaper: because `ValidatePtr` retains the pointer, taking `&cfg`
leaks and moves the struct to the heap — one allocation per call — while the value
path does the copy on the stack for zero heap work. Confirm the leak with
`go build -gcflags='-m=2'` and look for `leaking param: c` and `moved to heap`.
The mistake to avoid is a blanket "always pass big structs by pointer": if the
callee retains or otherwise leaks the pointer, you have swapped a stack copy for a
heap allocation. Measure the crossover for your struct size and call rate.

## Resources

- [Go Blog: Escape analysis](https://go.dev/blog/escape-analysis) — leaking params and pointer escapes.
- [Go GC Guide](https://go.dev/doc/gc-guide) — why heap objects cost beyond the allocation.
- [unsafe.Sizeof](https://pkg.go.dev/unsafe#Sizeof) — reasoning about copy cost.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-closure-capture-escape.md](06-closure-capture-escape.md) | Next: [08-builder-vs-buffer-io-writer.md](08-builder-vs-buffer-io-writer.md)
