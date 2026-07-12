# Exercise 5: Token Expiry / Event Dedup: time.Time Equal vs ==

Two of the most common backend tasks — checking whether a token/window has expired,
and de-duplicating events by timestamp — both compare `time.Time` values, and both
are wrong the moment you reach for `==`. A `time.Time` carries a wall reading, an
optional monotonic reading, and a `*Location`, and `==` compares all three. This
exercise builds an expiry check and an event de-dup that compare instants correctly
with `.Equal`, `.Before`, and a normalization step before keying.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
eventdedup/                 independent module: example.com/eventdedup
  go.mod                    go 1.26
  eventdedup.go             Normalize, Window.Expired (instant compare), Dedup.Seen (normalized key)
  cmd/
    demo/
      main.go               runnable demo: == vs Equal, dedup across zones, expiry
  eventdedup_test.go        docs UTC-vs-Beijing example; monotonic reflexivity; normalized map key
```

- Files: `eventdedup.go`, `cmd/demo/main.go`, `eventdedup_test.go`.
- Implement: `Normalize(t)` = `t.Round(0).UTC()`; a `Window` whose `Expired` uses `.Before`; a `Dedup` keyed on the normalized instant.
- Test: same instant in UTC vs Beijing is not `==` but is `.Equal`; a monotonic reading breaks `==` but not `.Equal`; normalization makes two representations collapse to one map key.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/05-struct-comparison-and-equality/05-time-equal-monotonic-location/cmd/demo
cd go-solutions/07-structs-and-methods/05-struct-comparison-and-equality/05-time-equal-monotonic-location
```

### Why `==` is the wrong instant comparison

A `time.Time` is a struct, and `==` compares its fields. Two of those fields are not
the instant: the `*Location` (a zone) and the monotonic-clock reading attached by
`time.Now()`. So two values that denote the *same instant* can be unequal under
`==`:

- **Different zone.** `time.Date(2000, 2, 1, 12, 30, 0, 0, time.UTC)` and the same
  moment expressed in a Beijing zone via `.In(beijing)` are the identical instant,
  but their `Location` fields differ, so `==` is false. `.Equal` compares only the
  instant, so it is true. This is the example from the `time` package docs.
- **Monotonic reading.** `time.Now()` carries a monotonic reading; a wall-clock-only
  copy of the same moment (`now.Round(0)`) does not. Their internal representations
  differ, so `now == now.Round(0)` is false — a `time.Time` that is "not equal to
  itself". `.Equal` is true because the instant is the same.

The rules that follow:

1. To compare instants, use `t1.Equal(t2)`; to order them, use `t1.Before(t2)` /
   `t1.After(t2)`. All three compare the instant and ignore `Location`. That is why
   `Window.Expired` below uses `!now.Before(w.expires)` rather than any `==`.
2. Before using a `time.Time` as a map key or a database key, **normalize** it:
   `.Round(0)` strips the monotonic reading (it is meaningless outside this process
   and would otherwise make every fresh reading a distinct key), and `.UTC()`
   canonicalizes the zone so two representations of one instant become one key.
   `Normalize` does both, and `Dedup` applies it on every insert and lookup.

Create `eventdedup.go`:

```go
package eventdedup

import "time"

// Normalize returns t stripped of its monotonic reading and moved to UTC, so it
// is safe to use as a map or database key. Round(0) drops the monotonic reading;
// UTC canonicalizes the location.
func Normalize(t time.Time) time.Time {
	return t.Round(0).UTC()
}

// Window is a time window with an expiry instant.
type Window struct {
	expires time.Time
}

// NewWindow returns a window that expires ttl after start.
func NewWindow(start time.Time, ttl time.Duration) Window {
	return Window{expires: start.Add(ttl)}
}

// Expired reports whether the window has expired at instant now. It compares
// instants with Before, never ==, so a differing zone or monotonic reading on
// now does not corrupt the answer.
func (w Window) Expired(now time.Time) bool {
	return !now.Before(w.expires)
}

// Dedup records event instants and reports duplicates. It keys on the normalized
// instant, so the same moment in a different zone or with a monotonic reading is
// recognized as a duplicate.
type Dedup struct {
	seen map[time.Time]struct{}
}

// NewDedup returns an empty Dedup.
func NewDedup() *Dedup { return &Dedup{seen: make(map[time.Time]struct{})} }

// Seen records t and reports whether its instant had already been seen.
func (d *Dedup) Seen(t time.Time) (dup bool) {
	k := Normalize(t)
	if _, ok := d.seen[k]; ok {
		return true
	}
	d.seen[k] = struct{}{}
	return false
}
```

### The runnable demo

The demo reproduces the docs example (`==` false, `.Equal` true for the same instant
in two zones), shows the de-dup recognizing that same instant across zones as a
duplicate, and checks two expiry points.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/eventdedup"
)

func main() {
	t1 := time.Date(2000, 2, 1, 12, 30, 0, 0, time.UTC)
	beijing := time.FixedZone("Beijing Time", 8*60*60)
	t2 := t1.In(beijing)

	fmt.Printf("== : %v\n", t1 == t2)
	fmt.Printf("Equal: %v\n", t1.Equal(t2))

	d := eventdedup.NewDedup()
	fmt.Printf("first seen: %v\n", d.Seen(t1))
	fmt.Printf("same instant other zone: %v\n", d.Seen(t2))

	w := eventdedup.NewWindow(t1, time.Hour)
	fmt.Printf("expired at +30m: %v\n", w.Expired(t1.Add(30*time.Minute)))
	fmt.Printf("expired at +2h: %v\n", w.Expired(t1.Add(2*time.Hour)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
== : false
Equal: true
first seen: false
same instant other zone: true
expired at +30m: false
expired at +2h: true
```

### Tests

`TestDocsExampleUTCvsBeijing` pins the canonical trap: same instant, two zones,
`==` false, `.Equal` true. `TestMonotonicBreaksDoubleEquals` shows a `time.Now()`
value is not `==` to its own `Round(0)` copy while `.Equal` still holds — reflexive
in instant, not in representation. `TestNormalizedMapKeyCollapses` proves that
without normalization two representations of one instant would be distinct map keys,
and that `Dedup` (which normalizes) collapses them. `TestExpiry` checks the window
boundary across a zone change to confirm `Expired` compares instants.

Create `eventdedup_test.go`:

```go
package eventdedup

import (
	"testing"
	"time"
)

func TestDocsExampleUTCvsBeijing(t *testing.T) {
	t.Parallel()

	t1 := time.Date(2000, 2, 1, 12, 30, 0, 0, time.UTC)
	beijing := time.FixedZone("Beijing Time", 8*60*60)
	t2 := t1.In(beijing)

	if t1 == t2 {
		t.Fatal("== should be false: same instant, different Location")
	}
	if !t1.Equal(t2) {
		t.Fatal("Equal should be true: same instant")
	}
}

func TestMonotonicBreaksDoubleEquals(t *testing.T) {
	t.Parallel()

	now := time.Now()    // carries a monotonic reading
	wall := now.Round(0) // monotonic stripped, same instant

	if now == wall {
		t.Fatal("== should be false: monotonic reading differs")
	}
	if !now.Equal(wall) {
		t.Fatal("Equal should be true: same instant")
	}
}

func TestNormalizedMapKeyCollapses(t *testing.T) {
	t.Parallel()

	t1 := time.Date(2000, 2, 1, 12, 30, 0, 0, time.UTC)
	beijing := time.FixedZone("Beijing Time", 8*60*60)
	t2 := t1.In(beijing)

	// Un-normalized, the two representations are distinct map keys.
	raw := map[time.Time]struct{}{t1: {}}
	if _, ok := raw[t2]; ok {
		t.Fatal("precondition: un-normalized keys should NOT collapse")
	}

	// Dedup normalizes, so they are recognized as the same instant.
	d := NewDedup()
	if d.Seen(t1) {
		t.Fatal("first observation should not be a duplicate")
	}
	if !d.Seen(t2) {
		t.Fatal("same instant in another zone should be a duplicate")
	}
}

func TestExpiry(t *testing.T) {
	t.Parallel()

	start := time.Date(2000, 2, 1, 12, 0, 0, 0, time.UTC)
	beijing := time.FixedZone("Beijing Time", 8*60*60)
	w := NewWindow(start, time.Hour)

	if w.Expired(start.Add(59 * time.Minute).In(beijing)) {
		t.Fatal("should not be expired one minute before deadline (across zones)")
	}
	if !w.Expired(start.Add(time.Hour)) {
		t.Fatal("should be expired exactly at the deadline")
	}
}
```

## Review

The code is correct when every instant comparison goes through `.Equal`/`.Before`
and every time-as-key path goes through `Normalize`. `TestMonotonicBreaksDoubleEquals`
is the assertion that makes the danger tangible — a value not `==` to itself — and it
is exactly why `Window.Expired` must never use `==`. The normalization detail is
easy to get half-right: `.UTC()` alone fixes the zone but leaves the monotonic
reading, so two fresh `time.Now()` values at the "same" wall instant would still be
distinct keys; `.Round(0)` is what strips it. `TestNormalizedMapKeyCollapses` guards
both halves. Run `go test -race`.

## Resources

- [time.Time.Equal](https://pkg.go.dev/time#Time.Equal) — instant comparison and the `==` pitfalls (monotonic clock, Location).
- [The time package: monotonic clocks](https://pkg.go.dev/time#hdr-Monotonic_Clocks) — why `Round(0)` strips the monotonic reading.
- [time.Time.Before](https://pkg.go.dev/time#Time.Before) — ordering by instant, ignoring Location.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-config-reload-equality-guard.md](06-config-reload-equality-guard.md)
