# Exercise 8: Contain panics in handler-spawned goroutines

The `Recoverer` middleware protects the request goroutine — and nothing else. A
panic in a goroutine the handler launches is on a *different* goroutine, escapes
recovery, and crashes the whole process. This exercise builds a `safeGo` helper
that recovers inside the goroutine and reports the panic, and a handler that fires
background work correctly.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
safego/                      independent module: example.com/safego
  go.mod                     go 1.26
  safego.go                  SafeGo(recover + report); handler that spawns background work
  cmd/
    demo/
      main.go                runnable demo: background panic captured, request still 200
  safego_test.go             recovered goroutine panic captured; request returns valid; -race
```

Files: `safego.go`, `cmd/demo/main.go`, `safego_test.go`.
Implement: `SafeGo(wg, onPanic, work)` that runs `work` in a goroutine, recovers a
panic inside it, and reports the value; a handler that responds 202 and does
background work through `SafeGo`.
Test: prove the recovered goroutine panic is captured (the report channel/logger
receives the value) and the request still returns a valid response; document that
the naive `go work()` would escape `Recoverer`. Run with `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/safego/cmd/demo
cd ~/go-exercises/safego
go mod init example.com/safego
```

### Why Recoverer cannot help here

`recover` is goroutine-local. A deferred `recover` catches a panic only if that
panic is unwinding *the same goroutine* the `defer` is registered on. The
`Recoverer` middleware defers its recover on the request goroutine — the one
running `ServeHTTP`. When a handler does:

```go
func handler(w http.ResponseWriter, r *http.Request) {
	go sendWelcomeEmail(user) // fire and forget
	w.WriteHeader(http.StatusAccepted)
}
```

`sendWelcomeEmail` runs on a *new* goroutine. If it panics — a nil pointer, a map
write, an assertion — that panic unwinds the new goroutine, finds no deferred
recover on it, propagates to the top of that goroutine, and the Go runtime crashes
the *entire process*. Every in-flight request on that instance dies. The request
that spawned the goroutine may already have returned 202; the crash arrives
milliseconds later, unattributable. This is one of the nastiest production failure
modes in Go services precisely because it looks fine in every test that does not
exercise the panic path, and `Recoverer` gives a false sense of safety.

The fix is a helper that puts the recover *on the goroutine that does the work*.
`SafeGo` takes the work function, runs it inside a goroutine whose deferred
function recovers, and on a non-nil recovered value reports it — through a
callback, an error channel, or a logger — instead of letting it reach the runtime.
Optionally it participates in a `sync.WaitGroup` so a graceful shutdown can wait
for background work to drain. The recovered value is logged with `debug.Stack()`
captured *inside* the goroutine, so the stack points at the real fault.

The reporting seam matters. In production `SafeGo` logs the panic (and increments a
metric); in a test you inject a callback that records the value so the test can
assert the panic was contained rather than crashing the test binary. The same
helper serves both by taking an `onPanic func(any)`.

Create `safego.go`:

```go
package safego

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
)

// SafeGo runs work on a new goroutine with a recover, so a panic in work is
// contained and reported via onPanic instead of crashing the process. If wg is
// non-nil the goroutine is tracked for graceful shutdown. onPanic may be nil, in
// which case the panic is logged.
func SafeGo(wg *sync.WaitGroup, onPanic func(v any), work func()) {
	if wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		defer func() {
			if v := recover(); v != nil {
				stack := debug.Stack()
				if onPanic != nil {
					onPanic(v)
				} else {
					slog.Error("recovered background panic", "panic", v, "stack", string(stack))
				}
			}
		}()
		work()
	}()
}

// Notifier is a realistic dependency the handler fires in the background.
type Notifier struct {
	wg      sync.WaitGroup
	onPanic func(v any)
}

func NewNotifier(onPanic func(v any)) *Notifier {
	return &Notifier{onPanic: onPanic}
}

// Wait blocks until all background work has drained (for graceful shutdown).
func (n *Notifier) Wait() { n.wg.Wait() }

// Handler accepts a request, kicks off background work through SafeGo, and
// returns 202 immediately. A panic in the background work is contained.
func (n *Notifier) Handler(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user")
	SafeGo(&n.wg, n.onPanic, func() {
		if user == "" {
			panic("notify: empty user") // simulated background bug
		}
		// real work would send an email / enqueue a job here
	})
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("accepted"))
}
```

### The runnable demo

The demo drives one request with an empty `user`, which makes the background work
panic. It proves two things at once: the request still returns 202, and the panic
is captured (not crashing the process) — the demo waits for the background
goroutine and prints the captured value.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/safego"
)

func main() {
	var captured any
	done := make(chan struct{})
	n := safego.NewNotifier(func(v any) {
		captured = v
		close(done)
	})

	srv := httptest.NewServer(http.HandlerFunc(n.Handler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/?user=") // empty user -> background panic
	if err != nil {
		panic(err)
	}
	resp.Body.Close()

	<-done // wait for the background goroutine to report
	n.Wait()

	fmt.Printf("request status=%d\n", resp.StatusCode)
	fmt.Printf("background panic captured=%v\n", captured)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request status=202
background panic captured=notify: empty user
```

The process does not crash: the background panic was contained by `SafeGo` and
surfaced as a value, while the request completed normally.

### Tests

`TestBackgroundPanicIsContained` drives the panicking path and asserts (a) the
request returns 202 and (b) the injected `onPanic` callback received the panic
value — i.e. the process was not crashed. `TestSafeGoReportsAndDrains` calls
`SafeGo` directly with a panicking work function and a `WaitGroup`, then `Wait`s
and asserts the value was reported. The comment documents the naive `go work()`
alternative that would escape `Recoverer`; the tests do not actually crash the
binary. Run with `-race`.

Create `safego_test.go`:

```go
package safego

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestBackgroundPanicIsContained(t *testing.T) {
	t.Parallel()

	got := make(chan any, 1)
	n := NewNotifier(func(v any) { got <- v })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?user=", nil) // empty -> panic
	n.Handler(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	n.Wait() // drain background work

	select {
	case v := <-got:
		if v != "notify: empty user" {
			t.Fatalf("captured panic = %v, want %q", v, "notify: empty user")
		}
	default:
		t.Fatal("background panic was not reported (would have crashed the process)")
	}
}

func TestNoPanicPathReturnsCleanly(t *testing.T) {
	t.Parallel()

	got := make(chan any, 1)
	n := NewNotifier(func(v any) { got <- v })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?user=alice", nil)
	n.Handler(rec, req)

	n.Wait()

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	select {
	case v := <-got:
		t.Fatalf("unexpected panic report %v on the happy path", v)
	default:
	}
}

// Naive alternative, for the record (NOT run): `go work()` where work panics
// would unwind a goroutine with no recover and crash the whole test binary.
// SafeGo is what makes the panic path testable at all.

func TestSafeGoReportsAndDrains(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	got := make(chan any, 1)

	SafeGo(&wg, func(v any) { got <- v }, func() {
		panic("boom")
	})

	wg.Wait()

	select {
	case v := <-got:
		if v != "boom" {
			t.Fatalf("reported = %v, want boom", v)
		}
	default:
		t.Fatal("SafeGo did not report the panic")
	}
}

func ExampleSafeGo() {
	var wg sync.WaitGroup
	done := make(chan any, 1)
	SafeGo(&wg, func(v any) { done <- v }, func() { panic("x") })
	wg.Wait()
	fmt.Println(<-done)
	// Output: x
}
```

## Review

The contract this module pins is that a panic in background work is a *value the
program handles*, not a process-ending event. `TestBackgroundPanicIsContained`
proves both halves: the request returns 202 and the panic reaches the `onPanic`
callback — if `SafeGo` were replaced by a bare `go work()`, this test would crash
the binary instead of failing, which is exactly the production failure this exists
to prevent. The `WaitGroup` gives graceful shutdown a way to drain background work
before exit. The one rule to carry out of here: any goroutine your handler spawns
needs its *own* recover; the `Recoverer` at the boundary sees only the request
goroutine. Capture `debug.Stack()` inside the goroutine so the logged stack points
at the real fault, and run `-race` because the report channel is written from the
background goroutine and read from the request/test goroutine.

## Resources

- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — recover is per-goroutine.
- [`runtime/debug#Stack`](https://pkg.go.dev/runtime/debug#Stack) — the background goroutine's stack, captured inside it.
- [`sync#WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — draining background work for graceful shutdown.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-request-body-limit.md](09-request-body-limit.md)
