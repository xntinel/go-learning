# Exercise 3: Deterministic Config-as-Code with Starlark and Execution Budgets

When a boolean expression is not enough — the operator needs control flow,
functions, and computed structure — you reach up the capability spectrum to a
scripting language, and pay for it with explicit budgets. This exercise builds a
Bazel/Tilt-style config pipeline on Starlark: it executes user-authored script to
produce a data structure, reads a global and calls a defined function from Go, and
bounds the script with a step budget, a wall-clock deadline, disabled recursion,
and frozen shared state.

This module is fully self-contained: its own `go mod init`, types, demo, and
tests. Nothing here imports another exercise. It is a bar-mode module: it depends
on `go.starlark.net`, so the offline gate cannot fetch it and fails at build; the
value is the correct shape, the verified APIs, and tests that assert the real
budget and freeze behavior once the dependency is present.

## What you'll build

```text
config/                      independent module: example.com/config
  go.mod                     go 1.26; requires go.starlark.net
  config.go                  Runner{MaxSteps,Options,Pre}; Exec; Call; Predeclared (frozen); tier builtin
  cmd/
    demo/
      main.go                run a config script, print the produced structure and a called function
  config_test.go             exec+extract, step budget, recursion disabled, timeout, frozen, Example
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: a `Runner` holding a step budget, `syntax.FileOptions`, and a frozen predeclared `StringDict`; `Exec` running `ExecFileOptions` under `SetMaxExecutionSteps` and a context watcher that calls `Thread.Cancel`; `Call` invoking a script-defined function; a `Predeclared` builder that freezes shared state; one host builtin `tier`.
Test: exec a script and extract globals plus a called function's return; a step-budget breach; a recursion-disabled error under default `FileOptions`; a timeout via a cancelled context; a frozen-value mutation rejection.
Verify: `go test -race ./...` with the dependency present (bar-mode: the offline gate fails to build).

Set up the module:

```bash
mkdir -p ~/go-exercises/config/cmd/demo
cd ~/go-exercises/config
go mod init example.com/config
go mod edit -go=1.26
go get go.starlark.net@latest
```

### The safe defaults, made explicit

Starlark's Go implementation is deliberately conservative for untrusted input, and
this pipeline keeps it that way. `syntax.FileOptions` with `Recursion`, `While`,
and `GlobalReassign` all false is the safe posture: no recursive functions, no
`while` loops, no reassigning globals. Iteration is expressed with bounded
`for`-over-`range`, which is always allowed and cannot spin forever on an
unbounded condition. If you ever flip `Recursion` or `While` on for untrusted
input, you have reopened the infinite-loop DoS and must pair it with a step
budget — which is why the step budget is here regardless.

Three budgets bound a run. `SetMaxExecutionSteps` caps the instruction count: a
heavy loop is cancelled after N steps with a "too many steps" error. A context
watcher goroutine enforces wall-clock time: when the context is done it calls
`Thread.Cancel`, and the interpreter observes the cancel flag and aborts.
Starlark has no built-in time bound, so this goroutine *is* the timeout — there is
no option that does it for you.

### Determinism through freezing

For config-as-code, evaluation must be a pure function of inputs, or caching and
auditability break. The predeclared environment includes a shared `defaults` dict.
Calling `Freeze` on the predeclared `StringDict` makes every value in it deeply
immutable, so a script that tries `defaults["region"] = "evil"` fails with a
frozen-mutation error instead of leaking a mutation into the next evaluation.
Combined with exposing no clock, no randomness, and no IO — only the pure `tier`
host builtin — the sandbox is deterministic.

Create `config.go`:

```go
package config

import (
	"context"
	"errors"
	"fmt"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// defaultMaxSteps bounds instruction count for a run. A heavy loop is cancelled
// after this many steps with a "too many steps" error.
const defaultMaxSteps = 10_000_000

// ErrNotFound is returned when a requested global is absent from a script.
var ErrNotFound = errors.New("global not found")

// Runner executes untrusted config scripts under a fixed sandbox policy: a step
// budget, conservative file options, and a frozen predeclared environment.
type Runner struct {
	MaxSteps uint64
	Options  *syntax.FileOptions
	Pre      starlark.StringDict
}

// NewRunner returns a Runner with the safe defaults: no recursion, no while, no
// global reassignment, a step budget, and a frozen predeclared environment.
func NewRunner() *Runner {
	return &Runner{
		MaxSteps: defaultMaxSteps,
		Options: &syntax.FileOptions{
			Recursion:      false,
			While:          false,
			GlobalReassign: false,
		},
		Pre: Predeclared(),
	}
}

// Predeclared builds the controlled global environment scripts may read. The
// shared defaults dict is frozen so a script cannot mutate host state and leak an
// effect into a later evaluation.
func Predeclared() starlark.StringDict {
	defaults := starlark.NewDict(2)
	_ = defaults.SetKey(starlark.String("region"), starlark.String("us-east-1"))
	_ = defaults.SetKey(starlark.String("replicas"), starlark.MakeInt(2))

	pre := starlark.StringDict{
		"defaults": defaults,
		"tier":     starlark.NewBuiltin("tier", tier),
	}
	pre.Freeze()
	return pre
}

// tier is a pure host builtin mapping a replica count to a coarse capacity tier.
func tier(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var replicas int
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "replicas", &replicas); err != nil {
		return nil, err
	}
	switch {
	case replicas >= 5:
		return starlark.String("high"), nil
	case replicas >= 2:
		return starlark.String("standard"), nil
	default:
		return starlark.String("low"), nil
	}
}

// watchContext starts a goroutine that cancels the thread when ctx is done. The
// returned stop function tears the goroutine down. This goroutine is the only
// wall-clock enforcement Starlark has.
func watchContext(ctx context.Context, thread *starlark.Thread) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			thread.Cancel(ctx.Err().Error())
		case <-done:
		}
	}()
	return func() { close(done) }
}

// Exec runs src under the step budget, the conservative file options, and a
// context-derived wall-clock deadline, returning the script's global bindings.
func (r *Runner) Exec(ctx context.Context, filename, src string) (starlark.StringDict, error) {
	thread := &starlark.Thread{Name: "exec:" + filename}
	thread.SetMaxExecutionSteps(r.MaxSteps)

	stop := watchContext(ctx, thread)
	defer stop()

	globals, err := starlark.ExecFileOptions(r.Options, thread, filename, src, r.Pre)
	if err != nil {
		return nil, fmt.Errorf("exec %s: %w", filename, err)
	}
	return globals, nil
}

// Call invokes a script-defined function from globals under the same budgets.
func (r *Runner) Call(ctx context.Context, globals starlark.StringDict, name string, args ...starlark.Value) (starlark.Value, error) {
	fn, ok := globals[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	thread := &starlark.Thread{Name: "call:" + name}
	thread.SetMaxExecutionSteps(r.MaxSteps)

	stop := watchContext(ctx, thread)
	defer stop()

	return starlark.Call(thread, fn, starlark.Tuple(args), nil)
}
```

### The runnable demo

The demo runs a config script that reads the frozen `defaults`, calls the `tier`
host builtin, builds a `service` dict, and defines a `scale` function. The Go side
then reads the dict fields and calls `scale`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"go.starlark.net/starlark"

	"example.com/config"
)

const script = `
region = defaults["region"]
replicas = 4
service = {
    "region": region,
    "replicas": replicas,
    "tier": tier(replicas = replicas),
}

def scale(factor):
    return replicas * factor
`

func main() {
	r := config.NewRunner()
	ctx := context.Background()

	globals, err := r.Exec(ctx, "service.star", script)
	if err != nil {
		log.Fatalf("exec: %v", err)
	}

	svc := globals["service"].(*starlark.Dict)
	region, _, _ := svc.Get(starlark.String("region"))
	replicas, _, _ := svc.Get(starlark.String("replicas"))
	stier, _, _ := svc.Get(starlark.String("tier"))
	fmt.Printf("region=%s replicas=%s tier=%s\n",
		string(region.(starlark.String)), replicas, string(stier.(starlark.String)))

	out, err := r.Call(ctx, globals, "scale", starlark.MakeInt(3))
	if err != nil {
		log.Fatalf("call scale: %v", err)
	}
	fmt.Printf("scale(3)=%s\n", out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
region=us-east-1 replicas=4 tier=standard
scale(3)=12
```

### Tests

`TestExecAndCall` runs a script, extracts a global dict field, and calls a defined
function, asserting the Go-typed results. `TestStepBudget` sets a tiny
`MaxSteps` and asserts a heavy loop is cancelled with "too many steps".
`TestRecursionDisabled` proves the default `FileOptions` reject a self-recursive
function at call time with "called recursively". `TestTimeout` runs a large loop
under an already-cancelled context and asserts the watcher-driven cancellation
surfaces. `TestFrozenDefaults` proves a script cannot mutate the frozen shared
dict.

Create `config_test.go`:

```go
package config

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

func TestExecAndCall(t *testing.T) {
	t.Parallel()
	r := NewRunner()
	ctx := context.Background()

	src := `
replicas = 3
plan = {"tier": tier(replicas = replicas), "region": defaults["region"]}

def total(base):
    sum = 0
    for i in range(replicas):
        sum = sum + base
    return sum
`
	globals, err := r.Exec(ctx, "plan.star", src)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	plan, ok := globals["plan"].(*starlark.Dict)
	if !ok {
		t.Fatalf("plan is %T, want *starlark.Dict", globals["plan"])
	}
	tierVal, _, _ := plan.Get(starlark.String("tier"))
	if got := string(tierVal.(starlark.String)); got != "standard" {
		t.Fatalf("plan.tier = %q, want standard", got)
	}

	out, err := r.Call(ctx, globals, "total", starlark.MakeInt(10))
	if err != nil {
		t.Fatalf("Call total: %v", err)
	}
	got, _ := out.(starlark.Int).Int64()
	if got != 30 {
		t.Fatalf("total(10) = %d, want 30", got)
	}
}

func TestStepBudget(t *testing.T) {
	t.Parallel()
	r := NewRunner()
	r.MaxSteps = 1000
	_, err := r.Exec(context.Background(), "loop.star", `
def run():
    x = 0
    for i in range(1000000):
        x = x + i
    return x

result = run()
`)
	if err == nil {
		t.Fatal("heavy loop completed; want step-budget cancellation")
	}
	if !strings.Contains(err.Error(), "too many steps") {
		t.Fatalf("error = %v, want too many steps", err)
	}
}

func TestRecursionDisabled(t *testing.T) {
	t.Parallel()
	r := NewRunner()
	_, err := r.Exec(context.Background(), "rec.star", `
def fib(n):
    if n < 2:
        return n
    return fib(n - 1) + fib(n - 2)

result = fib(5)
`)
	if err == nil {
		t.Fatal("recursion succeeded; want called-recursively error with default options")
	}
	if !strings.Contains(err.Error(), "called recursively") {
		t.Fatalf("error = %v, want called recursively", err)
	}
}

func TestTimeout(t *testing.T) {
	t.Parallel()
	r := NewRunner()
	r.MaxSteps = 1 << 62 // keep the step budget out of the way; test the watcher
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done: the watcher cancels the thread promptly
	_, err := r.Exec(ctx, "spin.star", `
def spin():
    x = 0
    for i in range(100000000):
        x = x + i
    return x

result = spin()
`)
	if err == nil {
		t.Fatal("loop completed despite cancelled context; want cancellation")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("error = %v, want cancelled", err)
	}
}

func TestFrozenDefaults(t *testing.T) {
	t.Parallel()
	r := NewRunner()
	_, err := r.Exec(context.Background(), "mutate.star", `defaults["region"] = "evil"`)
	if err == nil {
		t.Fatal("mutation of frozen defaults succeeded; want frozen error")
	}
	if !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("error = %v, want frozen", err)
	}
}

func Example() {
	r := NewRunner()
	globals, err := r.Exec(context.Background(), "ex.star", `x = 2 + 3`)
	if err != nil {
		panic(err)
	}
	fmt.Println(globals["x"])
	// Output: 5
}
```

## Review

The pipeline is correct when a script is a pure, bounded function of its inputs:
the same script and predeclared environment always produce the same globals, no
run can spin forever, and no run can mutate host state. Confirm determinism with
`TestFrozenDefaults` — if a script can write `defaults`, the environment was not
frozen and evaluations are no longer isolated. Confirm the two independent bounds:
`TestStepBudget` trips the instruction cap ("too many steps"), and `TestTimeout`
trips the wall-clock watcher ("cancelled"); the step budget is not a timer and the
timer is not a step budget, so both must hold.

The mistakes to avoid: assuming Python-like recursion works — it does not under
the safe defaults, as `TestRecursionDisabled` shows, and enabling
`FileOptions.Recursion` or `While` for untrusted input without a step budget
reopens the infinite-loop DoS. Do not forget the watcher goroutine: Starlark has
no built-in timeout, so without it a cancelled context does nothing; and always
`stop()` it (the `defer` does) so it does not leak. Do not expose a mutable shared
value without freezing it. Keep host builtins pure and let them return errors via
`UnpackArgs` rather than panicking.

## Resources

- [starlark-go `starlark` package](https://pkg.go.dev/go.starlark.net/starlark) — `Thread`, `SetMaxExecutionSteps`, `Cancel`, `ExecFileOptions`, `Call`, `StringDict.Freeze`.
- [Starlark in Go — language spec](https://github.com/google/starlark-go/blob/master/doc/spec.md) — the implementation flags (recursion, while, global reassignment) and determinism guarantees.
- [starlark-go `syntax` package](https://pkg.go.dev/go.starlark.net/syntax) — `FileOptions` and its fields.
- [Starlark design intent](https://github.com/bazelbuild/starlark/blob/master/spec.md) — why the language is deterministic and hermetic by design.

---

Back to [02-policy-evaluation-with-cel.md](02-policy-evaluation-with-cel.md) | Next: [../../54-cloud-native-platform-and-orchestration/01-docker-engine-sdk/00-concepts.md](../../54-cloud-native-platform-and-orchestration/01-docker-engine-sdk/00-concepts.md)
