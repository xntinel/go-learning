# Exercise 23: Registering Shutdown Hooks at init — Store Now, Run Later

**Nivel: Intermedio** — validacion rapida (un test corto).

`init()` is sometimes the right place to register a cleanup hook — a cache
component and a database pool component each know, at load time, that they
will need to clean something up eventually. The rule this exercise enforces
is that registering must never mean running: hooks are only ever stored
during `init()`, collected onto a stack, and executed later — in LIFO order
— by an explicit `RunAll` call that a real shutdown handler would invoke.

## What you'll build

```text
shutdownstack/             independent module: example.com/shutdownstack
  go.mod                    module example.com/shutdownstack
  stack.go                   Hook, Register, Len, RunAll, RunLog
  cachehook.go                init() registers the "cache" hook (sorts before dbpoolhook.go)
  dbpoolhook.go               init() registers the "dbpool" hook
  cmd/
    demo/
      main.go                 shows hooks registered at init, then LIFO execution
  stack_test.go                registered-not-run proof + LIFO order + Register-doesn't-run
```

Files: `stack.go`, `cachehook.go`, `dbpoolhook.go`, `cmd/demo/main.go`, `stack_test.go`.
Implement: `Register(hook Hook)` that only appends to a stack; `Len() int`; `RunAll()` that drains the stack in last-registered-first-run order; two files each registering one named hook from their own `init()`.
Test: immediately after program start, both init-registered hooks are counted but neither has run; `RunAll` executes them in reverse-registration (LIFO) order; a hook passed directly to `Register` in a test is stored, not executed, until `RunAll` runs it.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/23-graceful-shutdown-handler-collection-stack/cmd/demo
cd go-solutions/04-functions/08-init-functions-and-package-initialization/23-graceful-shutdown-handler-collection-stack
go mod edit -go=1.24
```

### Why "register" must not mean "run", and why the file order matters

A cleanup hook registered from `init()` is, in effect, the component saying
"call me back when it's time to clean up" — not "clean up right now, while
the rest of the program hasn't even started". `Register` therefore does
exactly one thing: `append` the hook to a slice, under a lock. Nothing about
that operation can run the hook, which is what makes it safe to call from as
many `init()` functions as there are components, in whatever order the Go
runtime happens to process them, without any of them accidentally tearing
something down before `main` even begins.

LIFO order at actual shutdown time exists for the same reason a deferred
call unwinds last-registered-first: the component that was set up last is
usually the one that depends on something set up earlier, so it should be
torn down first. This exercise makes that ordering deterministic and testable
by having `cachehook.go` and `dbpoolhook.go` — two files in the *same*
package — each register one hook from its own `init()`. The standard `go`
tool compiles a package's files in lexical filename order, so `cachehook.go`
(alphabetically first) registers before `dbpoolhook.go`, which is why the
files are named the way they are rather than left to chance: register order
is `cache`, then `dbpool`, so `RunAll`'s LIFO order is `dbpool`, then
`cache`.

`RunLog` exists so this ordering can be observed directly by a test — and by
the demo — instead of only through printed output that a human has to read
in the right order.

Create `stack.go`:

```go
// stack.go
// Package shutdownstack lets components register cleanup hooks during
// package init — storage only, never execution — and run them all later, in
// last-registered-first-run (LIFO) order, when the application actually
// shuts down.
package shutdownstack

import "sync"

// Hook is a cleanup function to run at shutdown.
type Hook func()

var (
	mu    sync.Mutex
	stack []Hook
)

// RunLog records, in the order hooks actually executed, which hook ran. It
// exists so a test (and the demo) can observe LIFO order directly instead
// of relying on printed output.
var (
	logMu  sync.Mutex
	RunLog []string
)

// Register appends hook to the shutdown stack. It only ever stores hook —
// it never invokes it — which is what makes it safe to call from a
// package's init().
func Register(hook Hook) {
	mu.Lock()
	defer mu.Unlock()
	stack = append(stack, hook)
}

// Len reports how many hooks are currently registered and not yet run.
func Len() int {
	mu.Lock()
	defer mu.Unlock()
	return len(stack)
}

// RunAll invokes every registered hook in LIFO order — last registered,
// first run — and clears the stack afterward.
func RunAll() {
	mu.Lock()
	hooks := make([]Hook, len(stack))
	copy(hooks, stack)
	stack = nil
	mu.Unlock()

	for i := len(hooks) - 1; i >= 0; i-- {
		hooks[i]()
	}
}

func logRun(name string) {
	logMu.Lock()
	defer logMu.Unlock()
	RunLog = append(RunLog, name)
}
```

Create `cachehook.go`:

```go
// cachehook.go
package shutdownstack

// init registers the cache component's shutdown hook. It only stores the
// hook; nothing runs here. This file is named to sort before dbpoolhook.go,
// so the standard go tool — which compiles a package's files in lexical
// filename order — runs this init first. Registered first means it is the
// LAST hook RunAll invokes.
func init() {
	Register(func() { logRun("cache") })
}
```

Create `dbpoolhook.go`:

```go
// dbpoolhook.go
package shutdownstack

// init registers the database pool component's shutdown hook, after
// cachehook.go's init has already run. Registered second means it is the
// FIRST hook RunAll invokes.
func init() {
	Register(func() { logRun("dbpool") })
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/shutdownstack"
)

func main() {
	fmt.Println("hooks registered at init:", shutdownstack.Len())
	shutdownstack.RunAll()
	fmt.Println("run order (LIFO):", shutdownstack.RunLog)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hooks registered at init: 2
run order (LIFO): [dbpool cache]
```

### Tests

Create `stack_test.go`:

```go
// stack_test.go
package shutdownstack

import (
	"reflect"
	"testing"
)

// TestHooksRegisteredButNotRunYet runs first (tests execute in source
// order) and observes the package's own init-time state: cachehook.go and
// dbpoolhook.go each registered one hook from their init(), and neither has
// run — registering is not running.
func TestHooksRegisteredButNotRunYet(t *testing.T) {
	if got := Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2 (from the two components' init hooks)", got)
	}
	if len(RunLog) != 0 {
		t.Fatalf("RunLog = %v, want empty: a hook ran before RunAll was ever called", RunLog)
	}
}

// TestRunAllExecutesLIFO runs second and drains the stack, proving the two
// init-registered hooks run in last-registered-first-run order.
func TestRunAllExecutesLIFO(t *testing.T) {
	RunAll()

	want := []string{"dbpool", "cache"}
	if !reflect.DeepEqual(RunLog, want) {
		t.Fatalf("RunLog = %v, want %v", RunLog, want)
	}
	if got := Len(); got != 0 {
		t.Fatalf("Len() after RunAll = %d, want 0", got)
	}
}

// TestRegisterDoesNotRunTheHook proves the core contract in isolation, on a
// hook the test controls directly rather than one supplied by init.
func TestRegisterDoesNotRunTheHook(t *testing.T) {
	ran := false
	Register(func() { ran = true })
	if ran {
		t.Fatal("Register executed the hook instead of only storing it")
	}
	RunAll() // drain so this hook doesn't leak into a later test
	if !ran {
		t.Fatal("RunAll never invoked a hook it was holding")
	}
}
```

## Review

`TestHooksRegisteredButNotRunYet` runs before anything in the test binary has
called `RunAll`, so it is direct proof that the two components' `init()`
functions only registered their hooks — `RunLog` is still empty at that
point. `TestRunAllExecutesLIFO` then drains the stack and checks the exact
order, which only comes out right because of the filename-ordering
observation above; renaming `dbpoolhook.go` to something that sorts before
`cachehook.go` would flip the expected order, which is exactly why that
dependency is called out explicitly in a comment rather than left implicit.

The mistake to avoid is calling a hook's real cleanup logic directly inside
`init()` "just to be safe" — the whole reason this pattern exists is that
`init()` runs far too early for a database connection or a cache warmup to
safely tear anything down; all it should ever do is note that a teardown
will eventually be needed.

## Resources

- [Go spec — Package initialization](https://go.dev/ref/spec#Package_initialization) — files within a single package are presented to the compiler in filename order, which this exercise's LIFO ordering relies on.
- [os/signal: Notify](https://pkg.go.dev/os/signal#Notify) — the typical real-world trigger for calling `RunAll` on process shutdown.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-message-queue-codec-blank-import-registry.md](22-message-queue-codec-blank-import-registry.md) | Next: [24-rate-limit-token-bucket-lazy-per-key.md](24-rate-limit-token-bucket-lazy-per-key.md)
