# Exercise 10: Serialize a Connection Registry with a Command Channel

**Level: Intermediate**

A WebSocket gateway keeps a live registry mapping client id to session metadata.
Under load, connects, disconnects, lookups, and a periodic count all hit that map
concurrently, and a mutex-guarded map turns into a contention hot spot that is easy
to deadlock or forget to lock. This exercise keeps the map inside one owner
goroutine reached only through a command channel, so there is no shared mutable
state and no data race by construction — the multi-command variant of the actor
loop, where each caller sends one of several tagged commands, each carrying its own
private reply channel.

This module is self-contained: its own module, a `connreg` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
connreg/                     independent module: example.com/connreg
  go.mod                     go 1.26
  connreg.go                 type Conn, Registry; New, Register, Deregister, Lookup, Count, Close
  cmd/demo/main.go           runnable demo: register, dup, lookup, count, deregister, close
  connreg_test.go            semantics, duplicate/absent, 100 concurrent mixed ops, after-close, double-close
```

- Files: `connreg.go`, `cmd/demo/main.go`, `connreg_test.go`.
- Implement: `New() *Registry`, `(*Registry).Register(Conn) bool`, `Deregister(string) bool`, `Lookup(string) (Conn, bool)`, `Count() int`, `Close()`, with all map state owned by one goroutine reached only through channels.
- Test: register/lookup/deregister semantics; duplicate register returns false without overwriting; deregister of an absent id returns false; 100 concurrent mixed operations run clean under `-race`; count reflects net live registrations; every operation after `Close` returns its zero result; double `Close` does not panic.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/connreg/cmd/demo
cd ~/go-exercises/connreg
go mod init example.com/connreg
```

### One channel, several tagged commands

The single-operation actor loop (the monotonic ID generator) had exactly one thing
to ask: "give me the next value". A registry has four: register, deregister, look
up, count. The naive extension — one channel per operation — forces the loop into a
four-way `select` and scatters the protocol. The idiomatic move is one command
channel carrying a *tagged* command struct:

1. `command` has an `op kind` tag, the payload each operation needs (`conn` for
   register, `id` for deregister and lookup), and a private `reply chan result`.
   One struct type covers every operation, so the loop reads a single channel and
   dispatches on `c.op` with a `switch`.
2. `result` is the loop's answer. Which field is meaningful depends on the
   operation: `ok` for register/deregister/lookup, `conn` for lookup, `count` for
   count. Packing them into one struct keeps every reply channel the same type.
3. The owner loop holds the `map[string]Conn` as a local variable. Nobody else has
   a reference to it. Because the loop processes one command at a time, the map is
   mutated by exactly one goroutine and never needs a lock — the serialization is a
   consequence of the loop, not of any mutex.

Each call allocates a fresh `reply` channel and blocks receiving on it. That
per-call rendezvous is how the loop routes *this* answer back to *this* caller with
no correlation id and no shared map of pending requests.

The duplicate-register rule lives entirely inside the loop: on `opRegister` it
checks presence first and answers `ok:false` without touching the map when the id
already exists, so an existing session is never silently overwritten. Deregister is
symmetric: `ok:false` when the id is absent, `delete` and `ok:true` when present.

### Shutting the loop down without a panic or a hang

Lifecycle is the hard part of the actor pattern. `Close` must stop the loop, and
any operation that races or follows `Close` must neither panic on a dead loop nor
block forever. A separate `done` channel plus a `select` on both sides handles it:

- The loop `select`s between receiving a command and observing `done` closed; when
  `done` fires it returns and the goroutine ends.
- Every public method funnels through `do`, which `select`s between sending its
  command and observing `done`; if the loop is gone the `done` case wins and `do`
  returns the zero `result`. That zero result maps to exactly the documented
  no-op: `false` for register/deregister, empty `Conn` and `false` for lookup, `0`
  for count.

`Close` is guarded by `sync.Once`, so two shutdown paths — a `defer` plus an
explicit call, say — never panic on a double `close(done)`. There is no mutex
anywhere near the map; the only synchronization primitive is the `Once` guarding
lifecycle.

Create `connreg.go`:

```go
package connreg

import "sync"

// Conn is the session metadata the gateway tracks for one live client.
type Conn struct {
	ID   string
	Node string
}

// kind tags which operation a command asks the owner loop to perform.
type kind int

const (
	opRegister kind = iota
	opDeregister
	opLookup
	opCount
)

// command is one caller's request. It carries the operation tag, whatever
// payload that operation needs, and a private reply channel the owner loop
// answers on. One struct type covers every operation so the loop reads a
// single channel.
type command struct {
	op    kind
	conn  Conn        // opRegister
	id    string      // opDeregister, opLookup
	reply chan result
}

// result is the loop's answer. Which fields are meaningful depends on op:
// ok for register/deregister/lookup, conn for lookup, count for count.
type result struct {
	conn  Conn
	ok    bool
	count int
}

// Registry maps client id to session metadata. The map lives inside one owner
// goroutine and is reached only through the commands channel, so there is no
// shared mutable state and no data race by construction.
type Registry struct {
	commands  chan command
	done      chan struct{}
	closeOnce sync.Once
}

// New starts the owner loop and returns a ready Registry.
func New() *Registry {
	r := &Registry{
		commands: make(chan command),
		done:     make(chan struct{}),
	}
	go r.loop()
	return r
}

// loop is the sole owner of conns. It processes one command at a time, so no
// two operations ever touch the map concurrently.
func (r *Registry) loop() {
	conns := make(map[string]Conn)
	for {
		select {
		case c := <-r.commands:
			switch c.op {
			case opRegister:
				if _, exists := conns[c.conn.ID]; exists {
					c.reply <- result{ok: false} // duplicate: do not overwrite
				} else {
					conns[c.conn.ID] = c.conn
					c.reply <- result{ok: true}
				}
			case opDeregister:
				if _, exists := conns[c.id]; exists {
					delete(conns, c.id)
					c.reply <- result{ok: true}
				} else {
					c.reply <- result{ok: false}
				}
			case opLookup:
				conn, ok := conns[c.id]
				c.reply <- result{conn: conn, ok: ok}
			case opCount:
				c.reply <- result{count: len(conns)}
			}
		case <-r.done:
			return
		}
	}
}

// do sends one command to the loop and waits for its reply. The select guards
// against Close: if the loop is gone, the done case wins and do returns the
// zero result instead of blocking forever or panicking on a closed loop.
func (r *Registry) do(c command) result {
	c.reply = make(chan result)
	select {
	case r.commands <- c:
		return <-c.reply
	case <-r.done:
		return result{}
	}
}

// Register adds c. It returns false if c.ID is already present and does not
// overwrite the existing entry.
func (r *Registry) Register(c Conn) bool {
	return r.do(command{op: opRegister, conn: c}).ok
}

// Deregister removes id. It returns false if id was not present.
func (r *Registry) Deregister(id string) bool {
	return r.do(command{op: opDeregister, id: id}).ok
}

// Lookup returns the Conn for id and whether it was present.
func (r *Registry) Lookup(id string) (Conn, bool) {
	res := r.do(command{op: opLookup, id: id})
	return res.conn, res.ok
}

// Count returns the number of live registrations.
func (r *Registry) Count() int {
	return r.do(command{op: opCount}).count
}

// Close stops the loop. It is idempotent, and operations after Close are safe
// no-ops that return the zero result (false / empty Conn / 0).
func (r *Registry) Close() {
	r.closeOnce.Do(func() { close(r.done) })
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/connreg"
)

func main() {
	r := connreg.New()

	fmt.Println("register c1:", r.Register(connreg.Conn{ID: "c1", Node: "edge-a"}))
	fmt.Println("register c2:", r.Register(connreg.Conn{ID: "c2", Node: "edge-b"}))
	fmt.Println("register c1 again:", r.Register(connreg.Conn{ID: "c1", Node: "edge-z"}))

	c, ok := r.Lookup("c1")
	fmt.Printf("lookup c1: %+v present=%v\n", c, ok)

	_, ok = r.Lookup("c9")
	fmt.Println("lookup c9 present:", ok)

	fmt.Println("count:", r.Count())
	fmt.Println("deregister c2:", r.Deregister("c2"))
	fmt.Println("deregister c2 again:", r.Deregister("c2"))
	fmt.Println("count:", r.Count())

	r.Close()
	r.Close() // idempotent

	fmt.Println("register after close:", r.Register(connreg.Conn{ID: "c3", Node: "edge-c"}))
	fmt.Println("count after close:", r.Count())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
register c1: true
register c2: true
register c1 again: false
lookup c1: {ID:c1 Node:edge-a} present=true
lookup c9 present: false
count: 2
deregister c2: true
deregister c2 again: false
count: 1
register after close: false
count after close: 0
```

### Tests

`TestRegisterLookupDeregister` pins the basic contract end to end. `TestDuplicate`
`RegisterDoesNotOverwrite` proves a second register on a live id returns false and
leaves the original `Node` intact — the overwrite bug this rule prevents.
`TestDeregisterAbsentReturnsFalse` covers the missing-id case.
`TestCountReflectsLiveRegistrations` walks count through registrations, a duplicate
that must not change it, and a deregister. `TestConcurrentMixedOperations` is the
core proof: 100 ids are pre-registered, then 100 goroutines each fire a mix of
lookup, count, deregister, and duplicate-register; because the loop is the sole map
owner, the final count is exactly the surviving half and every id's presence
matches its parity, with no lost or duplicated entries — a clean `-race` run is the
evidence that only the loop touches the map. `TestOperationsAfterCloseAreZeroValued`
checks every method returns its zero result after `Close`, and
`TestDoubleCloseDoesNotPanic` confirms the `sync.Once` guard. The `ExampleRegistry`
doubles as documentation of the duplicate rule.

Create `connreg_test.go`:

```go
package connreg

import (
	"fmt"
	"sync"
	"testing"
)

func TestRegisterLookupDeregister(t *testing.T) {
	t.Parallel()
	r := New()
	defer r.Close()

	if !r.Register(Conn{ID: "a", Node: "n1"}) {
		t.Fatal("Register(a) = false, want true")
	}
	got, ok := r.Lookup("a")
	if !ok || got.Node != "n1" {
		t.Fatalf("Lookup(a) = %+v,%v, want {a n1},true", got, ok)
	}
	if !r.Deregister("a") {
		t.Fatal("Deregister(a) = false, want true")
	}
	if _, ok := r.Lookup("a"); ok {
		t.Fatal("Lookup(a) after deregister = present, want absent")
	}
}

func TestDuplicateRegisterDoesNotOverwrite(t *testing.T) {
	t.Parallel()
	r := New()
	defer r.Close()

	if !r.Register(Conn{ID: "a", Node: "first"}) {
		t.Fatal("first Register(a) = false, want true")
	}
	if r.Register(Conn{ID: "a", Node: "second"}) {
		t.Fatal("duplicate Register(a) = true, want false")
	}
	got, _ := r.Lookup("a")
	if got.Node != "first" {
		t.Fatalf("Lookup(a).Node = %q, want %q (overwritten)", got.Node, "first")
	}
}

func TestDeregisterAbsentReturnsFalse(t *testing.T) {
	t.Parallel()
	r := New()
	defer r.Close()

	if r.Deregister("missing") {
		t.Fatal("Deregister(missing) = true, want false")
	}
}

func TestCountReflectsLiveRegistrations(t *testing.T) {
	t.Parallel()
	r := New()
	defer r.Close()

	if n := r.Count(); n != 0 {
		t.Fatalf("Count on empty = %d, want 0", n)
	}
	r.Register(Conn{ID: "a"})
	r.Register(Conn{ID: "b"})
	r.Register(Conn{ID: "a"}) // duplicate, no effect
	if n := r.Count(); n != 2 {
		t.Fatalf("Count = %d, want 2", n)
	}
	r.Deregister("a")
	if n := r.Count(); n != 1 {
		t.Fatalf("Count after deregister = %d, want 1", n)
	}
}

func TestConcurrentMixedOperations(t *testing.T) {
	t.Parallel()
	r := New()
	defer r.Close()

	const n = 100
	// Pre-register everyone so lookups and deregisters have real targets.
	for i := range n {
		if !r.Register(Conn{ID: fmt.Sprintf("c%d", i), Node: "node"}) {
			t.Fatalf("setup Register(c%d) = false", i)
		}
	}
	if got := r.Count(); got != n {
		t.Fatalf("Count after setup = %d, want %d", got, n)
	}

	// 100 goroutines each run a mix of operations. Even ids are deregistered;
	// odd ids stay. Re-registering a present id must return false.
	var wg sync.WaitGroup
	for i := range n {
		id := fmt.Sprintf("c%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Lookup(id)
			r.Count()
			if i%2 == 0 {
				r.Deregister(id)
			} else {
				if r.Register(Conn{ID: id, Node: "dup"}) {
					t.Errorf("duplicate Register(%s) = true, want false", id)
				}
			}
			r.Lookup(id)
		}()
	}
	wg.Wait()

	// Exactly the odd ids remain: n/2 live entries, none lost, none duplicated.
	if got := r.Count(); got != n/2 {
		t.Fatalf("Count after mixed ops = %d, want %d", got, n/2)
	}
	for i := range n {
		id := fmt.Sprintf("c%d", i)
		_, ok := r.Lookup(id)
		wantPresent := i%2 == 1
		if ok != wantPresent {
			t.Fatalf("Lookup(%s) present=%v, want %v", id, ok, wantPresent)
		}
	}
}

func TestOperationsAfterCloseAreZeroValued(t *testing.T) {
	t.Parallel()
	r := New()
	r.Register(Conn{ID: "a"})
	r.Close()

	if r.Register(Conn{ID: "b"}) {
		t.Fatal("Register after Close = true, want false")
	}
	if r.Deregister("a") {
		t.Fatal("Deregister after Close = true, want false")
	}
	if c, ok := r.Lookup("a"); ok || c != (Conn{}) {
		t.Fatalf("Lookup after Close = %+v,%v, want {},false", c, ok)
	}
	if n := r.Count(); n != 0 {
		t.Fatalf("Count after Close = %d, want 0", n)
	}
}

func TestDoubleCloseDoesNotPanic(t *testing.T) {
	t.Parallel()
	r := New()
	r.Close()
	r.Close()
	r.Close()
}

func ExampleRegistry() {
	r := New()
	defer r.Close()

	fmt.Println(r.Register(Conn{ID: "c1", Node: "edge-a"}))
	fmt.Println(r.Register(Conn{ID: "c1", Node: "edge-b"}))
	fmt.Println(r.Count())
	// Output:
	// true
	// false
	// 1
}
```

## Review

The registry is correct when the map is provably touched by exactly one goroutine,
so no connect/disconnect/lookup/count can ever interleave a half-updated map. The
tagged `command` on one channel plus a per-call `reply` channel is what serializes
them: the loop dequeues one command, applies it in full, and answers before
touching the next, which is why 100 concurrent mixed operations leave exactly the
expected survivors with no lost or duplicated entries under `-race`. The two
lifecycle traps are the ones the design closes: sending on `commands` without a
`select` on `done` would let an operation racing `Close` block forever or panic on
a dead loop, and closing `done` without `sync.Once` would panic on a second
`Close`; the `do` helper's `select` and the `Once` guard eliminate both. There is
no mutex anywhere near the map — that absence is the lesson. This pattern prevents
the classic production bug of a concurrently-accessed session map: a forgotten lock
on one code path, a data race that corrupts the map, and a gateway that hands one
client another client's session.

## Resources

- [Go Blog: Share Memory By Communicating](https://go.dev/doc/codewalk/sharemem/) -- the command-channel codewalk this registry generalizes to multiple operations.
- [The Go Memory Model: channel communication](https://go.dev/ref/mem#chan) -- why a value published by the loop is safely visible to the caller that receives it, with no extra synchronization.
- [`sync.Once`](https://pkg.go.dev/sync#Once) -- the exactly-once guard that makes `Close` idempotent.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- the select-based send/receive discipline the shutdown path relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-ordered-results-index.md](09-ordered-results-index.md) | Next: [11-prefetched-key-broadcast-future.md](11-prefetched-key-broadcast-future.md)
