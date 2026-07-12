# Exercise 34: Graceful Shutdown: Building Cleanup Stack with Captured Context CancelFuncs

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

A shutdown sequencer starts one stage per name via `context.WithCancel` and
wants each stage's `Stop` to cancel exactly its own context. The trap:
declaring the `context.CancelFunc` variable ONCE outside the loop and having
every stage's `Stop` closure reference that SAME variable, instead of
capturing its own. Each stage's context is stored correctly (a plain value,
not a closure) — but every `Stop` ends up cancelling only the LAST stage's
context, no matter which stage it was called on, and every earlier stage's
context never gets cancelled at all.

## What you'll build

```text
shutdownctx/                 independent module: example.com/shutdownctx
  go.mod                     go 1.24
  shutdownctx.go               Stage, BuggyBuildStack, BuildStack
  cmd/
    demo/
      main.go                runnable demo: 3 stages, Stop stage 0, print which got cancelled
  shutdownctx_test.go          table test: Stop cancels own stage vs always cancels the last; edge case
```

- Files: `shutdownctx.go`, `cmd/demo/main.go`, `shutdownctx_test.go`.
- Implement: `BuggyBuildStack` declaring `cancel` once outside the loop and closing over it; `BuildStack` declaring `cancel` fresh inside the loop body with `:=`.
- Test: build a stack of stages, call `Stop` on the first one, and assert `BuildStack`'s first stage (and only it) gets cancelled while `BuggyBuildStack`'s LAST stage gets cancelled instead, regardless of which `Stop` was called.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why this is not the loop-variable bug Go 1.22 fixed

It is tempting to assume every "loop captures the wrong thing" bug got fixed
by Go 1.22's per-iteration range variables. This one did not, and cannot be,
because the shared variable here is never the loop's own iteration variable
at all: `var cancel context.CancelFunc` is declared BEFORE the `for`
statement, and the loop body does `_, cancel = context.WithCancel(parent)`,
reassigning that one pre-existing variable every iteration. Go 1.22 changed
the semantics of variables a `for` statement itself introduces; it has
nothing to say about a variable declared outside the loop and merely
assigned inside it. Every stage's `Stop` closure — `func() { cancel() }` —
references that single variable, so after the loop finishes, EVERY `Stop`
calls whatever `cancel` ended up holding: the last stage's `CancelFunc`.
Each stage's `Ctx` field is stored as a direct value at append time, not
through a closure, so `Ctx` is always correct; only `Stop`, which closes over
the shared variable, is wrong.

The fix is to declare `cancel` fresh inside the loop body with `:=` —
`ctx, cancel := context.WithCancel(parent)` — which has always created a new
variable per iteration, on every Go version, because it is declared inside
the loop's block, not merely assigned there.

Create `shutdownctx.go`:

```go
package shutdownctx

import "context"

// Stage is one shutdown stage: the context it runs under, plus a Stop
// closure meant to cancel exactly that stage's context.
type Stage struct {
	Name string
	Ctx  context.Context
	Stop func()
}

// BuggyBuildStack starts one stage per name via context.WithCancel, but the
// CancelFunc variable is declared ONCE outside the loop and every stage's
// Stop closure closes over that SAME variable instead of capturing its own.
// Each stage's OWN context is stored directly in Stage.Ctx (a plain value
// copy, not a closure, so that field is always correct) -- but by the time
// anyone calls Stop on any stage, `cancel` has been reassigned by every
// later iteration and holds only the LAST stage's cancel func. Calling
// EARLIER stages' Stop does nothing to their own context; it actually
// cancels the LAST stage instead, and the earlier stages' contexts leak.
func BuggyBuildStack(parent context.Context, names []string) []Stage {
	var stack []Stage
	var cancel context.CancelFunc // BUG: declared outside the loop, shared by every closure
	for _, name := range names {
		var ctx context.Context
		ctx, cancel = context.WithCancel(parent)
		stack = append(stack, Stage{
			Name: name,
			Ctx:  ctx,
			Stop: func() { cancel() },
		})
	}
	return stack
}

// BuildStack starts one stage per name too, but declares each stage's own
// `cancel` INSIDE the loop body with `:=`, so every closure captures its own
// distinct variable. This fix does not depend on Go 1.22's per-iteration
// range variables at all -- it works on any Go version, because the bug was
// never about the range variable; it was about a manually shared variable
// declared outside the loop.
func BuildStack(parent context.Context, names []string) []Stage {
	var stack []Stage
	for _, name := range names {
		ctx, cancel := context.WithCancel(parent) // fresh variable every iteration
		stack = append(stack, Stage{
			Name: name,
			Ctx:  ctx,
			Stop: func() { cancel() },
		})
	}
	return stack
}
```

### The runnable demo

The demo starts three stages with both variants, calls `Stop` only on the
FIRST stage ("database"), and prints which stages actually got cancelled.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/shutdownctx"
)

func main() {
	names := []string{"database", "cache", "http-server"}

	buggy := shutdownctx.BuggyBuildStack(context.Background(), names)
	buggy[0].Stop() // meant to cancel only "database"
	for _, s := range buggy {
		fmt.Printf("buggy  %-12s cancelled=%v\n", s.Name, s.Ctx.Err() != nil)
	}

	fixed := shutdownctx.BuildStack(context.Background(), names)
	fixed[0].Stop() // cancels only "database"
	for _, s := range fixed {
		fmt.Printf("fixed  %-12s cancelled=%v\n", s.Name, s.Ctx.Err() != nil)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy  database     cancelled=false
buggy  cache        cancelled=false
buggy  http-server  cancelled=true
fixed  database     cancelled=true
fixed  cache        cancelled=false
fixed  http-server  cancelled=false
```

### Tests

`TestBuildStackStopOnlyFirstStage` is a table test: calling `Stop` on the
first stage cancels exactly that stage for `BuildStack`, but cancels only
the LAST stage for `BuggyBuildStack`, regardless of which `Stop` was
actually invoked. `TestBuildStackSingleStageEdgeCase` covers the boundary
where there is no other stage to alias into.

Create `shutdownctx_test.go`:

```go
package shutdownctx

import (
	"context"
	"testing"
)

func TestBuildStackStopOnlyFirstStage(t *testing.T) {
	names := []string{"database", "cache", "http-server"}

	tests := []struct {
		name       string
		build      func(context.Context, []string) []Stage
		wantCancel []bool // wantCancel[i] = whether stage i should be cancelled
	}{
		{
			name:       "fixed: Stop cancels only the stage it belongs to",
			build:      BuildStack,
			wantCancel: []bool{true, false, false},
		},
		{
			name:       "buggy: every Stop shares the LAST stage's cancel func",
			build:      BuggyBuildStack,
			wantCancel: []bool{false, false, true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stack := tt.build(context.Background(), names)
			stack[0].Stop() // meant to cancel only "database"

			for i, s := range stack {
				cancelled := s.Ctx.Err() != nil
				if cancelled != tt.wantCancel[i] {
					t.Fatalf("%s: cancelled = %v, want %v", s.Name, cancelled, tt.wantCancel[i])
				}
			}
		})
	}
}

func TestBuildStackSingleStageEdgeCase(t *testing.T) {
	names := []string{"solo"}

	fixed := BuildStack(context.Background(), names)
	fixed[0].Stop()
	if err := fixed[0].Ctx.Err(); err == nil {
		t.Fatal("fixed solo: Ctx.Err() = nil, want cancelled")
	}

	buggy := BuggyBuildStack(context.Background(), names)
	buggy[0].Stop()
	if err := buggy[0].Ctx.Err(); err == nil {
		t.Fatal("buggy solo: Ctx.Err() = nil, want cancelled (single stage: bug can't manifest)")
	}
}
```

## Review

A shutdown stack is correct when calling `Stop` on any one stage cancels
exactly that stage's context and no other. The mechanism to keep straight is
that this bug has nothing to do with range-variable semantics at all: `cancel`
here is a variable manually declared before the `for` statement and reused
across iterations with plain `=`, which has always been one shared variable
on every Go version, unaffected by Go 1.22's loop-variable change. The
symptom is sharp and easy to miss in review: `Ctx` looks completely correct
per stage (it is a value, copied at append time), while `Stop` looks
completely correct too — both fields are set inside the same loop body — but
only `Stop` is wired to a closure over shared state. The fix is one keystroke:
`:=` instead of `=`, declared inside the loop.

## Resources

- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — the `CancelFunc` each stage's closure needs to capture independently.
- [Go spec: Declarations and scope](https://go.dev/ref/spec#Declarations_and_scope) — `:=` introduces a new variable in the innermost block; `=` assigns to an existing one.
- [Go blog: Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview) — why this specific bug falls outside what that change covers.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-fan-in-multi-channel-shared-result-write-index.md](33-fan-in-multi-channel-shared-result-write-index.md) | Next: [../10-higher-order-functions/00-concepts.md](../10-higher-order-functions/00-concepts.md)
