# Exercise 5: Value vs Pointer Embedding and the copylocks Trap

Embedding a `sync.Mutex` by value has a consequence that catches people in
production: the containing struct becomes non-copyable, and copying it silently
breaks mutual exclusion. `go vet`'s copylocks analyzer exists to catch exactly
this. Here you ship a correct, vet-clean counter and study the copy bug beside it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
counter/                   independent module: example.com/counter
  go.mod                   go 1.26
  counter.go               Counter embeds sync.Mutex by value; pointer-receiver methods; never copied
  cmd/
    demo/
      main.go              runnable demo: concurrent increments, print the total
  counter_test.go          -race concurrency; the total is exact; vet stays clean
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: a `Counter` embedding `sync.Mutex` with `Inc`/`Add`/`Value` on pointer receivers, used only through a pointer so it is never copied.
- Test: many goroutines increment concurrently under `-race`; the final value is exact; and `go vet` (run by the gate) reports no copylocks issue.
- Verify: `go test -count=1 -race ./...` and `go vet ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/counter/cmd/demo
cd ~/go-exercises/counter
go mod init example.com/counter
```

### Why a value-embedded mutex makes the struct non-copyable

A `sync.Mutex` is a small struct holding lock state. The rule from its
documentation is blunt: "A Mutex must not be copied after first use." A copy has
its own independent state word, so if you lock the original and then work with a
copy, the copy's lock is unheld and a second goroutine sails straight through —
mutual exclusion is gone, silently, with no panic. Embedding the mutex by value
makes the *whole* `Counter` inherit this restriction: copying a `Counter` copies
its mutex, so a `Counter` must not be copied after first use either.

Copies happen in more places than an explicit `b := a`. Passing the struct to a
function by value copies it. Appending it to a `[]Counter` copies it. Storing it as
a map value copies it. Returning it by value from a function copies it. Ranging
over a `[]Counter` with `for _, c := range s` copies each element into `c`. Every
one of these is a copylocks violation, and `go vet` flags all of them:
`assignment copies lock value` or `func passes lock by value`. The gate runs
`go vet`, so shipped code with any such copy fails outright.

The discipline that keeps it correct: give every method a *pointer* receiver
(`func (c *Counter) Inc()`), and only ever handle `*Counter`, never `Counter`. A
pointer receiver does not copy the struct — the method operates on the one real
`Counter` — and passing `*Counter` around copies the pointer, not the mutex. That
is why concurrency-bearing types are constructed with `New` returning `*Counter`
and passed by pointer everywhere. (When you genuinely need to share one lock across
values, embed `*sync.Mutex` instead; then the pointer is shared and copying the
struct copies the pointer, which is fine — but that is a deliberate sharing
decision, not the default.)

Create `counter.go`:

```go
package counter

import "sync"

// Counter is a concurrency-safe integer. It embeds sync.Mutex by value, which
// makes Counter non-copyable: it must be used only through a pointer, and every
// method takes a pointer receiver so the struct (and its lock) is never copied.
type Counter struct {
	sync.Mutex
	n int
}

// New returns a ready Counter. Callers hold the *Counter and never copy it.
func New() *Counter {
	return &Counter{}
}

// Inc adds one under the promoted lock.
func (c *Counter) Inc() {
	c.Lock()
	c.n++
	c.Unlock()
}

// Add adds delta under the promoted lock.
func (c *Counter) Add(delta int) {
	c.Lock()
	c.n += delta
	c.Unlock()
}

// Value reads the current total under the promoted lock.
func (c *Counter) Value() int {
	c.Lock()
	defer c.Unlock()
	return c.n
}
```

The bug this design avoids, shown as an illustration only (do not add it to the
package — `go vet` would reject it):

```go
// WRONG: value receiver copies the Counter, including its Mutex, on every call.
// go vet: "printValue passes lock by value: counter.Counter contains sync.Mutex".
func printValue(c Counter) int { // c is a COPY; its lock is a different lock
	return c.n
}

// WRONG: storing Counters by value in a slice copies each one.
// go vet: "range var c copies lock: counter.Counter contains sync.Mutex".
var counters []Counter
for _, c := range counters {
	_ = c
}
```

### The runnable demo

The demo increments one shared `*Counter` from many goroutines and prints the
exact total, which is deterministic because the lock serializes the writes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/counter"
)

func main() {
	c := counter.New()
	var wg sync.WaitGroup
	for range 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()

	fmt.Println("total:", c.Value())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total: 1000
```

### Tests

`TestConcurrentInc` runs many goroutines against one `*Counter` and asserts the
total is exactly the number of increments — the property that fails the instant the
lock is copied or missing. `TestAdd` covers the `Add` path. Both run under `-race`,
and because the code has no by-value copy of `Counter`, the gate's `go vet` stays
clean; that clean vet is itself part of what these tests defend.

Create `counter_test.go`:

```go
package counter

import (
	"sync"
	"testing"
)

func TestConcurrentInc(t *testing.T) {
	t.Parallel()
	c := New()
	const n = 5000
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()

	if got := c.Value(); got != n {
		t.Fatalf("Value = %d, want %d (a copied or missing lock loses increments)", got, n)
	}
}

func TestAdd(t *testing.T) {
	t.Parallel()
	c := New()
	const workers = 100
	const perWorker = 50
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				c.Add(1)
			}
		}()
	}
	wg.Wait()

	if got, want := c.Value(), workers*perWorker; got != want {
		t.Fatalf("Value = %d, want %d", got, want)
	}
}
```

## Review

The counter is correct when the total under concurrency is exact, which
`TestConcurrentInc` asserts against 5000 increments — a copied lock would drop
some and the count would come up short. The design rules that make it correct are
mechanical: value-embed the mutex, take pointer receivers on every method, hand out
`*Counter` from `New`, and never copy a `Counter`. The gate's `go vet` is the
backstop: any accidental by-value copy — a value receiver, a `[]Counter`, a map
value, a value return — trips copylocks and fails the build, which is the tooling
doing its job. When you actually want to share one lock across several structs,
embed `*sync.Mutex` and copy the pointer deliberately; the value-embed default is
for a single owned instance.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the "must not be copied after first use" rule.
- [copylocks analyzer](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/copylock) — what `go vet` checks and the diagnostics it emits.
- [Go Code Review Comments: copying](https://go.dev/wiki/CodeReviewComments#copying) — why copying a value with an embedded lock is a bug.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-base-model-embedding-json.md](06-base-model-embedding-json.md)
