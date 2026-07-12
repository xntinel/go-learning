# Exercise 32: Conditional Post-Processor Execution on Success

Some pipelines need a follow-up step that only makes sense once the main
handler has actually succeeded — publishing a downstream notification,
incrementing a counter, whatever the case may be. Calling that follow-up
next to every successful return statement scales badly the moment a second
success exit is added. This exercise builds a `HandleEvent` that runs a
`postProcess` callback from a single deferred closure keyed on the named
`handled bool` result, so "only on success" is enforced in exactly one
place.

**Nivel: Intermedio** — validacion rapida (dos pruebas: exito y fallo).

## What you'll build

```text
eventpost/                  independent module: example.com/eventpost
  go.mod
  eventpost.go                Event; HandleEvent (named handled, deferred conditional post-process)
  cmd/demo/
    main.go                  runnable demo: a successful event and a failing one
  eventpost_test.go           post-process runs once on success, never runs on failure
```

- Files: `eventpost.go`, `cmd/demo/main.go`, `eventpost_test.go`.
- Implement: `HandleEvent(ev Event, handle func(Event) error, postProcess func(Event)) (handled bool, err error)` whose deferred closure calls `postProcess(ev)` only when the named `handled` is true.
- Test: a successful `handle` results in `postProcess` being called exactly once with the right event; a failing `handle` never calls `postProcess` at all.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### One post-process, gated on the named result

```go
defer func() {
    if handled {
        postProcess(ev)
    }
}()

if err = handle(ev); err != nil {
    return false, err
}
handled = true
return true, nil
```

`handled` starts false and only becomes true on the one line that follows a
successful `handle` call. The deferred closure runs after either return
statement has copied its boolean into the named `handled` result, so it
never has to guess why `HandleEvent` is returning — it just checks the flag
the function's own logic already set. This is the same shape as the
committed-transaction and released-buffer exercises earlier in this
chapter: a boolean named result exists purely so a single deferred closure
can answer "did the thing that matters happen?" without duplicating that
question at every call site of `postProcess`.

Create `eventpost.go`:

```go
package eventpost

// Event is one unit of work delivered to a handler.
type Event struct {
	ID   string
	Body string
}

// HandleEvent runs handle against ev and, only if handle succeeds, runs
// postProcess against the same event.
//
// handled is a named result: a single deferred closure checks it once
// HandleEvent is about to return and runs postProcess exactly when handled
// is true. This keeps the "only on success" rule in one place regardless of
// which return statement produced the result, instead of duplicating a call
// to postProcess next to every successful exit.
func HandleEvent(ev Event, handle func(Event) error, postProcess func(Event)) (handled bool, err error) {
	defer func() {
		if handled {
			postProcess(ev)
		}
	}()

	if err = handle(ev); err != nil {
		return false, err
	}
	handled = true
	return true, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/eventpost"
)

func main() {
	postProcess := func(ev eventpost.Event) {
		fmt.Println("post-processed:", ev.ID)
	}

	handled, err := eventpost.HandleEvent(
		eventpost.Event{ID: "e1", Body: "ok"},
		func(ev eventpost.Event) error { return nil },
		postProcess,
	)
	fmt.Printf("success case: handled=%v err=%v\n", handled, err)

	handled, err = eventpost.HandleEvent(
		eventpost.Event{ID: "e2", Body: "bad"},
		func(ev eventpost.Event) error { return errors.New("handler failed") },
		postProcess,
	)
	fmt.Printf("failure case: handled=%v err=%v\n", handled, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
post-processed: e1
success case: handled=true err=<nil>
failure case: handled=false err=handler failed
```

### Tests

Create `eventpost_test.go`:

```go
package eventpost

import (
	"errors"
	"testing"
)

func TestHandleEventRunsPostProcessOnSuccess(t *testing.T) {
	t.Parallel()

	var postProcessed []string
	handled, err := HandleEvent(
		Event{ID: "e1"},
		func(Event) error { return nil },
		func(ev Event) { postProcessed = append(postProcessed, ev.ID) },
	)
	if err != nil {
		t.Fatalf("HandleEvent: unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("handled = false, want true on success")
	}
	if len(postProcessed) != 1 || postProcessed[0] != "e1" {
		t.Fatalf("postProcessed = %v, want [e1]", postProcessed)
	}
}

func TestHandleEventSkipsPostProcessOnFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	postProcessCalled := false
	handled, err := HandleEvent(
		Event{ID: "e2"},
		func(Event) error { return wantErr },
		func(Event) { postProcessCalled = true },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if handled {
		t.Fatal("handled = true, want false on failure")
	}
	if postProcessCalled {
		t.Fatal("postProcess was called after a failed handler, want it skipped")
	}
}
```

## Review

`HandleEvent` is correct when `postProcess` runs exactly once per successful
event and never at all for a failed one — the two properties the tests
check directly by recording calls rather than trusting side effects. The
named `handled` result is what centralizes the decision: it is the only
piece of state the deferred closure consults, so adding a second success
exit later (an early-return "already handled, skip" branch, say) only needs
to set `handled = true` on that new path to inherit the same post-processing
behavior. The mistake to avoid is calling `postProcess(ev)` directly at the
one success return statement instead of through the defer — it produces
identical behavior today, but the moment a second success path is added, one
of the two copies is guaranteed to be forgotten.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Effective Go: Named result parameters](https://go.dev/doc/effective_go#named-results)
- [Go Spec: Function types](https://go.dev/ref/spec#Function_types)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-multi-file-handle-partial-close-unwind.md](31-multi-file-handle-partial-close-unwind.md) | Next: [33-environment-variable-temporary-set-restore.md](33-environment-variable-temporary-set-restore.md)
