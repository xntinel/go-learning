# Exercise 2: Pin A Public API's Return Types With Compile-Time Assertions

An inferred return type is a promise the compiler makes silently. This module turns
that promise into a build-time contract: a `_test.go` whose whole job is to fail to
compile if a public function's return type ever drifts. You will apply it to a
config loader's exported surface, so that widening `Workers` from `int` to `int64`
breaks `go build` instead of a downstream caller.

## What you'll build

```text
runtimecfg/                 independent module: example.com/runtimecfg
  go.mod                    go 1.26
  config.go                 Load, Config, and InferredDefaults (the guarded API)
  cmd/
    demo/
      main.go               prints the inferred default types via %T
  types_contract_test.go    compile-time var _ T = expr assertions + one runtime check
```

Files: `config.go`, `cmd/demo/main.go`, `types_contract_test.go`.
Implement: `InferredDefaults() (int, float64, time.Duration)` plus a small `Load`
whose result type is also pinned.
Test: a dedicated contract file of `var _ T = fn()` assertions and a trivial
runtime check so `go test` counts the file.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/runtimecfg/cmd/demo
cd ~/go-exercises/runtimecfg
go mod init example.com/runtimecfg
go mod edit -go=1.26
```

## Why a runtime test cannot catch a widened return type

Consider `InferredDefaults`. Inside it, `workers := defaultWorkers` infers `int`
(the constant's default type), `sampleRate := defaultSampleRate` infers `float64`,
and `timeout := defaultTimeout` is `time.Duration`. Those are the types the
function *returns*. Now imagine a future refactor changes the signature's first
result to `int64` "to match a column". Every existing caller that did
`var n int = workers()` would stop compiling — but a caller that used `:=` would
keep compiling with a now-wider type, and a runtime test asserting `workers == 4`
would still pass, because `int64(4) == 4`. The drift is invisible to values.

The guard is a line whose *only* effect is compilation: `var _ int = fn()`. It
succeeds only while `fn()`'s first result is assignable to `int`. Widen the return
to `int64` and the assignment `var _ int = int64(...)` is a compile error, so
`go build ./...` fails before any test runs. That is the contract: the type is now
part of the build. We also pin the composite result of `Load` field-by-field, and
pin a *zero-derived* value with `var _ T = zeroFrom(fn())` to show the technique
generalizes to values you extract rather than return directly.

The file still contains one trivial runtime assertion. That is deliberate: without
at least one test function, `go test` reports the package has no tests and some CI
setups treat that as a soft failure; the runtime check gives the file a reason to
run while the real value lives in the `var _` lines that gate compilation.

Create `config.go`:

```go
package runtimecfg

import "time"

const (
	defaultTimeout    = 2 * time.Second
	defaultSampleRate = 0.10
	defaultWorkers    = 4
)

// Config is the loader's public result type. The contract test pins each field
// type so a later widening breaks the build.
type Config struct {
	Timeout    time.Duration
	SampleRate float32
	Workers    int
}

// Load returns a Config built from the untyped-constant defaults. The struct
// fields drive the constant types.
func Load() Config {
	return Config{
		Timeout:    defaultTimeout,
		SampleRate: defaultSampleRate,
		Workers:    defaultWorkers,
	}
}

// InferredDefaults returns the defaults through := inference, so each result
// carries the constant's DEFAULT type: int, float64, time.Duration. These are
// the exact types the contract test pins.
func InferredDefaults() (int, float64, time.Duration) {
	workers := defaultWorkers
	sampleRate := defaultSampleRate
	timeout := defaultTimeout
	return workers, sampleRate, timeout
}
```

## Demo

The demo prints the concrete type of each inferred default with `%T`, which is the
runtime shadow of what the contract test enforces at compile time.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/runtimecfg"
)

func main() {
	workers, sampleRate, timeout := runtimecfg.InferredDefaults()
	fmt.Printf("workers=%d (%T)\n", workers, workers)
	fmt.Printf("sampleRate=%v (%T)\n", sampleRate, sampleRate)
	fmt.Printf("timeout=%s (%T)\n", timeout, timeout)

	cfg := runtimecfg.Load()
	fmt.Printf("config.Workers=%d (%T)\n", cfg.Workers, cfg.Workers)
	fmt.Printf("config.SampleRate=%v (%T)\n", cfg.SampleRate, cfg.SampleRate)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workers=4 (int)
sampleRate=0.1 (float64)
timeout=2s (time.Duration)
config.Workers=4 (int)
config.SampleRate=0.1 (float32)
```

Each `%T` prints the concrete inferred type, which is the runtime shadow of what
the `var _ T` contract lines pin at compile time.

## Tests

The contract file is mostly declarations. Each `var _ T = ...` is a compile-time
assertion; `TestContractRuntime` exists only so `go test` has something to run.

Create `types_contract_test.go`:

```go
package runtimecfg

import (
	"testing"
	"time"
)

// Compile-time contracts on InferredDefaults' return types. If any result type
// drifts (e.g. int -> int64), these declarations stop compiling and go build
// fails before any test runs.
func inferredWorkers() int { w, _, _ := InferredDefaults(); return w }

func inferredSampleRate() float64 { _, s, _ := InferredDefaults(); return s }

func inferredTimeout() time.Duration { _, _, d := InferredDefaults(); return d }

var (
	_ int           = inferredWorkers()
	_ float64       = inferredSampleRate()
	_ time.Duration = inferredTimeout()
)

// Contracts on the Load result: each Config field type is pinned.
var (
	_ time.Duration = Load().Timeout
	_ float32       = Load().SampleRate
	_ int           = Load().Workers
)

// zeroFrom returns the zero value of whatever type it is given, preserving that
// type. It lets us pin a return type without depending on the runtime value.
func zeroFrom[T any](T) T {
	var z T
	return z
}

var _ int = zeroFrom(inferredWorkers())

func TestContractRuntime(t *testing.T) {
	t.Parallel()

	workers, sampleRate, timeout := InferredDefaults()
	if workers != 4 {
		t.Fatalf("workers = %d, want 4", workers)
	}
	if sampleRate != 0.10 {
		t.Fatalf("sampleRate = %v, want 0.10", sampleRate)
	}
	if timeout != 2*time.Second {
		t.Fatalf("timeout = %s, want 2s", timeout)
	}
}
```

## Review

The value of this module is what happens when you *break* it: change
`InferredDefaults` to return `int64` first, and `var _ int = inferredWorkers()`
fails to compile with "cannot use ... (variable of type int64) as int value" — the
build stops, the drift is caught at the earliest possible moment, and no customer
ever sees a widened id. The runtime test is a formality; the `var _ T` lines are
the contract. Keep them in a dedicated file so their intent is obvious, name the
helpers after the exact result they extract, and add one line per boundary you are
unwilling to let drift silently.

## Resources

- [Go Specification: Assignability](https://go.dev/ref/spec#Assignability) — the rule that makes `var _ T = expr` a type check.
- [Go Specification: Blank identifier](https://go.dev/ref/spec#Blank_identifier) — why `_` discards the value but keeps the check.
- [Go Specification: Type inference](https://go.dev/ref/spec#Type_inference) — what `:=` infers for each result.

---

Back to [01-runtimecfg-config-loader.md](01-runtimecfg-config-loader.md) | Next: [03-port-and-id-parsing-uint.md](03-port-and-id-parsing-uint.md)
