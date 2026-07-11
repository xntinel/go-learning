# Exercise 3: Bounding guest execution: timeouts and memory caps

Untrusted code will misbehave: it will loop forever, or it will try to allocate
the world. This exercise builds a `Runner` that treats every guest invocation as
hostile work carrying a time budget and a memory budget, so a runaway guest is
interrupted at its deadline and an over-allocating guest fails its own
allocation instead of taking the host down.

This module is fully self-contained: its own `go mod init`, two embedded guests
(an infinite loop and a memory grower), its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
runner/                      independent module: example.com/runner
  go.mod                     go 1.26; requires github.com/tetratelabs/wazero
  runner.go                  type Runner; New, Instantiate, RunWithin, Close
                             sentinels ErrGuestBudget, ErrGuestTrap; embedded guests
  cmd/
    demo/
      main.go                kill a runaway; observe the memory cap
  runner_test.go             deadline-bounded runaway; memory-cap host Grow;
                             guest-side grow_by; Example
```

- Files: `runner.go`, `cmd/demo/main.go`, `runner_test.go`.
- Implement: a `Runner` built with `NewRuntimeConfig().WithCloseOnContextDone(true).WithMemoryLimitPages(n)`; `RunWithin` wraps each `Call` in `context.WithTimeout` and classifies the outcome into `ErrGuestBudget` or `ErrGuestTrap`, both wrapping the underlying error.
- Test: a runaway guest interrupted near its deadline, asserted against `ErrGuestBudget` and `context.DeadlineExceeded` with `errors.Is` and a generous time bound; a memory-cap test where host-side `Memory().Grow` past the cap fails and within it succeeds; a guest-side `grow_by` returning `-1`; a deterministic `Example` for the memory path.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/runner/cmd/demo
cd ~/go-exercises/runner
go mod init example.com/runner
go mod edit -go=1.26
go get github.com/tetratelabs/wazero@latest
```

### Two guests that misbehave on purpose

The loop guest exports `run: ()->()` whose body is `(loop (br 0))` — an
unconditional infinite loop, the canonical runaway. The grow guest exports a
1-page linear memory named `memory` and a function `grow_by: (i32)->i32` whose
body is `local.get 0; memory.grow`, so calling it asks the runtime to grow linear
memory by the argument and returns the previous page count, or `-1` if the growth
is refused. Both are hand-assembled and embedded, with their WAT in a comment, so
the whole exercise runs offline with no toolchain.

### Why a timeout alone does nothing

This is the trap that catches everyone. Passing a `context.WithTimeout` into
`Call` does **not** interrupt a spinning guest by default — wazero only checks for
cancellation if the runtime was built with
`RuntimeConfig.WithCloseOnContextDone(true)`. Without that flag, the deadline
context expires but the guest keeps running and the goroutine hangs forever. So
`New` builds the runtime with `WithCloseOnContextDone(true)` *and* `RunWithin`
passes a deadline context into `Call`. Both halves are mandatory; the runaway test
would hang instead of failing fast if either were missing.

When the deadline fires, `Call` returns an error that wraps
`context.DeadlineExceeded`. `RunWithin` inspects it: if it is a deadline or
cancellation, it returns `ErrGuestBudget`; otherwise (an out-of-bounds access, an
`unreachable`, a divide by zero) it returns `ErrGuestTrap`. Both are produced with
the two-verb wrap `fmt.Errorf("%w: %w", sentinel, err)`, so a caller can match the
class (`errors.Is(err, ErrGuestBudget)`) *and* still reach the underlying cause
(`errors.Is(err, context.DeadlineExceeded)`). Distinguishing "the guest ran out of
time" from "the guest faulted" is exactly the signal an operator needs.

### Why memory must be capped, and how growth fails

A Wasm linear memory grows in 64 KiB pages up to the format's 4 GiB maximum. An
unbounded guest can pressure the host by growing without end.
`WithMemoryLimitPages(n)` sets the ceiling: `New` here takes the cap as a
parameter. Once set, a `memory.grow` past the cap does not partially succeed — it
fails atomically. From the guest side, `grow_by` returns `-1`. From the host side,
`api.Module.Memory().Grow(delta)` returns `ok == false` and leaves the memory
unchanged. `Memory().Size()` reports the current size in bytes (one page is
65536), so a fresh 1-page instance reports 65536. The memory-cap test drives both
the host view (`Grow(100)` fails, `Grow(1)` succeeds under a 2-page cap) and the
guest view (`grow_by(100)` returns `-1`), which are two windows onto the same
enforced limit.

Create `runner.go`:

```go
// Package runner treats every guest invocation as untrusted work bounded by a
// time budget and a memory cap, so a runaway or over-allocating guest fails
// instead of taking the host down with it.
package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Sentinels classify how a guest invocation ended.
var (
	// ErrGuestBudget means the guest exceeded its time budget (deadline or
	// cancellation). It wraps context.DeadlineExceeded or context.Canceled.
	ErrGuestBudget = errors.New("runner: guest exceeded time budget")
	// ErrGuestTrap means the guest faulted (out-of-bounds, unreachable, etc.).
	ErrGuestTrap = errors.New("runner: guest trapped")
)

// loopWasm exports run: ()->() as an unconditional infinite loop.
//
//	(module
//	  (func (export "run")
//	    (loop (br 0))))
var loopWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x07, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00,
	0x0a, 0x09, 0x01, 0x07, 0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x0b,
}

// growWasm exports memory "memory" (min 1 page) and grow_by: (i32)->i32, which
// calls memory.grow by the argument and returns the previous page count (or -1).
//
//	(module
//	  (memory (export "memory") 1)
//	  (func (export "grow_by") (param i32) (result i32)
//	    local.get 0
//	    memory.grow))
var growWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x06, 0x01, 0x60, 0x01, 0x7f, 0x01, 0x7f,
	0x03, 0x02, 0x01, 0x00,
	0x05, 0x03, 0x01, 0x00, 0x01,
	0x07, 0x14, 0x02,
	0x06, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00,
	0x07, 0x67, 0x72, 0x6f, 0x77, 0x5f, 0x62, 0x79, 0x00, 0x00,
	0x0a, 0x08, 0x01, 0x06, 0x00, 0x20, 0x00, 0x40, 0x00, 0x0b,
}

// LoopWasm and GrowWasm expose the embedded guests for demos and tests.
func LoopWasm() []byte { return loopWasm }
func GrowWasm() []byte { return growWasm }

// Runner compiles one guest and runs its exports under strict bounds. The
// runtime is built with WithCloseOnContextDone so a deadline can interrupt a
// running guest, and with WithMemoryLimitPages so growth past the cap fails.
type Runner struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
}

// New builds a bounded runtime (interruptible, memory-capped) and compiles wasm.
func New(ctx context.Context, wasm []byte, maxPages uint32) (*Runner, error) {
	cfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(maxPages)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("runner: compile: %w", err)
	}
	return &Runner{rt: rt, compiled: compiled}, nil
}

// Instantiate produces a fresh isolated instance. The caller closes it.
func (r *Runner) Instantiate(ctx context.Context, name string) (api.Module, error) {
	mod, err := r.rt.InstantiateModule(ctx, r.compiled,
		wazero.NewModuleConfig().WithName(name))
	if err != nil {
		return nil, fmt.Errorf("runner: instantiate %q: %w", name, err)
	}
	return mod, nil
}

// RunWithin instantiates a throwaway instance and calls name under a per-call
// deadline of d. A guest that overruns is interrupted and reported as
// ErrGuestBudget; any other call failure is reported as ErrGuestTrap. Both wrap
// the underlying error so errors.Is can reach context.DeadlineExceeded.
func (r *Runner) RunWithin(ctx context.Context, name string, d time.Duration, args ...uint64) ([]uint64, error) {
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	mod, err := r.Instantiate(cctx, name)
	if err != nil {
		return nil, err
	}
	defer mod.Close(context.Background())

	fn := mod.ExportedFunction(name)
	if fn == nil {
		return nil, fmt.Errorf("runner: export %q not found", name)
	}

	res, err := fn.Call(cctx, args...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("%w: %w", ErrGuestBudget, err)
		}
		return nil, fmt.Errorf("%w: %w", ErrGuestTrap, err)
	}
	return res, nil
}

// Close releases the runtime and everything it created.
func (r *Runner) Close(ctx context.Context) error {
	return r.rt.Close(ctx)
}
```

Note that `RunWithin` closes the throwaway instance with a fresh
`context.Background()`, not the deadline context: after a guest is interrupted,
`cctx` is already expired, and cleanup must not itself be cancelled. The instance
is instantiated with the deadline context so that instantiation is also bounded.

### The runnable demo

The demo runs the loop guest under a 100 ms budget and confirms it was stopped
promptly, then instantiates the grow guest under a 2-page cap and drives its
memory from the host side to show the cap enforced.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"example.com/runner"
)

func main() {
	ctx := context.Background()

	// A runaway guest is stopped at its deadline instead of hanging forever.
	loop, err := runner.New(ctx, runner.LoopWasm(), 4)
	if err != nil {
		log.Fatal(err)
	}
	defer loop.Close(ctx)

	start := time.Now()
	_, err = loop.RunWithin(ctx, "run", 100*time.Millisecond)
	fmt.Println("runaway stopped by budget:", errors.Is(err, runner.ErrGuestBudget))
	fmt.Println("deadline observed:", errors.Is(err, context.DeadlineExceeded))
	fmt.Println("returned promptly:", time.Since(start) < 5*time.Second)

	// An over-allocating guest hits the memory cap instead of pressuring the host.
	grow, err := runner.New(ctx, runner.GrowWasm(), 2)
	if err != nil {
		log.Fatal(err)
	}
	defer grow.Close(ctx)

	mod, err := grow.Instantiate(ctx, "grow")
	if err != nil {
		log.Fatal(err)
	}
	defer mod.Close(ctx)

	fmt.Println("initial pages:", mod.Memory().Size()/65536)
	_, okBig := mod.Memory().Grow(100)
	prevOne, okOne := mod.Memory().Grow(1)
	fmt.Printf("grow(100) ok=%v; grow(1) ok=%v prev=%d\n", okBig, okOne, prevOne)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
runaway stopped by budget: true
deadline observed: true
returned promptly: true
initial pages: 1
grow(100) ok=false; grow(1) ok=true prev=1
```

### Tests

`TestRunawayInterrupted` is the timing-sensitive one, guarded the right way: it
gives the runaway a 100 ms budget and asserts three things — the error `Is`
`ErrGuestBudget`, the error `Is` `context.DeadlineExceeded`, and the whole call
returned in well under a generous 5-second bound. The generous bound is what keeps
it from flaking on a loaded CI machine while still proving the guest did not hang.
`TestMemoryCap` drives the host-side `Memory().Grow` past and within a 2-page cap.
`TestGrowByGuest` calls the guest's own `grow_by` through `RunWithin` and asserts
it returns `-1` when the growth is refused. `ExampleRunner_memoryCap` is
deterministic (no timing) and prints the initial byte size and the two grow
outcomes.

Create `runner_test.go`:

```go
package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tetratelabs/wazero/api"
)

func TestRunawayInterrupted(t *testing.T) {
	t.Parallel()
	r, err := New(t.Context(), LoopWasm(), 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { r.Close(context.Background()) })

	start := time.Now()
	_, err = r.RunWithin(t.Context(), "run", 100*time.Millisecond)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrGuestBudget) {
		t.Fatalf("RunWithin error = %v, want ErrGuestBudget", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunWithin error = %v, want to wrap context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("runaway took %v; deadline did not interrupt it", elapsed)
	}
}

func TestMemoryCap(t *testing.T) {
	t.Parallel()
	r, err := New(t.Context(), GrowWasm(), 2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { r.Close(context.Background()) })

	mod, err := r.Instantiate(t.Context(), "grow")
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { mod.Close(context.Background()) })

	if pages := mod.Memory().Size() / 65536; pages != 1 {
		t.Fatalf("initial pages = %d, want 1", pages)
	}
	if _, ok := mod.Memory().Grow(100); ok {
		t.Fatal("Grow(100) succeeded past the memory cap, want failure")
	}
	if _, ok := mod.Memory().Grow(1); !ok {
		t.Fatal("Grow(1) within the cap failed, want success")
	}
}

func TestGrowByGuest(t *testing.T) {
	t.Parallel()
	r, err := New(t.Context(), GrowWasm(), 2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { r.Close(context.Background()) })

	res, err := r.RunWithin(t.Context(), "grow_by", time.Second, api.EncodeI32(100))
	if err != nil {
		t.Fatalf("RunWithin: %v", err)
	}
	if got := api.DecodeI32(res[0]); got != -1 {
		t.Fatalf("grow_by(100) = %d, want -1 (cap hit)", got)
	}
}

func ExampleRunner_memoryCap() {
	ctx := context.Background()
	r, _ := New(ctx, GrowWasm(), 2)
	defer r.Close(ctx)

	mod, _ := r.Instantiate(ctx, "grow")
	defer mod.Close(ctx)

	fmt.Println(mod.Memory().Size())
	_, okBig := mod.Memory().Grow(100)
	_, okOne := mod.Memory().Grow(1)
	fmt.Println(okBig, okOne)
	// Output:
	// 65536
	// false true
}
```

## Review

The runner is correct when both bounds are actually enforced. For time, the tell
is that `TestRunawayInterrupted` returns fast: if you drop
`WithCloseOnContextDone(true)` or forget to pass the deadline context into `Call`,
the test does not fail — it hangs, because nothing interrupts the loop. That is
the failure mode to internalize: a missing cancellation setup does not produce a
wrong answer, it produces no answer. For memory, `Grow` past the cap must return
`ok == false` and leave `Size` unchanged; if it succeeds, the cap was never wired
through `WithMemoryLimitPages`.

Two details are easy to get wrong. Wrap both the class sentinel and the underlying
cause (`fmt.Errorf("%w: %w", ErrGuestBudget, err)`) so callers can match either;
wrapping only one loses information. And close the throwaway instance with a live
context, not the already-expired deadline context, or cleanup silently no-ops.
Run `go test -race` to confirm the bounded call path is clean.

## Resources

- [wazero RuntimeConfig](https://pkg.go.dev/github.com/tetratelabs/wazero#RuntimeConfig) — `WithCloseOnContextDone` and `WithMemoryLimitPages`.
- [wazero/api Memory](https://pkg.go.dev/github.com/tetratelabs/wazero/api#Memory) — `Size` and `Grow`, and how growth past the limit is reported.
- [context package](https://pkg.go.dev/context) — `WithTimeout`, `DeadlineExceeded`, and `Canceled`, matched with `errors.Is`.
- [wazero.io documentation](https://wazero.io/docs/) — runtime configuration and how context cancellation interrupts a running guest.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-runtime-lifecycle.md](02-runtime-lifecycle.md) | Next: [../02-host-guest-abi-and-memory/00-concepts.md](../02-host-guest-abi-and-memory/00-concepts.md)
