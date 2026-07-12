# Exercise 17: Parse Logs and Aggregate Errors by Type and Location

**Nivel: Intermedio** — validacion rapida (un test corto).

An on-call dashboard that shows "5,000 error lines" is useless; the useful
question is "which error type, at which call site, is spiking." This module
streams raw log lines through a parser exposed as `iter.Seq2[LogLine, error]`
— the Go 1.23 iterator shape for a sequence that can fail per-element — then
ranges that stream once to group errors by `(Type, Location)` in a map, and
flattens the map back to a sorted slice with occurrence counts. The module is
fully self-contained: its own `go mod init`, no external dependencies.

## What you'll build

```text
logagg/                     independent module: example.com/log-error-aggregator-by-type
  go.mod                    go 1.24
  logagg.go                 Parse(raw) iter.Seq2[LogLine, error]; Aggregate(seq) ([]ErrorGroup, []error)
  cmd/
    demo/
      main.go               runnable demo: mixed valid/malformed lines
  logagg_test.go            table test: grouping + malformed-line handling + empty input
```

- Files: `logagg.go`, `cmd/demo/main.go`, `logagg_test.go`.
- Implement: `Parse(raw []string) iter.Seq2[LogLine, error]` and
  `Aggregate(seq iter.Seq2[LogLine, error]) ([]ErrorGroup, []error)`.
- Test: one table covering grouping across repeated and distinct
  `(Type, Location)` pairs plus a malformed line, and an empty-input case.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/17-log-error-aggregator-by-type/cmd/demo
cd go-solutions/03-control-flow/05-range-over-collections/17-log-error-aggregator-by-type
go mod edit -go=1.24
```

### iter.Seq2 lets a stream carry per-element failure without stopping it

A naive parser would return `([]LogLine, error)` and abort on the first bad
line, which throws away every error report parsed after the corrupt one — the
opposite of what an aggregator needs. `Parse` instead yields `(LogLine, error)`
pairs one at a time: a well-formed line yields `(ll, nil)`, a malformed one
yields `(LogLine{}, err)`, and the stream keeps going either way unless the
consumer's `yield` call returns `false` (a `break` in the caller's range).
This is the same shape as `bufio.Scanner`'s `(token, err)` pattern, just lazy
and composable as a `for ... range` instead of a `for scanner.Scan()` loop.

`Aggregate` is the accumulate-then-flatten idiom from this lesson's other
exercises, applied to a range-over-func source instead of a slice: range the
`iter.Seq2` once, route errors to one slice and successes into a
`map[key]int` scratch structure, then flatten that map into a sorted slice.
The struct key `{typ, loc}` — rather than string-concatenating `Type+"@"+Location`
— avoids both a delimiter-collision bug (a `Location` containing `@`) and an
allocation per line; struct keys with comparable fields are valid map keys
with zero extra cost.

Create `logagg.go`:

```go
package logagg

import (
	"fmt"
	"iter"
	"sort"
	"strings"
)

// LogLine is one parsed error-log entry.
type LogLine struct {
	Type     string
	Location string
	Message  string
}

// ErrorGroup is the aggregated occurrence count for one (Type, Location)
// pair.
type ErrorGroup struct {
	Type     string
	Location string
	Count    int
}

// Parse returns a lazy iter.Seq2 over raw log lines formatted as
// "type|location|message". A malformed line yields a zero LogLine and a
// non-nil error instead of stopping the stream, so the caller decides
// whether to skip it or abort by returning false from its range body.
func Parse(raw []string) iter.Seq2[LogLine, error] {
	return func(yield func(LogLine, error) bool) {
		for _, line := range raw {
			parts := strings.SplitN(line, "|", 3)
			if len(parts) != 3 {
				if !yield(LogLine{}, fmt.Errorf("malformed log line: %q", line)) {
					return
				}
				continue
			}
			ll := LogLine{Type: parts[0], Location: parts[1], Message: parts[2]}
			if !yield(ll, nil) {
				return
			}
		}
	}
}

// Aggregate ranges seq once, grouping successfully parsed lines by
// (Type, Location) and counting occurrences. Malformed lines are collected
// separately as parse errors rather than silently dropped. The returned
// groups are sorted by Type then Location so the result is deterministic
// regardless of the intermediate map's iteration order.
func Aggregate(seq iter.Seq2[LogLine, error]) (groups []ErrorGroup, errs []error) {
	type key struct{ typ, loc string }
	counts := make(map[key]int)

	for ll, err := range seq {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		counts[key{ll.Type, ll.Location}]++
	}

	groups = make([]ErrorGroup, 0, len(counts))
	for k, c := range counts {
		groups = append(groups, ErrorGroup{Type: k.typ, Location: k.loc, Count: c})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Type != groups[j].Type {
			return groups[i].Type < groups[j].Type
		}
		return groups[i].Location < groups[j].Location
	})
	return groups, errs
}
```

### The runnable demo

The demo feeds five raw lines — two repeats of the same type/location, one
malformed line, and two other type/location combinations — through `Parse`
and `Aggregate`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/log-error-aggregator-by-type"
)

func main() {
	raw := []string{
		"timeout|payments-svc|dial tcp: i/o timeout",
		"timeout|payments-svc|dial tcp: i/o timeout",
		"not-json",
		"nilptr|checkout-svc|invalid memory address",
		"timeout|checkout-svc|context deadline exceeded",
	}

	groups, errs := logagg.Aggregate(logagg.Parse(raw))

	for _, g := range groups {
		fmt.Printf("%s@%s: %d\n", g.Type, g.Location, g.Count)
	}
	fmt.Printf("parse errors: %d\n", len(errs))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
nilptr@checkout-svc: 1
timeout@checkout-svc: 1
timeout@payments-svc: 2
parse errors: 1
```

### Tests

The table covers a mix of repeated and distinct `(Type, Location)` pairs with
one malformed line interleaved, plus an empty-input case that must return an
empty (not nil-vs-empty-mismatched) slice and zero parse errors.

Create `logagg_test.go`:

```go
package logagg

import (
	"reflect"
	"testing"
)

func TestAggregate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      []string
		want     []ErrorGroup
		wantErrs int
	}{
		{
			name:     "empty input",
			raw:      []string{},
			want:     []ErrorGroup{},
			wantErrs: 0,
		},
		{
			name: "groups by type and location, malformed lines counted separately",
			raw: []string{
				"timeout|payments-svc|dial tcp: i/o timeout",
				"timeout|payments-svc|dial tcp: i/o timeout",
				"not-json",
				"nilptr|checkout-svc|invalid memory address",
				"timeout|checkout-svc|context deadline exceeded",
			},
			want: []ErrorGroup{
				{Type: "nilptr", Location: "checkout-svc", Count: 1},
				{Type: "timeout", Location: "checkout-svc", Count: 1},
				{Type: "timeout", Location: "payments-svc", Count: 2},
			},
			wantErrs: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			groups, errs := Aggregate(Parse(tc.raw))
			if !reflect.DeepEqual(groups, tc.want) {
				t.Errorf("Aggregate() groups = %+v, want %+v", groups, tc.want)
			}
			if len(errs) != tc.wantErrs {
				t.Errorf("Aggregate() errs = %d, want %d", len(errs), tc.wantErrs)
			}
		})
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

The aggregator is correct when every malformed line lands in `errs`, every
well-formed line is counted under its exact `(Type, Location)` pair, and the
group order never depends on the accumulation map's iteration order. The bug
this design specifically avoids is letting one bad line abort the whole
parse — because `Parse` yields an error per element instead of returning one
error for the entire stream, a single corrupt log line costs you one entry in
`errs`, not the rest of the report.

## Resources

- [package iter](https://pkg.go.dev/iter) — `Seq2` and the range-over-func iterator shape.
- [Go Specification: For statements (range-over-func)](https://go.dev/ref/spec#For_statements)
- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner) — the classic pull-based analog to this push iterator.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-idempotency-key-gate-windowed.md](16-idempotency-key-gate-windowed.md) | Next: [18-health-check-aggregator.md](18-health-check-aggregator.md)
