# Exercise 14: Key-Scoped Debounce Detector Driven by a Logical Clock

**Nivel: Intermedio** — validacion rapida (un test corto).

A webhook receiver gets the same delivery redelivered a few times in a row
whenever the sender's retry policy fires, and must treat only the first of
each burst as new — without a real timer, since the ingestion loop already
has its own notion of "how many cycles have passed." `NewDetector` closes
over a map keyed by delivery ID and an injected tick function instead of a
wall clock, so the whole test runs on an integer counter.

## What you'll build

```text
debounce/                  independent module: example.com/logical-clock-debounce
  go.mod                   go 1.24
  debounce.go              NewDetector returns func(key string) bool
  debounce_test.go         table test: per-key window, two detectors isolated
```

- Files: `debounce.go`, `debounce_test.go`.
- Implement: `NewDetector(window int64, tick func() int64) func(key string) bool`, closing over a `map[string]int64` of last-seen tick per key.
- Test: a table advances a fake integer ticker and checks that repeats of the same key within `window` ticks are suppressed, a different key is never suppressed by another key's history, and a second detector never shares the map.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A tick function instead of a clock

Every time-based module elsewhere in this lesson injects `now func() time.Time`
so a test can fake wall-clock time. This one injects `tick func() int64` instead
— a plain integer counter standing in for whatever monotonic sequence the
caller's own event loop already tracks, such as a processed-batch counter or an
inbound-request counter. The technique is identical dependency injection, just
over a coarser, application-defined "clock" than `time.Time`; a production
caller wires in a real counter, and the test wires in one it advances by hand.

`NewDetector` captures one `map[string]int64` shared by every call to the
returned closure. On each call it reads `tick()`, compares it against the last
tick recorded for that key, and reports whether enough ticks (`>= window`) have
elapsed since. A hit inside the window returns `false` and leaves the map
untouched — the original delivery's tick keeps counting down the window for any
further retries in the same burst. A miss records the new tick and returns
`true`. The map never shrinks: this closure's captured state grows with the
number of distinct keys ever seen, so a long-lived detector over an unbounded
key space needs its own periodic sweep in production — the module intentionally
does not solve that here.

Create `debounce.go`:

```go
package debounce

// NewDetector returns a closure that reports whether key is "new" — that is,
// not seen within the last window logical ticks. tick is injected so tests
// drive a fake logical clock directly instead of sleeping; production wires
// in a monotonic request counter or a wall-clock-derived tick.
//
// The returned closure keeps one map entry per key for the detector's whole
// lifetime; nothing evicts old keys, so a long-lived detector over an
// unbounded key space needs its own periodic cleanup.
func NewDetector(window int64, tick func() int64) func(key string) bool {
	lastSeen := make(map[string]int64)

	return func(key string) bool {
		now := tick()
		if last, ok := lastSeen[key]; ok && now-last < window {
			return false
		}
		lastSeen[key] = now
		return true
	}
}
```

### Tests

Create `debounce_test.go`:

```go
package debounce

import "testing"

// fakeTicker returns a controllable logical clock: tick reads the current
// counter, advance moves it forward by exactly the given number of ticks.
func fakeTicker(start int64) (tick func() int64, advance func(int64)) {
	cur := start
	tick = func() int64 { return cur }
	advance = func(d int64) { cur += d }
	return tick, advance
}

func TestDetectorSuppressesDuplicatesWithinWindow(t *testing.T) {
	tick, advance := fakeTicker(0)
	isNew := NewDetector(5, tick)

	tests := []struct {
		name    string
		key     string
		advance int64
		want    bool
	}{
		{"a first delivery", "a", 0, true},
		{"a immediate retry, same tick", "a", 0, false},
		{"b different key, same tick", "b", 0, true},
		{"a retry within window", "a", 4, false},
		{"a retry still within window", "a", 0, false},
		{"a retry past window", "a", 1, true},
	}

	for _, tc := range tests {
		advance(tc.advance)
		if got := isNew(tc.key); got != tc.want {
			t.Fatalf("%s: isNew(%q) = %v, want %v", tc.name, tc.key, got, tc.want)
		}
	}
}

func TestTwoDetectorsDoNotShareState(t *testing.T) {
	tickA, _ := fakeTicker(0)
	tickB, _ := fakeTicker(0)

	isNewA := NewDetector(5, tickA)
	isNewB := NewDetector(5, tickB)

	if !isNewA("x") {
		t.Fatal("detector A: first sighting of x, want true")
	}
	if !isNewB("x") {
		t.Fatal("detector B: first sighting of x, want true — detectors must not share captured state")
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The table walks one key through a full burst — first sighting, an immediate
duplicate, two retries still inside the five-tick window, then one past it —
and checks a second key is judged independently at the same ticks. The
isolation test confirms what `NewDetector` promises structurally: two calls
allocate two separate `lastSeen` maps, so nothing a caller does with detector
`A` can influence what detector `B` reports, exactly like every other stateful
factory in this lesson.

## Resources

- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — the captured `lastSeen` map.
- [pkg.go.dev: maps package overview](https://pkg.go.dev/maps) — for a production version that also needs eviction, see `maps.DeleteFunc`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-request-scoped-logger-factory.md](13-request-scoped-logger-factory.md) | Next: [15-admission-gate-closure-pair.md](15-admission-gate-closure-pair.md)
