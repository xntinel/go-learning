# Exercise 16: Fan-Out Log Shipper: One Record, Independent Sinks

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Fluentd and Vector both sit in front of a fan-out: one parsed log record comes
in, and it has to go out to several places at once -- an S3 archive for
compliance retention, a metrics pipeline that counts event types, and a
redaction stage that strips PII before the record reaches a third party.
Every earlier exercise in this lesson dealt with a single producer handing a
slice to a single consumer, where the ownership question has one answer: does
the consumer own it or not? A fan-out changes the shape of the question,
because now there are *several* consumers of the same record, and each one's
answer can be different independently of the others.

The bug this produces is specific to that multi-consumer shape and does not
show up in a single-handoff design at all: if the router passes the identical
`[]byte` to every sink, and one sink mutates it in place -- a redaction sink
zeroing out a token field is the realistic case -- then every other sink that
still holds that slice, including ones that already ran and cached what they
saw, now observes the redaction too. The archive sink silently loses the very
data it was supposed to preserve. This is not a data race in the `-race`
detector's sense if the sinks run sequentially; it is a plain aliasing bug,
and it is worse than a crash because nothing about it looks wrong at the call
site. Each sink's code is correct in isolation. The bug lives entirely in
what the router handed out.

This module builds `fanout`, a package with a `Router` that gives every sink
its own clone of the record before invoking it, so no sink can ever observe
another sink's mutation, in either direction and regardless of call order.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
fanout/                   module example.com/fanout
  go.mod                  go 1.24
  router.go                Sink, Router; NewRouter, Route; two sentinel errors
  router_test.go          delivery table, failure propagation, the routeShared contrast,
                          both aliasing directions, concurrency, ExampleRouter_Route
```

- Files: `router.go`, `router_test.go`.
- Implement: `NewRouter(source string) (*Router, error)` rejecting an empty source with `ErrEmptySource`; `type Sink func([]byte) error`; `(*Router).Route(record []byte, sinks ...Sink) error` returning `ErrNoSinks` for zero sinks, cloning `record` with `bytes.Clone` once per sink before invoking it, calling every sink even after an earlier one fails, and returning `errors.Join` of every wrapped failure.
- Test: ordinary, empty, and nil records; `ErrNoSinks` and `ErrEmptySource` via `errors.Is`; a failing sink not blocking the rest, with the joined error still satisfying `errors.Is` against the sink's own error; the `routeShared` contrast proving cross-sink corruption; both aliasing directions -- a sink cannot see another sink's mutation, and the caller mutating its own slice after `Route` returns cannot reach any sink; `Router` is safe for concurrent use; and `ExampleRouter_Route` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fanout
cd ~/go-exercises/fanout
go mod init example.com/fanout
go mod edit -go=1.24
```

### Cloning on the way in, not on the way out

Every prior lesson in this file taught cloning at a boundary a slice crosses
once: a repository returning its internal state, a scanner token being
retained, a buffer being handed to an async sink before it gets reused. A
fan-out crosses the same boundary *n* times from a single starting value, and
each crossing needs its own independent copy, because the sinks do not
coordinate with each other and are not supposed to have to. The naive router
looks like this:

```go
func routeShared(record []byte, sinks ...Sink) error {
    for _, sink := range sinks {
        if err := sink(record); err != nil { // every sink gets the same slice
            ...
        }
    }
    ...
}
```

This compiles, passes a smoke test with one sink, and then corrupts data the
day a second sink that mutates in place -- redaction, normalization,
in-place trimming -- gets added to the pipeline. The fix is to clone once per
sink, right before that sink runs:

```go
for i, sink := range sinks {
    clone := bytes.Clone(record)   // this sink's own backing array
    if err := sink(clone); err != nil {
        ...
    }
}
```

`bytes.Clone` allocates a fresh backing array holding a copy of the input, so
after this loop there are as many independent backing arrays as there are
sinks, plus the caller's own untouched `record`. No sink can observe any
other sink's writes, and none of them can observe a mutation the caller makes
to `record` after `Route` has returned, because every clone was already taken
before the first sink ran.

Create `router.go`:

```go
// Package fanout delivers one parsed log record to several independent
// sinks, the shape of Fluentd or Vector routing a record to an archive
// sink, a metrics sink, and a redaction sink at once.
//
// Its one job is an ownership-boundary rule the multi-consumer case makes
// easy to get wrong: every sink must receive its own copy of the record. If
// two sinks share the same backing array and one of them redacts a field in
// place, every other sink -- including one that already ran, if it retains
// the slice past the call -- observes the redaction too.
package fanout

import (
	"bytes"
	"errors"
	"fmt"
)

// ErrEmptySource means NewRouter was given an empty source label.
var ErrEmptySource = errors.New("fanout: source must not be empty")

// ErrNoSinks means Route was called with zero sinks.
var ErrNoSinks = errors.New("fanout: at least one sink is required")

// Sink receives one independent copy of a record. It returns an error if
// delivery fails; Router does not retry.
type Sink func([]byte) error

// Router delivers one record to many sinks, cloning it once per sink so that
// no two sinks ever observe the same backing array.
//
// A Router is immutable after construction and is safe for concurrent use:
// concurrent calls to Route share no mutable state, since each call clones
// fresh from its own record argument.
type Router struct {
	source string
}

// NewRouter returns a Router that labels its errors with source, the name of
// the log stream it fans out (for example a service name). It returns
// ErrEmptySource if source is empty.
func NewRouter(source string) (*Router, error) {
	if source == "" {
		return nil, ErrEmptySource
	}
	return &Router{source: source}, nil
}

// Source reports the label this Router attaches to its errors.
func (r *Router) Source() string { return r.source }

// Route delivers record to every sink in sinks, giving each one its own
// clone made with bytes.Clone. Sinks run in the order given; a sink that
// mutates its argument in place -- a redaction sink is the obvious case --
// affects only its own clone, never record itself and never any other
// sink's copy. record may be reused or mutated by the caller as soon as
// Route returns: every clone was taken before the first sink ran.
//
// Route calls every sink even if an earlier one fails, so one broken sink
// (say, a metrics endpoint that is down) never prevents delivery to the
// others. It returns ErrNoSinks if sinks is empty, or a joined error
// wrapping the index and underlying error of every sink that failed.
func (r *Router) Route(record []byte, sinks ...Sink) error {
	if len(sinks) == 0 {
		return ErrNoSinks
	}

	var errs []error
	for i, sink := range sinks {
		clone := bytes.Clone(record)
		if err := sink(clone); err != nil {
			errs = append(errs, fmt.Errorf("%s: sink %d: %w", r.source, i, err))
		}
	}
	return errors.Join(errs...)
}
```

### Using it

Construct one `Router` per log source at startup -- `NewRouter` only
validates a label used in error messages, so a single value is cheap to
share -- then call `Route` per record with whatever sinks that source
currently fans out to. Because `Router` holds no mutable state after
construction, the same value can be called from many goroutines at once
without a mutex, which `TestRouterIsSafeForConcurrentUse` holds it to.

Two aliasing guarantees cross the package boundary, both documented on
`Route` and both pinned by name in the tests. First, no sink can observe
another sink's mutation, because each receives its own `bytes.Clone`.
Second, the caller is free to mutate the `record` slice it passed in the
moment `Route` returns, because every clone was already taken before any
sink ran -- a later mutation of the caller's slice can never reach a sink
that has already been called. Neither of these is true of the `routeShared`
helper the tests contrast against, and that is exactly the point.

The module's runnable demonstration is `ExampleRouter_Route`: `go test` runs
it and compares its stdout against the `// Output:` comment, so the usage
shown below cannot drift away from the code.

```go
func ExampleRouter_Route() {
	r, err := NewRouter("web-service")
	if err != nil {
		panic(err)
	}

	record := []byte(`{"user":"alice","token":"abc123"}`)

	var archived, redacted string
	archive := func(b []byte) error {
		archived = string(b)
		return nil
	}
	redact := func(b []byte) error {
		copy(b, bytes.Repeat([]byte{'*'}, len(b)))
		redacted = string(b)
		return nil
	}

	if err := r.Route(record, archive, redact); err != nil {
		panic(err)
	}
	fmt.Println("archive saw:", archived)
	fmt.Println("redact saw: ", redacted)
	fmt.Println("original record unchanged:", string(record))

	// Output:
	// archive saw: {"user":"alice","token":"abc123"}
	// redact saw:  *********************************
	// original record unchanged: {"user":"alice","token":"abc123"}
}
```

`archive` runs first and captures the original JSON; `redact` runs second and
overwrites its own clone with asterisks. Because each sink owns a separate
backing array, `archive`'s captured string is untouched by `redact`'s
mutation even though `redact` ran later, and the caller's own `record`
variable -- printed last -- was never touched by either sink.

### Tests

`TestRoute` is the delivery table across an ordinary, an empty, and a nil
record, checking that the sink actually receives what was sent.
`TestRouteRejectsZeroSinks` and `TestNewRouterRejectsEmptySource` pin the two
sentinel errors with `errors.Is`. `TestRouteCallsEverySinkEvenAfterAFailure`
checks that one failing sink does not stop delivery to the rest, and that the
joined error returned still satisfies `errors.Is` against the sink's own
error, which is what lets a caller check for a specific failure inside a
fan-out without string matching.

`TestRouteClonesPerSinkButRouteSharedCorrupts` is the heart of the module.
`routeShared` is unexported and unreachable from the package API; it exists
so the test can run the same redact-then-capture sequence through both
functions and show the capture sink observing the untouched original through
`Route` and the redacted bytes through `routeShared` -- the exact corruption
a shared backing array causes.
`TestRouteSinksAreIndependentOfCallerMutationAfterReturn` pins the second
aliasing direction: a caller mutating its own slice after `Route` returns
must never reach a sink that has already run.

Create `router_test.go`:

```go
package fanout

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestNewRouterRejectsEmptySource(t *testing.T) {
	t.Parallel()

	if _, err := NewRouter(""); !errors.Is(err, ErrEmptySource) {
		t.Fatalf("NewRouter(\"\") err = %v, want ErrEmptySource", err)
	}
}

func TestRouteRejectsZeroSinks(t *testing.T) {
	t.Parallel()

	r, err := NewRouter("web")
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if err := r.Route([]byte("x")); !errors.Is(err, ErrNoSinks) {
		t.Fatalf("Route with no sinks err = %v, want ErrNoSinks", err)
	}
}

func TestRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		record []byte
	}{
		{name: "ordinary record", record: []byte("user=alice")},
		{name: "empty record", record: []byte{}},
		{name: "nil record", record: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r, err := NewRouter("web")
			if err != nil {
				t.Fatalf("NewRouter: %v", err)
			}
			var seen string
			seenFlag := false
			sink := func(b []byte) error {
				seen = string(b)
				seenFlag = true
				return nil
			}
			if err := r.Route(tc.record, sink); err != nil {
				t.Fatalf("Route: %v", err)
			}
			if !seenFlag {
				t.Fatal("sink was never called")
			}
			if seen != string(tc.record) {
				t.Fatalf("sink saw %q, want %q", seen, tc.record)
			}
		})
	}
}

// errBoom is a sentinel a sink can fail with, so the test can assert Route's
// joined error still satisfies errors.Is against it.
var errBoom = errors.New("sink boom")

func TestRouteCallsEverySinkEvenAfterAFailure(t *testing.T) {
	t.Parallel()

	r, err := NewRouter("web")
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	var calls []int
	failing := func(b []byte) error {
		calls = append(calls, 0)
		return errBoom
	}
	ok := func(b []byte) error {
		calls = append(calls, 1)
		return nil
	}

	err = r.Route([]byte("x"), failing, ok)
	if !errors.Is(err, errBoom) {
		t.Fatalf("Route err = %v, want it to wrap errBoom", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want both sinks invoked despite the first failing", calls)
	}
}

// routeShared is the fan-out that a first draft of this router tends to
// ship: it hands the identical record slice to every sink instead of
// cloning. It is unexported and unreachable from the package API; it exists
// only so the tests can observe the cross-sink corruption it causes.
func routeShared(record []byte, sinks ...Sink) error {
	var errs []error
	for i, sink := range sinks {
		if err := sink(record); err != nil { // BUG: every sink gets the same backing array
			errs = append(errs, fmt.Errorf("sink %d: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

// TestRouteClonesPerSinkButRouteSharedCorrupts is the heart of this module.
// A redact sink mutates its argument in place -- the realistic shape of a
// PII scrubber -- and runs before a capture sink that just records what it
// was given. Through Router.Route, the capture sink sees the untouched
// original, because it received its own clone. Through routeShared, it sees
// the redaction, because both sinks shared one backing array.
func TestRouteClonesPerSinkButRouteSharedCorrupts(t *testing.T) {
	t.Parallel()

	original := []byte("secret=1234")
	redact := func(b []byte) error {
		for i := range b {
			b[i] = '*'
		}
		return nil
	}

	var captured string
	capture := func(b []byte) error {
		captured = string(b)
		return nil
	}

	r, err := NewRouter("web")
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	record := bytes.Clone(original)
	if err := r.Route(record, redact, capture); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if captured != string(original) {
		t.Fatalf("Route: capture sink saw %q, want the untouched %q", captured, original)
	}
	if string(record) != string(original) {
		t.Fatalf("Route: the caller's own record was mutated to %q", record)
	}

	captured = ""
	sharedRecord := bytes.Clone(original)
	if err := routeShared(sharedRecord, redact, capture); err != nil {
		t.Fatalf("routeShared: %v", err)
	}
	wantRedacted := strings.Repeat("*", len(original))
	if captured != wantRedacted {
		t.Fatalf("routeShared: capture sink saw %q, want the redacted %q -- the two sinks shared one backing array", captured, wantRedacted)
	}
}

// TestRouteSinksAreIndependentOfCallerMutationAfterReturn pins the other
// half of the aliasing contract: once Route has returned, the caller is
// free to mutate the record it passed in without disturbing what any sink
// already received, because every clone was taken before the first sink ran.
func TestRouteSinksAreIndependentOfCallerMutationAfterReturn(t *testing.T) {
	t.Parallel()

	record := []byte("hello")
	var gotA, gotB string
	sinkA := func(b []byte) error { gotA = string(b); return nil }
	sinkB := func(b []byte) error { gotB = string(b); return nil }

	r, err := NewRouter("web")
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if err := r.Route(record, sinkA, sinkB); err != nil {
		t.Fatalf("Route: %v", err)
	}

	record[0] = 'X' // caller mutates its own slice after Route has returned

	if gotA != "hello" || gotB != "hello" {
		t.Fatalf("a later mutation of the caller's record reached a sink: gotA=%q gotB=%q", gotA, gotB)
	}
}

func TestRouterIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	r, err := NewRouter("web")
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			record := []byte(fmt.Sprintf("record-%d", i))
			var got string
			sink := func(b []byte) error {
				got = string(b)
				return nil
			}
			if err := r.Route(record, sink); err != nil {
				t.Errorf("goroutine %d: Route: %v", i, err)
				return
			}
			want := fmt.Sprintf("record-%d", i)
			if got != want {
				t.Errorf("goroutine %d: sink saw %q, want %q", i, got, want)
			}
		}(i)
	}
	wg.Wait()
}

// ExampleRouter_Route is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleRouter_Route() {
	r, err := NewRouter("web-service")
	if err != nil {
		panic(err)
	}

	record := []byte(`{"user":"alice","token":"abc123"}`)

	var archived, redacted string
	archive := func(b []byte) error {
		archived = string(b)
		return nil
	}
	redact := func(b []byte) error {
		copy(b, bytes.Repeat([]byte{'*'}, len(b)))
		redacted = string(b)
		return nil
	}

	if err := r.Route(record, archive, redact); err != nil {
		panic(err)
	}
	fmt.Println("archive saw:", archived)
	fmt.Println("redact saw: ", redacted)
	fmt.Println("original record unchanged:", string(record))

	// Output:
	// archive saw: {"user":"alice","token":"abc123"}
	// redact saw:  *********************************
	// original record unchanged: {"user":"alice","token":"abc123"}
}
```

## Review

`Route` is correct when no sink can observe any other sink's mutation and no
sink can observe a mutation the caller makes after the call returns --
`TestRouteClonesPerSinkButRouteSharedCorrupts` and
`TestRouteSinksAreIndependentOfCallerMutationAfterReturn` pin exactly those
two directions. The mechanism is one `bytes.Clone` per sink, taken before
that sink runs, so every sink and the caller each end up on their own
backing array. `NewRouter` rejects an empty source label with
`ErrEmptySource`, `Route` rejects zero sinks with `ErrNoSinks`, both
checkable with `errors.Is`, and a failing sink never blocks delivery to the
rest -- their errors are joined and still satisfy `errors.Is` against the
underlying sentinel a sink returned. `Router` carries no mutable state after
construction and is therefore safe to share across goroutines.
`ExampleRouter_Route` is the executable documentation: `go test` verifies its
output. Run `go test -count=1 -race ./...`.

## Resources

- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone) — the allocation this module relies on to give every sink an independent backing array.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combines every failing sink's error while keeping each one discoverable with `errors.Is`.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — how a caller checks for a specific sink failure inside a joined, wrapped error.
- [Vector: Sinks](https://vector.dev/docs/reference/configuration/sinks/) — the real multi-sink routing shape this module models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-frame-inspector-eof-vs-unexpected-eof.md](15-frame-inspector-eof-vs-unexpected-eof.md) | Next: [17-loadbalancer-rotate-in-place.md](17-loadbalancer-rotate-in-place.md)
