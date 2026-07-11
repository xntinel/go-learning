# Exercise 7: Method Values vs Method Expressions in Handler Registration

When you register a handler by passing `svc.Handle` — a *method value* — what gets
captured depends entirely on the receiver kind, and getting it wrong is a subtle
production trap: a handler bound from a value receiver freezes a snapshot of the
service at registration time and never sees a later config change. This module
builds a tiny handler `Router`, demonstrates the freeze, and shows why a pointer
receiver captures live state.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
router/                    independent module: example.com/router
  go.mod
  router.go                Router; LiveService (*ptr receiver); FrozenService (value receiver)
  cmd/
    demo/
      main.go              register both, mutate the service, run the handlers
  router_test.go           value receiver freezes; pointer receiver sees updates
```

Files: `router.go`, `cmd/demo/main.go`, `router_test.go`.
Implement: a `Router` storing a `func() string` handler; `LiveService` with a pointer-receiver `Greeting()` method; `FrozenService` with a value-receiver `Greeting()` method; register each service's method value and observe capture semantics.
Test: registering `FrozenService.Greeting` freezes the receiver (a later mutation is invisible); registering `*LiveService.Greeting` captures the pointer (a later mutation is visible).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/router/cmd/demo
cd ~/go-exercises/router
go mod init example.com/router
```

### What a method value captures

A *method value* is what you get when you write `svc.Greeting` without calling it:
a `func() string` with the receiver already bound. The binding happens at the
moment you evaluate `svc.Greeting`, and it follows the receiver kind:

- If `Greeting` has a **value** receiver, evaluating `svc.Greeting` copies `svc`
  *right then* and stores the copy inside the func. Later writes to `svc` do not
  touch that frozen copy, so the registered handler keeps returning the value the
  service had at registration time.
- If `Greeting` has a **pointer** receiver, evaluating `svc.Greeting` captures the
  *pointer* `&svc`. The func holds a live reference, so later writes through that
  pointer are visible when the handler runs.

This is the whole bug. Imagine middleware wiring: you build a service, register
`svc.Handle` in your router, then load config that mutates the service. If
`Handle` had a value receiver, the registered handler is reading a snapshot from
before the config load — the change is silently ignored. Swap to a pointer
receiver and the same registration captures the live service. The trap is quiet
because both versions compile and run; only the *timing* of what the handler sees
differs.

A related tool is the *method expression*: `FrozenService.Greeting` (type
`func(FrozenService) string`) or `(*LiveService).Greeting` (type
`func(*LiveService) string`). A method expression does not bind a receiver at all;
it makes the receiver an explicit first parameter, so the copy-vs-pointer choice
is visible in the signature and deferred to call time. The demo uses one to make
the semantics concrete.

Create `router.go`:

```go
package router

// Router registers a single handler as a func with the receiver already bound.
type Router struct {
	handler func() string
}

// Register stores a handler (typically a method value like svc.Greeting).
func (r *Router) Register(h func() string) {
	r.handler = h
}

// Serve invokes the registered handler.
func (r *Router) Serve() string {
	if r.handler == nil {
		return ""
	}
	return r.handler()
}

// LiveService exposes Greeting through a POINTER receiver. A method value
// svc.Greeting captures the pointer, so later mutations to Message are visible.
type LiveService struct {
	Message string
}

func (s *LiveService) Greeting() string {
	return s.Message
}

// FrozenService exposes Greeting through a VALUE receiver. A method value
// svc.Greeting copies and freezes the receiver at bind time, so later mutations
// to Message are invisible to the bound handler.
type FrozenService struct {
	Message string
}

func (s FrozenService) Greeting() string {
	return s.Message
}
```

### The runnable demo

The demo registers a handler from each service, mutates each service's `Message`
after registration, then serves — showing the frozen handler stuck on its old
message while the live one reflects the update. It also shows a method expression
applied explicitly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/router"
)

func main() {
	// Value receiver: method value freezes a copy at registration.
	frozen := router.FrozenService{Message: "v1"}
	var frozenRouter router.Router
	frozenRouter.Register(frozen.Greeting)
	frozen.Message = "v2" // changed AFTER registration
	fmt.Printf("frozen handler serves: %s\n", frozenRouter.Serve())

	// Pointer receiver: method value captures the live pointer.
	live := &router.LiveService{Message: "v1"}
	var liveRouter router.Router
	liveRouter.Register(live.Greeting)
	live.Message = "v2" // changed AFTER registration
	fmt.Printf("live handler serves:   %s\n", liveRouter.Serve())

	// Method expression: receiver is an explicit parameter, resolved at call time.
	expr := (*router.LiveService).Greeting
	fmt.Printf("via method expression: %s\n", expr(live))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
frozen handler serves: v1
live handler serves:   v2
via method expression: v2
```

### Tests

`TestValueReceiverMethodValueFreezesReceiver` documents the trap: bind
`FrozenService.Greeting`, mutate the service, and assert the handler still returns
the stale value. `TestPointerReceiverMethodValueSeesUpdates` is the fix: bind
`(*LiveService).Greeting`, mutate through the pointer, and assert the handler
returns the new value. The gate passes because both tests assert the *correct*
documented behavior of each receiver kind — the "bug" is choosing the wrong kind
for your intent, which the two tests make impossible to confuse.

Create `router_test.go`:

```go
package router

import "testing"

func TestValueReceiverMethodValueFreezesReceiver(t *testing.T) {
	t.Parallel()

	svc := FrozenService{Message: "v1"}

	var r Router
	r.Register(svc.Greeting) // binds a COPY of svc at this moment

	svc.Message = "v2" // mutate after registration

	if got := r.Serve(); got != "v1" {
		t.Fatalf("frozen handler served %q; a value-receiver method value freezes v1", got)
	}
}

func TestPointerReceiverMethodValueSeesUpdates(t *testing.T) {
	t.Parallel()

	svc := &LiveService{Message: "v1"}

	var r Router
	r.Register(svc.Greeting) // binds the POINTER svc

	svc.Message = "v2" // mutate after registration

	if got := r.Serve(); got != "v2" {
		t.Fatalf("live handler served %q; a pointer-receiver method value should see v2", got)
	}
}

func TestMethodExpressionTakesReceiverAtCallTime(t *testing.T) {
	t.Parallel()

	expr := (*LiveService).Greeting // func(*LiveService) string

	svc := &LiveService{Message: "hello"}
	if got := expr(svc); got != "hello" {
		t.Fatalf("method expression returned %q, want hello", got)
	}
	svc.Message = "changed"
	if got := expr(svc); got != "changed" {
		t.Fatalf("method expression returned %q, want changed", got)
	}
}
```

## Review

The two capture tests are the lesson: a value-receiver method value froze `v1` at
bind time, and a pointer-receiver method value tracked the service to `v2`. If you
registered a callback expecting it to observe later config changes and it did not,
this is almost always why — the handler was bound from a value receiver and is
serving a snapshot. The fix is a pointer receiver (or an explicit closure over the
pointer). The method-expression test rounds out the mechanism: `(*LiveService).Greeting`
defers the receiver to call time entirely, which is why higher-order wiring often
prefers method expressions — the copy-vs-pointer decision is right there in the
`func(*LiveService) string` type.

## Resources

- [Go Spec: Method values](https://go.dev/ref/spec#Method_values) — how a method value binds and copies (or captures) its receiver.
- [Go Spec: Method expressions](https://go.dev/ref/spec#Method_expressions) — turning a method into a func with an explicit receiver parameter.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — receiver semantics behind these captures.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-nil-receiver-noop-methods.md](06-nil-receiver-noop-methods.md) | Next: [08-interface-method-set-repository.md](08-interface-method-set-repository.md)
