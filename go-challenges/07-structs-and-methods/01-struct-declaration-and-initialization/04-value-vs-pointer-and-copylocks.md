# Exercise 4: Value vs Pointer Receivers and the Copied-Mutex Trap

A struct that embeds a `sync.Mutex` has a hard rule attached: it must never be
copied, because copying the mutex copies its lock state and mutual exclusion
silently breaks. This module builds a `SessionCounter` guarded by a mutex, shows
exactly how a by-value copy destroys it (and how `go vet`'s `copylocks` analyzer
catches it), and proves the correct pointer-based design under `-race`.

Fully self-contained: own `go mod init`, inline code, own demo and tests.

## What you'll build

```text
copylocks/                  independent module: example.com/copylocks
  go.mod                    go 1.24
  counter.go                type SessionCounter (embeds sync.Mutex); pointer-receiver methods
  cmd/
    demo/
      main.go               drives *SessionCounter concurrently, prints total
  counter_test.go           concurrent -race test; method-set / interface assertion
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: a `SessionCounter` that embeds a `sync.Mutex` and a `count` field, with `Open`, `Close`, and `Active` methods, all on a **pointer receiver**.
- Test: drive `*SessionCounter` from many goroutines under `-race` and assert the exact final count; a compile-time assertion that `*SessionCounter` satisfies a `Counter` interface (and prose showing why the value type would not, and why passing by value trips `copylocks`).
- Verify: `go test -count=1 -race ./...` and `go vet ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/copylocks/cmd/demo
cd ~/go-exercises/copylocks
go mod init example.com/copylocks
go mod edit -go=1.24
```

### Why the mutex forces a pointer receiver

A `sync.Mutex` is a small struct holding lock state. When you copy a struct that
embeds one, you copy that state — and now you have two independent mutexes
guarding what was supposed to be one shared resource. Goroutine A locks the
original; goroutine B locks a *copy* it received by value; both proceed into the
critical section at once. The race is invisible in the code and shows up only as
corrupted counts under load.

Two consequences follow. First, every method must be on a **pointer receiver**
`(*SessionCounter)`: a value-receiver method would receive a copy of the whole
struct — including the mutex — on every call. Second, callers must pass
`*SessionCounter`, never `SessionCounter`, and must never range over a
`[]SessionCounter` by value (each iteration copies the element, mutex and all).

Go's tooling enforces this: `go vet` runs the `copylocks` analyzer, which reports
any copy of a value whose type (transitively) contains a `sync.Locker`. That is
why the gate for this module includes `go vet` — the correct code must pass it.

Here is the shape that **breaks** it. Do not add this to the module; it exists to
show what `copylocks` flags:

```
// WRONG: taking the counter by value copies the embedded mutex.
// `go vet` reports: passes lock by value: SessionCounter contains sync.Mutex
func totalOf(c SessionCounter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// WRONG: ranging by value copies each element's mutex per iteration.
for _, c := range counters { // counters is []SessionCounter
	_ = c.Active()
}
```

Now the correct version. The mutex is embedded (an anonymous `sync.Mutex` field),
so `Lock`/`Unlock` are promoted onto `*SessionCounter` directly.

Create `counter.go`:

```go
package counter

import "sync"

// SessionCounter tracks the number of open sessions. It embeds a sync.Mutex,
// which means it must never be copied: always pass *SessionCounter.
type SessionCounter struct {
	sync.Mutex
	count int
}

// Open records a new session.
func (c *SessionCounter) Open() {
	c.Lock()
	defer c.Unlock()
	c.count++
}

// Close records a session ending. It never goes below zero.
func (c *SessionCounter) Close() {
	c.Lock()
	defer c.Unlock()
	if c.count > 0 {
		c.count--
	}
}

// Active returns the current number of open sessions.
func (c *SessionCounter) Active() int {
	c.Lock()
	defer c.Unlock()
	return c.count
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/copylocks"
)

func main() {
	c := &counter.SessionCounter{} // pointer: never copy a mutex-bearing struct

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Open()
		}()
	}
	wg.Wait()

	// Close half of them.
	for range 40 {
		c.Close()
	}
	fmt.Println("active:", c.Active())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
active: 60
```

### Tests

`TestConcurrentOpenClose` is the real proof: many goroutines `Open` and a known
number `Close`, and the final `Active()` must be the exact arithmetic result under
`-race`. If the mutex were being copied, this would race and the count would drift.
`TestSatisfiesCounterInterface` is a compile-time assertion that `*SessionCounter`
implements a `Counter` interface built from the pointer-receiver methods; the
comment explains why a plain `SessionCounter` value does not.

Create `counter_test.go`:

```go
package counter

import (
	"fmt"
	"sync"
	"testing"
)

// Counter is satisfied by *SessionCounter because Open/Close/Active have pointer
// receivers, so they are in the method set of *SessionCounter, not SessionCounter.
type Counter interface {
	Open()
	Close()
	Active() int
}

// Compile-time assertion: *SessionCounter satisfies Counter.
// A plain SessionCounter value would NOT satisfy it, because a pointer-receiver
// method is not in the value type's method set. Uncommenting the next line would
// fail to compile:
//
//	var _ Counter = SessionCounter{}
var _ Counter = (*SessionCounter)(nil)

func TestConcurrentOpenClose(t *testing.T) {
	t.Parallel()
	c := &SessionCounter{}

	const opens = 500
	const closes = 200

	var wg sync.WaitGroup
	for range opens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Open()
		}()
	}
	wg.Wait()

	for range closes {
		c.Close()
	}

	if got := c.Active(); got != opens-closes {
		t.Fatalf("Active() = %d, want %d", got, opens-closes)
	}
}

func TestCloseNeverGoesNegative(t *testing.T) {
	t.Parallel()
	c := &SessionCounter{}
	c.Close()
	c.Close()
	if got := c.Active(); got != 0 {
		t.Fatalf("Active() = %d, want 0", got)
	}
}

func ExampleSessionCounter() {
	c := &SessionCounter{}
	c.Open()
	c.Open()
	c.Close()
	fmt.Println(c.Active())
	// Output: 1
}
```

## Review

The design is correct when the count is exact under concurrency, which is only
true if there is exactly one mutex guarding exactly one `count` — that is, if the
struct is never copied. The two enforcement mechanisms work together: `go vet`'s
`copylocks` catches copies at build time (that is why this module's gate runs
`go vet`), and `-race` catches any unsynchronized access at run time. The method
set is the other lesson: pointer-receiver methods are in `*T`'s method set only,
so `*SessionCounter` satisfies the `Counter` interface but a `SessionCounter`
value does not — which is exactly why the compile-time assertion uses the pointer
form. The mistakes to avoid: a value-receiver method on a mutex-bearing type (it
copies the mutex on every call), passing such a struct by value, and ranging over
a slice of them by value. When you must iterate, use an index: `for i := range s { s[i].Open() }`.

## Resources

- [`sync` package](https://pkg.go.dev/sync) — "A Mutex must not be copied after first use."
- [`go vet` copylocks](https://pkg.go.dev/cmd/vet) — the analyzer that flags copied locks.
- [Go Spec: method sets](https://go.dev/ref/spec#Method_sets) — why a pointer-receiver method is not in the value type's method set.
- [Go Code Review Comments: receiver type](https://go.dev/wiki/CodeReviewComments#receiver-type) — when to use a pointer receiver.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-comparable-structs-as-map-keys.md](05-comparable-structs-as-map-keys.md)
