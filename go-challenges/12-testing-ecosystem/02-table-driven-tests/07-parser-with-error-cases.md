# Exercise 7: A Byte-Size Parser with wantErr Cases

Parsing human-written limits — `512`, `10KB`, `4MiB` — out of config and flags is
a chore every backend does, and it is where rigorous error-case coverage earns its
keep, because a parser that silently returns a garbage value on bad input corrupts
everything downstream. This module builds `ParseByteSize(string) (int64, error)`
and tests it with a table that splits cleanly into success rows and failure rows,
with a guard that a failing parse never leaks a value.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
bytesize/                 independent module: example.com/bytesize
  go.mod                  go 1.26
  bytesize.go             ParseByteSize + unit table + sentinel errors
  cmd/
    demo/
      main.go             parses a few sizes, prints bytes
  bytesize_test.go        table over {name,in,want,wantErr} with a leak guard
```

- Files: `bytesize.go`, `cmd/demo/main.go`, `bytesize_test.go`.
- Implement: `ParseByteSize(string) (int64, error)` accepting a bare number and `KB`/`MB`/`GB` (decimal, 1000-based) and `KiB`/`MiB`/`GiB` (binary, 1024-based), rejecting empty, non-numeric, and negative input.
- Test: a table of `{name, in, want int64, wantErr bool}` that on an error row asserts `want == 0` and returns early so a spurious value can never pass.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bytesize/cmd/demo
cd ~/go-exercises/bytesize
go mod init example.com/bytesize
```

### Why the table splits into success and failure, and why the guard

This is the module where the *failure* rows carry as much weight as the success
rows, so the table's expected columns are designed for both: `want int64` for the
value on success, and `wantErr bool` for whether an error is expected. The subtle
bug a naive test invites is a *value leak*: on a row that should error, if you
assert the error but then also compare `got` against `want`, a parser that wrongly
returned `(500, someError)` might slip through if `want` happened to be 500. The
discipline is to check `(err != nil) == wantErr` first, and on an error row
`return` immediately after — never comparing the value. To make that guard
airtight, every failure row sets `want: 0`, so even if the early return were
removed, the assertion `got != 0` would still catch a leaked non-zero value.

The suffix matching has one real trap: `KiB` ends in `B`, and `KB` ends in `B`, so
the order you test suffixes in matters. The unit table is ordered longest-suffix
first — the three-character binary units, then the two-character decimal units,
then the bare `B` — so `4MiB` matches `MiB` (1048576) and not `B` (1). Getting this
order wrong would parse `4MiB` as "the number `4Mi` bytes", which fails to parse
and looks like a mysterious rejection. The numeric part is parsed with
`strconv.ParseFloat` so fractional sizes like `1.5MB` work, then multiplied and
converted to `int64`; a negative number is rejected outright, because a negative
byte limit is never meaningful.

The boundary rows are deliberate: `0` (valid, the additive identity), exact unit
multiples (`4MiB`, `1GiB`, where float multiplication must land on the exact
integer), and large values that exercise the `int64` range.

Create `bytesize.go`:

```go
package bytesize

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	ErrEmpty    = errors.New("empty size")
	ErrSyntax   = errors.New("malformed size")
	ErrNegative = errors.New("negative size")
)

// unit suffixes, ordered longest-first so KiB matches before KB before B.
var units = []struct {
	suffix string
	mult   int64
}{
	{"KiB", 1 << 10},
	{"MiB", 1 << 20},
	{"GiB", 1 << 30},
	{"KB", 1_000},
	{"MB", 1_000_000},
	{"GB", 1_000_000_000},
	{"B", 1},
}

// ParseByteSize parses a human-written size such as "512", "10KB", or "4MiB"
// into a count of bytes. Empty, non-numeric, and negative inputs are rejected.
func ParseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("parse %q: %w", s, ErrEmpty)
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			return scale(num, u.mult)
		}
	}
	return scale(s, 1)
}

func scale(num string, mult int64) (int64, error) {
	if num == "" {
		return 0, fmt.Errorf("no number: %w", ErrSyntax)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", num, ErrSyntax)
	}
	if f < 0 {
		return 0, fmt.Errorf("parse %q: %w", num, ErrNegative)
	}
	return int64(f * float64(mult)), nil
}
```

### The runnable demo

The demo parses a spread of sizes and prints the byte counts.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bytesize"
)

func main() {
	for _, in := range []string{"512", "10KB", "4MiB", "1GiB", "1.5MB"} {
		n, err := bytesize.ParseByteSize(in)
		if err != nil {
			fmt.Printf("%-6s -> error: %v\n", in, err)
			continue
		}
		fmt.Printf("%-6s -> %d bytes\n", in, n)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
512    -> 512 bytes
10KB   -> 10000 bytes
4MiB   -> 4194304 bytes
1GiB   -> 1073741824 bytes
1.5MB  -> 1500000 bytes
```

### The tests

The table divides into success rows (a real `want`, `wantErr: false`) and failure
rows (`want: 0`, `wantErr: true`). The assertion checks the error first and returns
early on a failure row, so no value comparison runs when an error is expected; the
`want: 0` on every failure row is the belt-and-suspenders guard.

Create `bytesize_test.go`:

```go
package bytesize

import "testing"

func TestParseByteSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{"bare_bytes", "512", 512, false},
		{"zero", "0", 0, false},
		{"explicit_B", "1000B", 1000, false},
		{"decimal_kb", "10KB", 10_000, false},
		{"binary_mib", "4MiB", 4_194_304, false},
		{"binary_gib", "1GiB", 1_073_741_824, false},
		{"fractional_mb", "1.5MB", 1_500_000, false},
		{"with_spaces", "  256KB  ", 256_000, false},
		{"empty", "", 0, true},
		{"non_numeric", "abc", 0, true},
		{"unknown_suffix", "12XY", 0, true},
		{"negative", "-1GB", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseByteSize(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseByteSize(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if tc.wantErr {
				return // never compare the value on an error row
			}
			if got != tc.want {
				t.Fatalf("ParseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
```

## Review

The parser is correct when success rows land on their exact byte counts and
failure rows return an error with no usable value. The two traps this table guards
are suffix ordering and value leaks. The longest-first unit order is what makes
`4MiB` parse as mebibytes rather than failing on a stray `Mi`; reorder the table so
`B` comes first and the `binary_mib` row goes red. The early `return` on `wantErr`,
backed by `want: 0` on every failure row, is what stops a parser that returns
`(garbage, err)` from sneaking a value past the test.

The boundary rows matter: `zero` proves 0 is valid, `binary_gib` proves the float
multiply lands exactly on 1073741824 with no rounding drift, and `with_spaces`
proves the outer and inner `TrimSpace` both fire. Run `go test -race` to confirm
the parallel rows share nothing.

## Resources

- [strconv.ParseFloat](https://pkg.go.dev/strconv#ParseFloat) — parsing the numeric part.
- [strings.HasSuffix / TrimSuffix](https://pkg.go.dev/strings#HasSuffix) — matching and stripping unit suffixes.
- [Go Wiki: TableDrivenTests](https://go.dev/wiki/TableDrivenTests) — the success/failure row split.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-json-serializer-golden.md](08-json-serializer-golden.md)
