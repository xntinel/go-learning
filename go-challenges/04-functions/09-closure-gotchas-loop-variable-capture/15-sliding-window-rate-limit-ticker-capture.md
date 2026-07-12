# Exercise 15: Sliding-Window Rate Limiter: Ticker Callback Capturing Loop Index

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

A sliding-window rate limiter keeps one `Window` per time slice and resets
each window's counter when its own ticker fires. Building those reset
callbacks in a loop is the classic trap: if the callback closes over a single
shared index variable instead of its own, every window's ticker ends up
resetting the SAME (last) window, because a ticker callback always fires
later than the loop that registered it.

## What you'll build

```text
windowlimiter/               independent module: example.com/windowlimiter
  go.mod                     go 1.24
  windowlimiter.go             Window, ResetFunc, BuildResetCallbacks, BuildResetCallbacksBuggy
  cmd/
    demo/
      main.go                runnable demo: fire every window's callback, print counts
  windowlimiter_test.go        table test: own index vs. shared index, single-window edge case
```

- Files: `windowlimiter.go`, `cmd/demo/main.go`, `windowlimiter_test.go`.
- Implement: `BuildResetCallbacks(windows)` closing over each window's own index; `BuildResetCallbacksBuggy` declaring a single `idx` variable outside the loop and closing over that instead.
- Test: a table test firing every callback for both variants and asserting the resulting per-window counts; an edge case with exactly one window, where the bug has nowhere else to leak into.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the callback needs its own index, not a shared one

A ticker callback is registered once but fires many times, always later than
the setup code that built it. `BuildResetCallbacksBuggy` declares `idx`
*outside* the loop and reuses `for idx = range windows` instead of a fresh
per-iteration variable — this is exactly the failure mode that made Go 1.22's
per-iteration loop variables necessary in the first place, recreated on
purpose here since `idx` is never the implicit range variable at all, just an
ordinary shared `int`. By the time any callback fires, `idx` holds whatever
the loop last assigned it: the index of the last window. Every callback,
regardless of which window it was "registered for," resets that same last
window.

`BuildResetCallbacks` fixes it by closing over `i` from `for i := range
windows` directly — on a `go 1.24` module the range variable is already a
fresh binding per iteration, so callback `i` always resets `windows[i]`, no
matter how much later it fires or how many more windows the loop goes on to
register.

Create `windowlimiter.go`:

```go
package windowlimiter

import "sync"

// Window counts requests admitted during one fixed slice of a sliding-window
// rate limiter. A production limiter resets each window's counter on its own
// ticker; this package models the reset as an explicit callback so tests can
// fire it deterministically instead of waiting on a real time.Ticker.
type Window struct {
	mu    sync.Mutex
	limit int
	count int
}

// NewWindow returns a Window that admits up to limit requests before Allow
// starts returning false.
func NewWindow(limit int) *Window {
	return &Window{limit: limit}
}

// Allow admits one request if the window has capacity left.
func (w *Window) Allow() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.count >= w.limit {
		return false
	}
	w.count++
	return true
}

// Count reports the number of requests admitted since the last Reset.
func (w *Window) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

// Reset zeroes the window's counter, as if a new time slice had begun.
func (w *Window) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.count = 0
}

// ResetFunc is what a window's ticker calls when its slice elapses.
type ResetFunc func()

// BuildResetCallbacksBuggy returns one reset callback per window, but every
// callback closes over a SINGLE shared index variable declared outside the
// loop instead of that window's own index. This is the mistake that made
// pre-1.22 range loops famous, recreated here deliberately: `idx` is one
// storage location, so by the time any callback actually fires (a ticker
// callback always runs later than the loop that registered it), every
// callback reads whatever `idx` holds NOW -- the index of the LAST window.
func BuildResetCallbacksBuggy(windows []*Window) []ResetFunc {
	callbacks := make([]ResetFunc, len(windows))
	var idx int // BUG: one shared cell for every window's callback
	for idx = range windows {
		callbacks[idx] = func() {
			windows[idx].Reset() // reads idx when the ticker fires, not when registered
		}
	}
	return callbacks
}

// BuildResetCallbacks returns one reset callback per window, each closing
// over its OWN index captured at registration time, so firing callback i
// always resets windows[i] no matter how much later it fires or what the
// loop has done since.
func BuildResetCallbacks(windows []*Window) []ResetFunc {
	callbacks := make([]ResetFunc, len(windows))
	for i := range windows {
		callbacks[i] = func() {
			windows[i].Reset()
		}
	}
	return callbacks
}
```

### The runnable demo

The demo fills three windows to their limit, fires every callback once
(simulating each window's ticker), and prints the resulting counts: the
buggy variant only resets the last window, the correct variant resets all
three.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/windowlimiter"
)

func newFullWindows() []*windowlimiter.Window {
	windows := make([]*windowlimiter.Window, 3)
	for i := range windows {
		windows[i] = windowlimiter.NewWindow(2)
		windows[i].Allow()
		windows[i].Allow() // fill to the limit so a reset is visible in the count
	}
	return windows
}

func counts(windows []*windowlimiter.Window) []int {
	out := make([]int, len(windows))
	for i, w := range windows {
		out[i] = w.Count()
	}
	return out
}

func main() {
	buggyWindows := newFullWindows()
	for _, reset := range windowlimiter.BuildResetCallbacksBuggy(buggyWindows) {
		reset() // simulate every window's ticker firing once
	}
	fmt.Println("buggy   counts after every ticker fires:", counts(buggyWindows))

	correctWindows := newFullWindows()
	for _, reset := range windowlimiter.BuildResetCallbacks(correctWindows) {
		reset()
	}
	fmt.Println("correct counts after every ticker fires:", counts(correctWindows))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy   counts after every ticker fires: [2 2 0]
correct counts after every ticker fires: [0 0 0]
```

### Tests

`TestBuildResetCallbacks` is a table test that fires every callback for both
variants and asserts the resulting counts: the correct variant resets every
window to 0, the buggy variant leaves the first two windows untouched at
their filled count and only resets the last. `TestBuildResetCallbacksSingle
WindowEdgeCase` covers the boundary where the bug cannot manifest because
there is no other window to leak into.

Create `windowlimiter_test.go`:

```go
package windowlimiter

import (
	"fmt"
	"testing"
)

func fullWindows(n, limit int) []*Window {
	windows := make([]*Window, n)
	for i := range windows {
		windows[i] = NewWindow(limit)
		for j := 0; j < limit; j++ {
			windows[i].Allow()
		}
	}
	return windows
}

func countsOf(windows []*Window) []int {
	out := make([]int, len(windows))
	for i, w := range windows {
		out[i] = w.Count()
	}
	return out
}

func TestBuildResetCallbacks(t *testing.T) {
	tests := []struct {
		name  string
		build func([]*Window) []ResetFunc
		want  []int // window counts after every callback fires once
	}{
		{
			name:  "own index resets each window independently",
			build: BuildResetCallbacks,
			want:  []int{0, 0, 0},
		},
		{
			name:  "shared index resets only the last window",
			build: BuildResetCallbacksBuggy,
			want:  []int{2, 2, 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			windows := fullWindows(3, 2)
			callbacks := tt.build(windows)
			for _, reset := range callbacks {
				reset()
			}
			got := countsOf(windows)
			if len(got) != len(tt.want) {
				t.Fatalf("counts = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("counts = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestBuildResetCallbacksSingleWindowEdgeCase(t *testing.T) {
	// With exactly one window, the shared-index bug has no other window to
	// leak into, so both variants must behave identically.
	windows := fullWindows(1, 2)
	for _, reset := range BuildResetCallbacksBuggy(windows) {
		reset()
	}
	if got := windows[0].Count(); got != 0 {
		t.Fatalf("single-window count = %d, want 0", got)
	}
}

func ExampleBuildResetCallbacks() {
	windows := fullWindows(2, 1)
	for _, reset := range BuildResetCallbacks(windows) {
		reset()
	}
	fmt.Println(countsOf(windows))
	// Output: [0 0]
}
```

Verify: `go test -count=1 ./...`

## Review

The limiter's reset callbacks are correct when firing every one leaves every
window's count at exactly what it was set to, independent of how many other
windows exist or in what order their tickers fire. The mechanism to keep
straight is timing: a callback is *built* once during setup but *runs* later,
so anything it closes over must already be the value it needs by the time
setup finishes — a variable that keeps changing after that point (like a
single `idx` reused across every iteration) is captured by reference, and
every callback reads whatever it holds at call time, not at registration
time. The single-window edge case is the sanity check that the fix does not
change behavior when there is nothing to leak into.

## Resources

- [Go spec: For statements with range clause](https://go.dev/ref/spec#For_range) — per-iteration variable semantics since Go 1.22.
- [`time.Ticker`](https://pkg.go.dev/time#Ticker) — the production mechanism this exercise's `ResetFunc` stands in for.
- [Go blog: Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview) — why a shared, explicitly-declared loop variable still reproduces the old failure mode.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-router-setup-shared-config-mutation.md](14-router-setup-shared-config-mutation.md) | Next: [16-webhook-batch-async-timeout-defer-accumulation.md](16-webhook-batch-async-timeout-defer-accumulation.md)
