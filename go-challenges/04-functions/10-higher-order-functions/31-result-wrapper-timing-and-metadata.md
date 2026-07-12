# Exercise 31: Execution Timing and Metadata Wrapper Decorator

**Nivel: Intermedio** — validacion rapida (un test corto).

Wrapping every operation call site with "start a timer, run it, stop the
timer, attach some tags, log it" is exactly the kind of boilerplate a
higher-order function should absorb once. `Instrument` is a generic
factory: give it an operation, a clock, and some metadata, and it hands
back a decorator that returns a `Result[T]` carrying the value, the
error, the elapsed duration, and a defensively-copied metadata map — with
nothing about timing or tagging leaking into the operation itself.

## What you'll build

```text
instrument/                  independent module: example.com/instrument
  go.mod                     go 1.24
  instrument.go                type Result[T], Clock; func Instrument[T]
  instrument_test.go           exact duration, defensive metadata copy, error pass-through
  cmd/demo/
    main.go                  instruments a fake fetch with a fixed-sequence clock
```

- Files: `instrument.go`, `instrument_test.go`, `cmd/demo/main.go`.
- Implement: `Result[T any] struct{ Value T; Err error; Duration time.Duration; Metadata map[string]string }`, `Clock func() time.Time`, and `Instrument[T any](op func() (T, error), now Clock, meta map[string]string) func() Result[T]`.
- Test: the recorded `Duration` matches the exact difference between two fake clock reads; mutating the original `meta` map after `Instrument` returns, or mutating one call's `Result.Metadata`, never affects any other call's recorded metadata; a failing `op` still returns its error and metadata in the `Result`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Two copies, at two different times, for two different reasons

`Instrument` takes a defensive copy of `meta` twice, and each copy closes
a different gap. The first copy — `snapshot`, taken once when `Instrument`
itself is called — protects against the caller mutating the original
`meta` map *after* building the decorator but before ever calling it; if
`Instrument` only closed over `meta` directly, a later `meta["region"] =
"eu-west-1"` would silently rewrite what every future call records, since
a map is a reference type and the closure would see the same underlying
data. The second copy — inside the returned closure, taken fresh on every
call — protects each call's `Result` from every other call's: two
`Result` values sharing one `Metadata` map would mean mutating one
result's metadata (a common thing to do right before logging it)
corrupts a different call's already-returned result too. Skipping either
copy reintroduces a real aliasing bug; skipping the second is the
subtler one, since it only shows up once a decorator is actually called
more than once.

The generic type parameter `T` is what lets one `Instrument` implementation
wrap an operation returning a string, an int, a struct — anything — while
`Result[T]` stays a single, reusable type instead of a bespoke wrapper
struct per operation shape.

Create `instrument.go`:

```go
package instrument

import "time"

// Result wraps an operation's outcome with the timing and contextual
// metadata Instrument recorded around it.
type Result[T any] struct {
	Value    T
	Err      error
	Duration time.Duration
	Metadata map[string]string
}

// Clock returns the current time. Production code passes time.Now;
// tests pass a fake sequence so the recorded Duration is deterministic.
type Clock func() time.Time

// Instrument decorates op so that every call is wrapped in a Result
// carrying its value or error, how long it took according to now, and a
// defensive copy of meta — mutating the meta map after Instrument
// returns, or mutating a returned Result's Metadata, never affects any
// other call's recorded metadata.
func Instrument[T any](op func() (T, error), now Clock, meta map[string]string) func() Result[T] {
	// Copied once, here, at decoration time — not inside the returned
	// closure — so a caller mutating meta after Instrument returns can
	// never reach into an already-built decorator's recorded metadata.
	snapshot := make(map[string]string, len(meta))
	for k, v := range meta {
		snapshot[k] = v
	}

	return func() Result[T] {
		start := now()
		val, err := op()
		end := now()

		// A fresh copy per call: two calls to the same decorator must
		// not share one Metadata map, or mutating one Result's map would
		// corrupt every other call's recorded metadata too.
		md := make(map[string]string, len(snapshot))
		for k, v := range snapshot {
			md[k] = v
		}

		return Result[T]{
			Value:    val,
			Err:      err,
			Duration: end.Sub(start),
			Metadata: md,
		}
	}
}
```

### The runnable demo

The demo instruments a fake user-fetch operation with a fixed two-value
clock sequence — 0ms, then 42ms — so the recorded duration is an exact,
predictable number instead of whatever wall-clock time the demo happens
to take.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/instrument"
)

func main() {
	// A fake clock returning a fixed sequence: start, then 42ms later.
	times := []time.Time{
		time.Unix(0, 0),
		time.Unix(0, 0).Add(42 * time.Millisecond),
	}
	call := 0
	clock := func() time.Time {
		t := times[call]
		call++
		return t
	}

	op := func() (string, error) {
		return "fetched-user-42", nil
	}

	instrumented := instrument.Instrument(op, clock, map[string]string{
		"operation": "fetch-user",
		"region":    "us-east-1",
	})

	result := instrumented()
	fmt.Printf("value=%q err=%v duration=%s metadata=%v\n",
		result.Value, result.Err, result.Duration, result.Metadata)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
value="fetched-user-42" err=<nil> duration=42ms metadata=map[operation:fetch-user region:us-east-1]
```

`fmt`'s `%v` verb prints map entries in sorted key order, which is why
this output — and the assertions that rely on comparing formatted output
in tests elsewhere in this curriculum — is deterministic even though
Go's own map iteration order is not.

### Tests

`TestInstrumentRecordsExactDuration` uses a two-value fake clock to
assert the recorded duration is exactly the difference between the two
reads, not merely non-zero. `TestInstrumentCopiesMetadataDefensively` is
the test that exercises both copies: it mutates the original `meta` map
after building the decorator and confirms the recorded result is
unaffected, then mutates a returned `Result.Metadata` and confirms a
second call to the same decorator gets its own untouched copy.
`TestInstrumentPassesThroughError` confirms a failing operation still
produces a fully-formed `Result` — the zero value for `T`, the error via
`errors.Is`, and the metadata intact — rather than some special-cased
error path that drops the timing or tagging.

Create `instrument_test.go`:

```go
package instrument

import (
	"errors"
	"testing"
	"time"
)

// sequenceClock returns each time in seq in order, one per call.
func sequenceClock(seq ...time.Time) Clock {
	i := 0
	return func() time.Time {
		t := seq[i]
		i++
		return t
	}
}

func TestInstrumentRecordsExactDuration(t *testing.T) {
	t.Parallel()

	start := time.Unix(0, 0)
	end := start.Add(150 * time.Millisecond)
	clock := sequenceClock(start, end)

	op := func() (int, error) { return 7, nil }
	instrumented := Instrument(op, clock, nil)

	result := instrumented()
	if result.Value != 7 {
		t.Fatalf("Value = %d, want 7", result.Value)
	}
	if result.Err != nil {
		t.Fatalf("Err = %v, want nil", result.Err)
	}
	if result.Duration != 150*time.Millisecond {
		t.Fatalf("Duration = %s, want 150ms", result.Duration)
	}
}

func TestInstrumentCopiesMetadataDefensively(t *testing.T) {
	t.Parallel()

	t0 := time.Unix(0, 0)
	clock := sequenceClock(t0, t0, t0, t0) // two calls to instrumented(), two clock reads each
	meta := map[string]string{"region": "us-east-1"}
	instrumented := Instrument(func() (string, error) { return "ok", nil }, clock, meta)

	// Mutate the original map after building the decorator.
	meta["region"] = "eu-west-1"
	meta["extra"] = "should-not-appear"

	result := instrumented()
	if result.Metadata["region"] != "us-east-1" {
		t.Fatalf("Metadata[region] = %q, want %q (must not see later mutation)", result.Metadata["region"], "us-east-1")
	}
	if _, ok := result.Metadata["extra"]; ok {
		t.Fatal("Metadata contains a key added to the original map after Instrument was called")
	}

	// Mutate the returned Result's Metadata and confirm a second call is unaffected.
	result.Metadata["region"] = "mutated"
	second := instrumented()
	if second.Metadata["region"] != "us-east-1" {
		t.Fatalf("second call's Metadata[region] = %q, want %q (must not share the first result's map)", second.Metadata["region"], "us-east-1")
	}
}

func TestInstrumentPassesThroughError(t *testing.T) {
	t.Parallel()

	clock := sequenceClock(time.Unix(0, 0), time.Unix(0, 0))
	errBoom := errors.New("upstream unavailable")
	op := func() (int, error) { return 0, errBoom }
	instrumented := Instrument(op, clock, map[string]string{"operation": "fetch"})

	result := instrumented()
	if !errors.Is(result.Err, errBoom) {
		t.Fatalf("Err = %v, want errBoom", result.Err)
	}
	if result.Value != 0 {
		t.Fatalf("Value = %d, want zero value", result.Value)
	}
	if result.Metadata["operation"] != "fetch" {
		t.Fatalf("Metadata[operation] = %q, want %q", result.Metadata["operation"], "fetch")
	}
}
```

## Review

`Instrument` is correct because it treats `meta` as data to snapshot, not
a reference to hold onto — every point where that map's contents could
otherwise leak across time (caller mutation after construction) or across
calls (one `Result` sharing a map with another) gets its own copy.
Skipping the per-call copy is the bug that is easiest to miss in review,
because a test that only calls the instrumented function once would
never catch it — `TestInstrumentCopiesMetadataDefensively` calls it
twice specifically to force that gap into the open. The clock injection
follows the same pattern this curriculum uses everywhere timing matters:
production passes `time.Now`, tests pass a fixed sequence, and the
assertion becomes an exact equality instead of a "duration is roughly
right" guess.

## Resources

- [time package](https://pkg.go.dev/time) — `Time.Sub`, `Duration`, the arithmetic behind the recorded timing.
- [Go spec: Type parameter declarations](https://go.dev/ref/spec#Type_parameter_declarations) — the generic `Result[T]` and `Instrument[T]` shape.
- [maps.Clone](https://pkg.go.dev/maps#Clone) — a standard-library shallow-copy helper for the same defensive-copy pattern this exercise writes out by hand.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-consensus-aggregate-with-quorum.md](30-consensus-aggregate-with-quorum.md) | Next: [32-time-series-percentile-reducer.md](32-time-series-percentile-reducer.md)
