# Exercise 10: Seeding a Fuzz Target from a Table

A table pins the named contracts a human wrote down; a fuzzer explores the space
around them for inputs nobody thought of. They are complementary, and the bridge
between them is `f.Add`: every representative table row becomes a fuzz seed. This
module builds a label-set encoder (`Parse`/`Format`, the kind of canonical
key=value codec behind a metrics line or a cache key), tests it with a table, and
then fuzzes its round-trip invariant with the table rows as seeds.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
labelset/                 independent module: example.com/labelset
  go.mod                  go 1.26
  labelset.go             Parse, Format (canonical key=value codec)
  cmd/
    demo/
      main.go             parses and re-formats a label string
  labelset_test.go        Parse/Format table + FuzzRoundTrip seeded from the table
```

- Files: `labelset.go`, `cmd/demo/main.go`, `labelset_test.go`.
- Implement: `Parse(string) (map[string]string, error)` and `Format(map[string]string) string`, a canonical comma-separated `k=v` codec with sorted output.
- Test: a table pinning specific `Parse` and `Format` results, plus `FuzzRoundTrip` that seeds `f.Add` from the table rows and asserts `Parse` never panics and `Parse(Format(m))` round-trips.
- Verify: `go test -count=1 -race ./...`; explore with `go test -fuzz=FuzzRoundTrip`.

Set up the module:

```bash
mkdir -p ~/go-exercises/labelset/cmd/demo
cd ~/go-exercises/labelset
go mod init example.com/labelset
```

### Why the table and the fuzzer need each other

The table is where the *named* contracts live: the empty string parses to an empty
map, `env=prod,region=us` parses to those two pairs, a pair missing its `=` is
rejected, `Format` emits keys in sorted order. Those are the human-meaningful
behaviors, each a row with a name you can filter and a failure that echoes the
input. The table is precise but narrow — it only checks the inputs you thought to
write.

The fuzzer covers the breadth the table cannot. `FuzzRoundTrip` seeds itself from
the table by calling `f.Add(row.in)` for each representative input, then generates
mutations around those seeds and asserts an *invariant* that must hold for every
input: `Parse` never panics, and whenever `Parse` succeeds, `Format` of the result
re-parses to the same map. That round-trip property is the honest invariant of a
codec — parse then serialize then parse must be stable — and it catches whole
classes of bugs (a value that breaks the delimiter, a key that survives one
direction but not the other) that no hand-written row would enumerate.

Two things about the shape are worth internalizing. First, a fuzz body is *not*
table-driven internally: `f.Fuzz(func(t *testing.T, s string) { ... })` receives
exactly one generated input per call, so the invariant is written once and applies
to all of them — you cannot loop a table inside it. The table's role is to supply
the seeds and the named contracts; the fuzzer's role is the invariant. Second, the
codec is designed so the invariant actually holds: `Parse` splits pairs on `,` and
each pair on its *first* `=` (via `strings.Cut`), so a value may contain `=` but,
having been split out of a `,`-delimited pair, can never contain `,`. That is what
lets `Format` reassemble without escaping and still round-trip — a real design
constraint the fuzzer would expose instantly if it were violated.

During a normal `go test` run the fuzz function executes only against its seed
corpus (the `f.Add` inputs), which is why seeding from the table matters even
before you run `-fuzz`: the seeds become regression cases that run on every build.

Create `labelset.go`:

```go
package labelset

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ErrSyntax is returned for a malformed label string.
var ErrSyntax = errors.New("malformed label set")

// Parse decodes a canonical "k=v,k=v" label string into a map. An empty string
// is an empty set. A pair without '=' or with an empty key is rejected.
func Parse(s string) (map[string]string, error) {
	m := make(map[string]string)
	if s == "" {
		return m, nil
	}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("pair %q missing '=': %w", pair, ErrSyntax)
		}
		if k == "" {
			return nil, fmt.Errorf("pair %q has empty key: %w", pair, ErrSyntax)
		}
		m[k] = v
	}
	return m, nil
}

// Format encodes a label map into the canonical "k=v,k=v" form with keys sorted,
// so equal maps always format to the same string.
func Format(m map[string]string) string {
	keys := slices.Sorted(maps.Keys(m))
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
	}
	return b.String()
}
```

### The runnable demo

The demo parses a label string, prints the map, and re-formats it to show the
canonical sorted output.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/labelset"
)

func main() {
	m, err := labelset.Parse("region=us-east,env=prod")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("parsed:   ", m)
	fmt.Println("formatted:", labelset.Format(m))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
parsed:    map[env:prod region:us-east]
formatted: env=prod,region=us-east
```

### The tests

`TestParse` and `TestFormat` pin the named contracts. `parseSeeds` is the shared
list of representative inputs; `TestParse` iterates it and `FuzzRoundTrip` seeds
`f.Add` from it, so the two stay in lockstep. The fuzz body asserts the invariant:
`Parse` never panics, and a successful parse round-trips through `Format`.

Create `labelset_test.go`:

```go
package labelset

import (
	"maps"
	"testing"
)

// parseSeeds are representative inputs shared by the table and the fuzz seeds.
var parseSeeds = []string{
	"",
	"a=1",
	"env=prod,region=us-east",
	"k=v=w",    // value may contain '='
	"empty=",   // empty value is valid
	"a=1,a=2",  // duplicate key: last wins
	"missing",  // no '=' -> error
	"=novalue", // empty key -> error
}

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{"empty", "", map[string]string{}, false},
		{"single", "a=1", map[string]string{"a": "1"}, false},
		{"pair", "env=prod,region=us-east", map[string]string{"env": "prod", "region": "us-east"}, false},
		{"value_has_equals", "k=v=w", map[string]string{"k": "v=w"}, false},
		{"empty_value", "empty=", map[string]string{"empty": ""}, false},
		{"duplicate_last_wins", "a=1,a=2", map[string]string{"a": "2"}, false},
		{"missing_equals", "missing", nil, true},
		{"empty_key", "=novalue", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Parse(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !maps.Equal(got, tc.want) {
				t.Fatalf("Parse(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   map[string]string
		want string
	}{
		{"empty", map[string]string{}, ""},
		{"single", map[string]string{"a": "1"}, "a=1"},
		{"sorted", map[string]string{"region": "us", "env": "prod"}, "env=prod,region=us"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Format(tc.in); got != tc.want {
				t.Fatalf("Format(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func FuzzRoundTrip(f *testing.F) {
	for _, s := range parseSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := Parse(s)
		if err != nil {
			return // only valid parses have a round-trip obligation
		}
		out := Format(m)
		m2, err := Parse(out)
		if err != nil {
			t.Fatalf("Format(%v) = %q not re-parseable: %v", m, out, err)
		}
		if !maps.Equal(m, m2) {
			t.Fatalf("round trip changed map: %v -> %q -> %v", m, out, m2)
		}
	})
}
```

## Review

The codec is correct when `Parse` and `Format` are inverses on valid input, and
the two testing styles prove different halves of that. The table pins the specific
behaviors — sorted output, first-`=` split, rejection of malformed pairs — with
named, filterable cases. The fuzzer, seeded from the same input list, asserts the
round-trip invariant across inputs no one enumerated, and its seeds double as
regression cases on every plain `go test` run. Keeping `parseSeeds` shared between
`TestParse` and `FuzzRoundTrip` is what keeps the seed corpus honest: a new named
case is a new seed for free.

The design constraint that makes the invariant true is the delimiter discipline:
splitting pairs on `,` and each pair on its *first* `=` means a parsed value never
contains `,`, so `Format` can reassemble without escaping. If you extended the
codec to allow `,` inside a value, the round trip would break and
`go test -fuzz=FuzzRoundTrip` would hand you the exact input that exposes it —
which is the fuzzer doing the job the table cannot.

## Resources

- [testing.F: Add and Fuzz](https://pkg.go.dev/testing#F) — seeding a corpus and writing a fuzz target.
- [Go Fuzzing tutorial](https://go.dev/doc/tutorial/fuzz) — seeding with `f.Add` and running `-fuzz`.
- [strings.Cut](https://pkg.go.dev/strings#Cut) — first-separator split, the basis of the round-trip guarantee.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../03-test-helpers/00-concepts.md](../03-test-helpers/00-concepts.md)
