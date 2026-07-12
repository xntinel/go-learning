# Exercise 29: Broadcast Observer Pattern: Per-Observer Recovery on Notification

**Nivel: Intermedio** — validacion rapida (un test corto).

An event-driven backend often fans one domain event out to several
independent observers — send a confirmation email, update a read model,
write an audit log — registered by unrelated teams over time. A bug in one
observer (a nil map somewhere in the analytics hook) must never stop a
completely unrelated observer (the order-confirmation email) from running,
and the system needs to know precisely which observers succeeded and which
failed for this event, not just "something went wrong somewhere." This
module builds `Registry.Notify`, which calls every registered observer in
order and isolates each one's panic so the rest of the list always runs. It
is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
broadcast/                  independent module: example.com/broadcast
  go.mod                     go 1.24
  broadcast.go                 Observer, Outcome, Registry, NewRegistry, Register, Notify
  cmd/
    demo/
      main.go                runnable demo: email, inventory (panics), audit observers
  broadcast_test.go            panic isolated mid-list, empty registry
```

Files: `broadcast.go`, `cmd/demo/main.go`, `broadcast_test.go`.
Implement: `Registry.Notify(event string) []Outcome` that calls every registered `Observer.Handle` in registration order, isolating each call's panic via `callObserver` so a panic in observer *k* never prevents observer *k+1* from running.
Test: three observers where the middle one panics with a nil map write, asserting both the first and third observers actually ran and that the returned outcomes correctly separate the two successes from the one panic; an empty registry.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/29-broadcast-observer-registry/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/29-broadcast-observer-registry
go mod edit -go=1.24
```

### Why isolation must happen per observer, not around the whole loop

The tempting shortcut is a single `defer recover()` wrapped around the
entire `Notify` loop. That is wrong in a way that only shows up in
production: once any one observer panics, that outer recover stops the
unwind at the *top* of `Notify`, which means every observer registered
*after* the one that panicked never runs at all - the loop itself is
already gone by the time the recover fires. `Notify` instead puts the
recover boundary inside `callObserver`, called once per observer, so a
panic is caught and converted into that single observer's `Outcome` before
the loop ever advances past it; the remaining observers are entirely
unaffected, exactly as if the failing observer had simply returned an
error instead of panicking. This is the same "per-item boundary, not a
per-batch boundary" principle that isolates a single bad record in a batch
job or a single misbehaving worker in a pool - one broken participant
degrades that participant's own outcome, not the whole broadcast.

Create `broadcast.go`:

```go
package broadcast

import "fmt"

// Observer is one registered listener: Handle is called once per event.
type Observer struct {
	Name   string
	Handle func(event string)
}

// Outcome is one observer's result from a single Notify call.
type Outcome struct {
	Name string
	Err  error
}

// Registry holds the set of observers to notify on each event.
type Registry struct {
	observers []Observer
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds an observer. Observers are notified in registration order.
func (r *Registry) Register(o Observer) {
	r.observers = append(r.observers, o)
}

// Notify calls every registered observer's Handle with event, in
// registration order. One observer's Handle panicking - a nil map write, a
// bad type assertion in a hastily-added integration - must never stop the
// remaining observers from being notified: a payment-confirmation email
// going out cannot be allowed to depend on whether an unrelated analytics
// hook happens to have a bug today. Each observer's call is isolated by its
// own recover boundary, so Notify always finishes the full observer list
// and reports one Outcome per observer, whether it succeeded or panicked.
func (r *Registry) Notify(event string) []Outcome {
	outcomes := make([]Outcome, 0, len(r.observers))
	for _, o := range r.observers {
		outcomes = append(outcomes, Outcome{Name: o.Name, Err: callObserver(o, event)})
	}
	return outcomes
}

// callObserver is the recover boundary: exactly one observer's untrusted
// Handle call.
func callObserver(o Observer, event string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("observer %q panicked: %w", o.Name, e)
				return
			}
			err = fmt.Errorf("observer %q panicked: %v", o.Name, r)
		}
	}()
	o.Handle(event)
	return nil
}
```

### The runnable demo

Three observers react to an `order.placed` event: `email` sends a
confirmation, `inventory` panics on a nil map write, and `audit` still logs
the event afterward.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/broadcast"
)

func main() {
	reg := broadcast.NewRegistry()

	reg.Register(broadcast.Observer{
		Name: "email",
		Handle: func(event string) {
			fmt.Printf("email: sending confirmation for %s\n", event)
		},
	})
	reg.Register(broadcast.Observer{
		Name: "inventory",
		Handle: func(event string) {
			var stock map[string]int
			stock["sku-42"]-- // nil map write panics
		},
	})
	reg.Register(broadcast.Observer{
		Name: "audit",
		Handle: func(event string) {
			fmt.Printf("audit: logged %s\n", event)
		},
	})

	outcomes := reg.Notify("order.placed")
	for _, o := range outcomes {
		status := "ok"
		if o.Err != nil {
			status = o.Err.Error()
		}
		fmt.Printf("%s: %s\n", o.Name, status)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
email: sending confirmation for order.placed
audit: logged order.placed
email: ok
inventory: observer "inventory" panicked: assignment to entry in nil map
audit: ok
```

### Tests

`TestNotifyContinuesAfterObserverPanics` registers three observers where
the middle one panics, and asserts both the surrounding observers actually
ran (via a shared slice they append to) and that `Notify`'s returned
outcomes correctly separate the two successes from the one panic.

Create `broadcast_test.go`:

```go
package broadcast

import (
	"strings"
	"testing"
)

func TestNotifyContinuesAfterObserverPanics(t *testing.T) {
	reg := NewRegistry()
	var calls []string

	reg.Register(Observer{Name: "a", Handle: func(event string) { calls = append(calls, "a:"+event) }})
	reg.Register(Observer{Name: "b", Handle: func(event string) {
		var m map[string]int
		m["x"] = 1
	}})
	reg.Register(Observer{Name: "c", Handle: func(event string) { calls = append(calls, "c:"+event) }})

	outcomes := reg.Notify("ping")

	if len(calls) != 2 || calls[0] != "a:ping" || calls[1] != "c:ping" {
		t.Fatalf("calls = %v, want both a and c to run despite b panicking", calls)
	}
	if len(outcomes) != 3 {
		t.Fatalf("len(outcomes) = %d, want 3", len(outcomes))
	}
	if outcomes[0].Err != nil || outcomes[2].Err != nil {
		t.Fatalf("outcomes = %+v, want a and c to succeed", outcomes)
	}
	if outcomes[1].Err == nil || !strings.Contains(outcomes[1].Err.Error(), "panicked") {
		t.Fatalf("outcomes[1].Err = %v, want a panic breadcrumb", outcomes[1].Err)
	}
}

func TestNotifyNoObservers(t *testing.T) {
	reg := NewRegistry()
	outcomes := reg.Notify("noop")
	if len(outcomes) != 0 {
		t.Fatalf("outcomes = %v, want empty", outcomes)
	}
}
```

## Review

`Notify` is correct when the recover boundary sits inside the per-observer
call, not around the loop that drives it — that is the entire difference
between "one bad observer loses its own outcome" and "one bad observer
silently cancels every observer registered after it." The second failure
mode is especially dangerous because it is order-dependent: the same buggy
observer could be harmless or could black out half the broadcast list,
purely depending on registration order, which makes it exactly the kind of
bug that passes code review and then fails unpredictably in production as
observers get added over time.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — why a recover placed around a loop only stops the panic, it does not resume the loop.
- [Observer pattern](https://refactoring.guru/design-patterns/observer) — the general shape this registry implements with Go-specific failure isolation.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-lease-renewal-background-panic.md](28-lease-renewal-background-panic.md) | Next: [30-distributed-tracing-child-spans.md](30-distributed-tracing-child-spans.md)
