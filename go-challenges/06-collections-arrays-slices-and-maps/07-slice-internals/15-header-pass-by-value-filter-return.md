# Exercise 15: A Redact Function That Silently Does Nothing

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every observability pipeline runs the same middleware: before a batch of
structured log records leaves the process, something walks it and strips
personally identifiable information -- emails, IPs, tokens -- so the
aggregator, and everyone with read access to it, never sees them. That
function is usually written twice in the same codebase with two different
signatures, and only one of them is safe to write as `func(records
[]Record)` with no return value: the one that overwrites fields on records
that already exist. The other shape, the one that also has to *drop* some
records, cannot be void, and a surprising number of production incidents
trace back to someone not noticing the difference until a customer's PII
showed up in a log sink it should never have reached.

The reason is the slice header. Passing `records []Record` into a function
copies three words -- pointer, length, capacity -- but the pointer still
refers to the caller's backing array. A function that writes through an
index, `records[i].Email = "..."`, is writing into that shared array, so the
caller sees the change the instant the call returns; no return value is
needed. A function that needs to *filter* records, though, must produce a
slice of a different length, and a different length cannot be expressed by
mutating existing elements -- it can only be expressed by returning a new
slice header. A `Redact` function that tries to filter void-style, by
building the right result internally and then doing `records = filtered`,
compiles without a single warning, runs without panicking, and has exactly
zero effect on the caller: the assignment only overwrites this function's
own local copy of the header. The batch that reaches the wire is the
original, unfiltered one.

This module builds `logscrub`, the pair of functions a log-scrubbing
middleware actually needs: `ScrubInPlace`, which is correctly void because it
only ever overwrites existing fields, and `Filter`, which is correctly
non-void because it changes cardinality. The void, do-nothing `Redact` is not
part of that API -- it lives only in the test file, as the thing the tests
prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
logscrub/                module example.com/logscrub
  go.mod                 go 1.24
  logscrub.go            Record, Scrubber; New, ScrubInPlace, Filter
  logscrub_test.go        visibility of in-place writes, filter table, aliasing,
                          the void-Redact contrast, concurrency, Example
```

- Files: `logscrub.go`, `logscrub_test.go`.
- Implement: `New(redaction string) (*Scrubber, error)` rejecting an empty marker with `ErrEmptyRedaction`; `(*Scrubber).ScrubInPlace(records []Record)` overwriting `Email` and `IP` on every record that has one set, through index assignment, with no return value; `Filter(records []Record, keep func(Record) bool) []Record` returning a freshly allocated slice the caller must reassign.
- Test: `ScrubInPlace`'s writes are visible at the call site with no return value, on a populated batch and on an empty and a nil one; empty `Email`/`IP` fields are left alone; the `Filter` table (keep some, keep all, keep none, nil input); `Filter`'s result never aliases its input; a `redactVoidBuggy` contrast proving a reassign-the-parameter "Redact" has zero effect on the caller's slice; `Scrubber` is safe for concurrent use; and `Example` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/logscrub
cd ~/go-exercises/logscrub
go mod init example.com/logscrub
go mod edit -go=1.24
```

### A slice header copy is not a slice copy

A slice value is three words: a pointer to a backing array, a length, and a
capacity. Passing `records []Record` to a function copies those three words,
but the pointer inside the copy still points at the *caller's* array. Two
consequences follow, and this module is built around the fact that they look
identical from the call site and are not. First: any write through an
existing index, `records[i] = x` or `records[i].Field = x`, lands in the
shared array, so it is visible to the caller the moment the function
returns -- no return value is involved, because none is needed.
`ScrubInPlace` is exactly this: it never changes how many records there are,
only what some of their fields hold, so it can be `func (s *Scrubber)
ScrubInPlace(records []Record)` and nothing more.

Second: `append` and reslicing produce a *new* header value. Assigning that
new header back to the local parameter, `records = something`, only changes
this function's own copy of the three words. It does not, and cannot, reach
back into the caller's variable -- Go has no reference-to-a-variable
semantics for a plain slice parameter. A filtering step, which by definition
must change the number of elements, therefore cannot be expressed as a
void function no matter how it is written internally:

```go
func Redact(records []Record) {
    filtered := make([]Record, 0, len(records))
    for _, r := range records {
        if !isPII(r) {
            filtered = append(filtered, r)
        }
    }
    records = filtered // compiles, looks correct, reaches nobody
}
```

`filtered` is built correctly and holds exactly the right records. The bug is
entirely in the last line: it reassigns a local variable that goes out of
scope the instant the function returns. The caller's own `records` slice --
its length, its contents, every PII field `isPII` was supposed to catch --
is untouched. (Swapping the last line for `records = append(records[:0],
filtered...)` does not fix this either; it trades a silent no-op for a
silent in-place overwrite of the caller's own backing array with the wrong
length still attached, which is worse, not better.) The only way to
communicate a new length out of a function is to return the new header and
have the caller reassign its own variable: `records = Filter(records,
keep)`.

Create `logscrub.go`:

```go
// Package logscrub strips personally identifiable information from a batch
// of structured log records before a middleware ships them to a downstream
// aggregator.
//
// It exists to keep two operations straight, because they look
// interchangeable and are not: ScrubInPlace writes into the elements a
// caller's slice already points at, so its effect is visible without a
// return value; Filter must build a shorter slice, so its result must be
// returned and reassigned by the caller. Confusing the two -- writing a
// "redact and filter" function that reassigns its own local slice variable
// and returns nothing -- compiles, looks correct, and silently ships every
// record it was supposed to drop. See the package tests for that exact
// failure, isolated and pinned.
package logscrub

import "errors"

// ErrEmptyRedaction is returned by New when the redaction marker is empty.
var ErrEmptyRedaction = errors.New("logscrub: redaction marker must not be empty")

// Record is one structured log line as produced by the application, before
// it is shipped to a log aggregator. Email and IP carry PII that must never
// leave the process unredacted.
type Record struct {
	Service string
	Message string
	Email   string
	IP      string
}

// Scrubber redacts the Email and IP fields of log records, replacing a
// non-empty value with a fixed marker.
//
// A Scrubber holds no mutable state after construction and is safe for
// concurrent use by multiple goroutines, provided distinct goroutines do not
// call ScrubInPlace on overlapping record slices at the same time -- that
// would be a data race on the records themselves, not on the Scrubber.
type Scrubber struct {
	redaction string
}

// New returns a Scrubber that replaces redacted values with redaction. It
// returns ErrEmptyRedaction if redaction is empty.
func New(redaction string) (*Scrubber, error) {
	if redaction == "" {
		return nil, ErrEmptyRedaction
	}
	return &Scrubber{redaction: redaction}, nil
}

// ScrubInPlace redacts Email and IP on every record that has one set.
//
// records is a slice header copied by value into this method, but the
// header's pointer still refers to the caller's backing array. The
// index-assignment writes below land in that shared array, so they are
// visible at the call site the moment ScrubInPlace returns -- there is
// nothing to reassign and nothing for the caller to do with a return value.
// An empty or nil records is a no-op.
func (s *Scrubber) ScrubInPlace(records []Record) {
	for i := range records {
		if records[i].Email != "" {
			records[i].Email = s.redaction
		}
		if records[i].IP != "" {
			records[i].IP = s.redaction
		}
	}
}

// Filter returns a new slice holding only the records for which keep
// reports true. It never mutates or aliases records: the returned slice has
// its own backing array, so the caller may retain, mutate, or discard it
// freely. Because Filter builds a slice of a different length than its
// input, its result cannot be observed through records -- the caller MUST
// reassign the return value (records = Filter(records, keep)) or the
// filtering has no effect at all.
//
// A nil or empty records returns a non-nil, zero-length slice.
func Filter(records []Record, keep func(Record) bool) []Record {
	out := make([]Record, 0, len(records))
	for _, r := range records {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}
```

### Using it

Construct one `Scrubber` at startup with the marker your log pipeline uses
for redacted values, then call it from the middleware on every outgoing
batch: `s.ScrubInPlace(batch)` first, to blank PII fields in place, then
`batch = logscrub.Filter(batch, keep)` if the pipeline also needs to drop
some records entirely -- for instance healthcheck noise that should never
reach the aggregator. The order matters for exactly the reason the type
exists: the first call needs no assignment, and forgetting the assignment on
the second call is the whole bug this module is about.

Both contracts a caller depends on are documented on the methods themselves.
`ScrubInPlace` never reallocates and never changes `len` or `cap`, so it is
safe to call on a slice a caller is about to hand to `json.Marshal` without
any further bookkeeping. `Filter`'s result never aliases its input, so
mutating the filtered slice afterward cannot corrupt the original batch --
`TestFilterDoesNotAliasInput` pins that.

`Example` is the runnable demonstration of this module: `go test` executes it
and compares its stdout against the `// Output:` comment below, so the usage
shown here cannot drift away from the code.

```go
func Example() {
	records := []Record{
		{Service: "checkout", Message: "order placed", Email: "alice@example.com", IP: "10.0.0.5"},
		{Service: "checkout", Message: "healthcheck", Email: "", IP: ""},
		{Service: "billing", Message: "charge failed", Email: "bob@example.com", IP: "10.0.0.9"},
	}

	s, err := New("[REDACTED]")
	if err != nil {
		panic(err)
	}

	s.ScrubInPlace(records)
	fmt.Println(records[0].Email, records[0].IP)
	fmt.Println(records[1].Email == "", records[1].IP == "")

	records = Filter(records, func(r Record) bool { return r.Message != "healthcheck" })
	fmt.Println(len(records))
	for _, r := range records {
		fmt.Println(r.Service, r.Message, r.Email)
	}

	// Output:
	// [REDACTED] [REDACTED]
	// true true
	// 2
	// checkout order placed [REDACTED]
	// billing charge failed [REDACTED]
}
```

### Tests

`TestScrubInPlaceIsVisibleAtTheCallSite` is the positive half of the lesson:
it calls a method that returns nothing and then reads the caller's own
slice, on a populated batch, an empty one, and a nil one, to show the write
already landed. `TestFilter` is the ordinary table -- keep some, keep all,
keep none, nil input -- each case asserting length first, because length is
what a void filter gets wrong. `TestFilterDoesNotAliasInput` pins the
aliasing contract documented on `Filter`.

`TestRedactVoidBuggyHasZeroEffect` is the heart of the module. `redactVoidBuggy`
is unexported and unreachable from the package API; it builds the correct
filtered result internally and then discards it by assigning it to its own
parameter, exactly the mistake described above. The test calls it, then
asserts the caller's slice is byte-for-byte identical to what it was before
the call -- same length, same PII, the healthcheck record still present --
and immediately afterward shows the correct pair, `ScrubInPlace` followed by
a reassigned `Filter`, actually changing what the caller sees. If a future
edit turns `Filter` into a void method, this test fails here instead of in
a customer's log sink.

Create `logscrub_test.go`:

```go
package logscrub

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func sample() []Record {
	return []Record{
		{Service: "checkout", Message: "order placed", Email: "alice@example.com", IP: "10.0.0.5"},
		{Service: "checkout", Message: "healthcheck", Email: "", IP: ""},
		{Service: "billing", Message: "charge failed", Email: "bob@example.com", IP: "10.0.0.9"},
	}
}

// redactVoidBuggy is the mapper as it is usually written the first time: it
// looks like an in-place scrub-and-drop, it compiles, and it does nothing at
// the call site. filtered is built correctly, but assigning it to the local
// parameter records only rewrites this function's own copy of the slice
// header -- the caller's header, and therefore the caller's slice, is
// untouched. It is never exported and never reachable from the package API;
// it exists only so the tests can pin what it gets wrong.
func redactVoidBuggy(records []Record, redaction string, keep func(Record) bool) {
	filtered := make([]Record, 0, len(records))
	for _, r := range records {
		if keep(r) {
			if r.Email != "" {
				r.Email = redaction
			}
			if r.IP != "" {
				r.IP = redaction
			}
			filtered = append(filtered, r)
		}
	}
	records = filtered // BUG: reassigns only the local header; filtered has its own backing array
}

func TestNewRejectsEmptyRedaction(t *testing.T) {
	t.Parallel()

	if _, err := New(""); !errors.Is(err, ErrEmptyRedaction) {
		t.Fatalf("New(\"\") error = %v, want ErrEmptyRedaction", err)
	}
}

func TestScrubInPlaceIsVisibleAtTheCallSite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []Record
		wantIdx int
	}{
		{name: "record with pii", records: sample(), wantIdx: 0},
		{name: "empty batch", records: []Record{}, wantIdx: -1},
		{name: "nil batch", records: nil, wantIdx: -1},
	}

	s, err := New("[REDACTED]")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s.ScrubInPlace(tc.records) // no return value: the mutation must already be visible
			if tc.wantIdx < 0 {
				return
			}
			r := tc.records[tc.wantIdx]
			if r.Email != "[REDACTED]" || r.IP != "[REDACTED]" {
				t.Fatalf("record %d = %+v, want Email and IP redacted", tc.wantIdx, r)
			}
		})
	}
}

func TestScrubInPlaceLeavesEmptyFieldsAlone(t *testing.T) {
	t.Parallel()

	s, err := New("[REDACTED]")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	records := sample()
	s.ScrubInPlace(records)

	health := records[1]
	if health.Email != "" || health.IP != "" {
		t.Fatalf("healthcheck record = %+v, want Email and IP left empty", health)
	}
}

func TestFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		records     []Record
		keep        func(Record) bool
		wantLen     int
		wantService string
	}{
		{
			name:        "keep some",
			records:     sample(),
			keep:        func(r Record) bool { return r.Message != "healthcheck" },
			wantLen:     2,
			wantService: "checkout",
		},
		{
			name:    "keep all",
			records: sample(),
			keep:    func(Record) bool { return true },
			wantLen: 3,
		},
		{
			name:    "keep none",
			records: sample(),
			keep:    func(Record) bool { return false },
			wantLen: 0,
		},
		{
			name:    "nil input",
			records: nil,
			keep:    func(Record) bool { return true },
			wantLen: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Filter(tc.records, tc.keep)
			if got == nil {
				t.Fatal("Filter returned nil, want a non-nil slice")
			}
			if len(got) != tc.wantLen {
				t.Fatalf("len(Filter(...)) = %d, want %d: %+v", len(got), tc.wantLen, got)
			}
			if tc.wantLen > 0 && tc.wantService != "" && got[0].Service != tc.wantService {
				t.Fatalf("Filter(...)[0].Service = %q, want %q", got[0].Service, tc.wantService)
			}
		})
	}
}

func TestFilterDoesNotAliasInput(t *testing.T) {
	t.Parallel()

	records := sample()
	filtered := Filter(records, func(Record) bool { return true })

	filtered[0].Message = "mutated"
	if records[0].Message != "order placed" {
		t.Fatalf("mutating the filtered slice changed the source record: %q", records[0].Message)
	}
}

// TestRedactVoidBuggyHasZeroEffect is the whole point of the module: it pins
// the exact failure a void-style, reassign-the-parameter "Redact" function
// ships to production. The buggy call runs to completion without error and
// without panic, and every record at the call site is exactly as it was
// before the call -- PII intact, healthcheck record still present.
func TestRedactVoidBuggyHasZeroEffect(t *testing.T) {
	t.Parallel()

	records := sample()
	before := append([]Record(nil), records...) // independent copy for comparison

	redactVoidBuggy(records, "[REDACTED]", func(r Record) bool { return r.Message != "healthcheck" })

	if len(records) != len(before) {
		t.Fatalf("len(records) = %d, want %d (the buggy call must not change the caller's length)", len(records), len(before))
	}
	for i := range records {
		if records[i] != before[i] {
			t.Fatalf("records[%d] = %+v, want unchanged %+v", i, records[i], before[i])
		}
	}

	// The correct pair, used the right way, does change the caller's view:
	// ScrubInPlace through the shared array, Filter through its return value.
	s, err := New("[REDACTED]")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.ScrubInPlace(records)
	records = Filter(records, func(r Record) bool { return r.Message != "healthcheck" })

	if len(records) != 2 {
		t.Fatalf("len(records) after the correct pair = %d, want 2", len(records))
	}
	for _, r := range records {
		if r.Email != "" && r.Email != "[REDACTED]" {
			t.Fatalf("record %+v still carries unredacted email", r)
		}
	}
}

func TestScrubberSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	s, err := New("[REDACTED]")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			batch := []Record{{Service: fmt.Sprintf("svc-%d", i), Email: "u@example.com", IP: "10.0.0.1"}}
			s.ScrubInPlace(batch)
			if batch[0].Email != "[REDACTED]" {
				t.Errorf("goroutine %d: batch = %+v", i, batch)
			}
		}(i)
	}
	wg.Wait()
}

// Example demonstrates the full pair used correctly: ScrubInPlace mutates
// through the shared backing array with no return value, and Filter's
// result is reassigned to records because Filter cannot change the caller's
// slice any other way.
func Example() {
	records := []Record{
		{Service: "checkout", Message: "order placed", Email: "alice@example.com", IP: "10.0.0.5"},
		{Service: "checkout", Message: "healthcheck", Email: "", IP: ""},
		{Service: "billing", Message: "charge failed", Email: "bob@example.com", IP: "10.0.0.9"},
	}

	s, err := New("[REDACTED]")
	if err != nil {
		panic(err)
	}

	s.ScrubInPlace(records)
	fmt.Println(records[0].Email, records[0].IP)
	fmt.Println(records[1].Email == "", records[1].IP == "")

	records = Filter(records, func(r Record) bool { return r.Message != "healthcheck" })
	fmt.Println(len(records))
	for _, r := range records {
		fmt.Println(r.Service, r.Message, r.Email)
	}

	// Output:
	// [REDACTED] [REDACTED]
	// true true
	// 2
	// checkout order placed [REDACTED]
	// billing charge failed [REDACTED]
}
```

## Review

`ScrubInPlace` is correct when it changes fields on records the caller
already owns and needs no return value, because index-assignment writes
land directly in the shared backing array; `Filter` is correct when it
changes cardinality and always returns a new header the caller must
reassign, because there is no other way to communicate a new length out of
a function. The trap is a function that tries to do both without a return
value: it builds the right result and then throws it away by assigning it
to its own parameter, which only rewrites a local copy of a three-word
header. `New` rejects an empty redaction marker with `ErrEmptyRedaction`,
checkable with `errors.Is`. `Filter`'s result never aliases its input, and
`Scrubber` carries no mutable state after construction, so it is safe to
share across goroutines. `Example` is the executable documentation: `go
test` verifies its output. Run `go test -count=1 -race ./...`.

## Resources

- [Go Slices: usage and internals](https://go.dev/blog/slices-intro) — the three-word header and why passing a slice does not copy its backing array.
- [Go Specification: Assignability](https://go.dev/ref/spec#Assignability) — why reassigning a function parameter never affects the caller's variable.
- [`append`](https://pkg.go.dev/builtin#append) — why it returns a new header instead of mutating its argument's length in place.
- [Effective Go: Slices](https://go.dev/doc/effective_go#slices) — the pass-by-value semantics of slice headers in function calls.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-slices-chunk-batch-aliasing.md](14-slices-chunk-batch-aliasing.md) | Next: [16-arena-allocator-capacity-ceiling.md](16-arena-allocator-capacity-ceiling.md)
