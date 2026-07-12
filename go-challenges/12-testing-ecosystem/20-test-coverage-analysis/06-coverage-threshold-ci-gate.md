# Exercise 6: Fail CI below a coverage floor by parsing the total line

The operational use of coverage is regression detection: a CI step reads the
`total:` line from `go tool cover -func` and fails the build if it dropped below a
configured floor. This module builds that gate as a real, table-tested unit — a
robust parser of the `total:` line plus a threshold check with a sentinel error —
and shows how a CI script wires it to `go tool cover` with `os/exec`.

This module is fully self-contained: its own `go mod init`, a demo, and tests.

## What you'll build

```text
covgate/                   independent module: example.com/covgate
  go.mod
  gate.go                  ParseTotal, CheckFloor; ErrBelowFloor sentinel
  cmd/
    demo/
      main.go              parse a sample -func output and report pass/fail
  gate_test.go             table tests over parsing and the floor check
```

- Files: `gate.go`, `cmd/demo/main.go`, `gate_test.go`.
- Implement: `ParseTotal(funcOutput string) (float64, error)` that finds the `total:` line and returns its percentage, and `CheckFloor(coverage, floor float64) error` returning a wrapped `ErrBelowFloor` when under.
- Test: table tests asserting parsing across whitespace and trailing-`%` variants, a missing-total error, and the floor check at, above, and below.
- Verify: `go test -count=1 -race ./...`

### Parse the total line robustly, not by index

`go tool cover -func=cover.out` prints one line per function and a final line:

```
example.com/svc/repo/repo.go:14:  Find          100.0%
example.com/svc/repo/repo.go:28:  Save           66.7%
total:                            (statements)   88.9%
```

The naive gate reads "the last line" or "line N" and splits on whitespace by a
fixed index. Both are fragile: the number of functions above `total:` varies from
build to build, and the column widths are alignment-padded, so a fixed field index
breaks the moment a function name gets longer. The robust approach is semantic:
scan for the line that begins with `total:`, take its *last* whitespace-separated
field, strip the trailing `%`, and parse the float. That survives any number of
functions and any padding, and it fails loudly with a clear error if no `total:`
line is present (an empty package, a tool error) rather than silently parsing
garbage.

`CheckFloor` compares the parsed coverage against the floor and returns a wrapped
sentinel `ErrBelowFloor` when it is under, so a CI wrapper can classify the failure
with `errors.Is` and print a precise message. The floor is deliberately a
parameter set below 100% — a ratchet that catches regressions, not a target to
maximize.

Create `gate.go`:

```go
package covgate

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrBelowFloor is returned by CheckFloor when coverage is under the floor.
var ErrBelowFloor = errors.New("coverage below floor")

// ErrNoTotal is returned by ParseTotal when the input has no total: line.
var ErrNoTotal = errors.New("no total line in coverage output")

// ParseTotal extracts the whole-profile percentage from the output of
// `go tool cover -func`. It finds the line beginning with "total:" and parses
// the trailing percentage, tolerating alignment padding and the % suffix.
func ParseTotal(funcOutput string) (float64, error) {
	for _, line := range strings.Split(funcOutput, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "total:") {
			continue
		}
		fields := strings.Fields(trimmed)
		last := fields[len(fields)-1]
		last = strings.TrimSuffix(last, "%")
		pct, err := strconv.ParseFloat(last, 64)
		if err != nil {
			return 0, fmt.Errorf("parse total %q: %w", fields[len(fields)-1], err)
		}
		return pct, nil
	}
	return 0, ErrNoTotal
}

// CheckFloor returns nil when coverage >= floor, else a wrapped ErrBelowFloor.
func CheckFloor(coverage, floor float64) error {
	if coverage < floor {
		return fmt.Errorf("coverage %.1f%% < floor %.1f%%: %w", coverage, floor, ErrBelowFloor)
	}
	return nil
}
```

### How a CI script wires this to `go tool cover`

In CI you run the tests, produce a profile, capture the `-func` output, and pipe
it through this gate. The wiring uses `os/exec` to run `go tool cover`; the code
below is the shape of that wrapper (it runs the real tool, so it belongs in the CI
binary, not the unit test):

```go
// runFuncTotal runs `go tool cover -func` on a profile and returns its percentage.
// This is the exec-based glue a CI binary uses; the unit tests exercise
// ParseTotal directly on captured output instead, so they need no toolchain.
func runFuncTotal(profile string) (float64, error) {
	out, err := exec.Command("go", "tool", "cover", "-func="+profile).Output()
	if err != nil {
		return 0, fmt.Errorf("go tool cover: %w", err)
	}
	return ParseTotal(string(out))
}
```

The equivalent shell one-liner many teams inline into a CI job:

```bash
go test -coverprofile=cover.out ./...
pct=$(go tool cover -func=cover.out | awk '/^total:/ {print $NF}' | tr -d '%')
awk -v p="$pct" -v floor=80 'BEGIN { exit (p < floor) }' || {
  echo "coverage $pct% below floor 80%"; exit 1; }
```

The Go gate is preferable in a real pipeline because it is unit-tested and returns
a typed error you can act on, rather than fragile inline `awk`.

### The runnable demo

The demo parses a fixed sample of `-func` output (so it is deterministic without a
toolchain) and reports the gate decision at two different floors.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/covgate"
)

const sample = `example.com/svc/repo/repo.go:14:  Find          100.0%
example.com/svc/repo/repo.go:28:  Save           66.7%
example.com/svc/service/service.go:11:  Register    85.7%
total:                            (statements)   88.9%
`

func main() {
	pct, err := covgate.ParseTotal(sample)
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}
	fmt.Printf("parsed total: %.1f%%\n", pct)

	report(pct, 80.0)
	report(pct, 95.0)
}

func report(pct, floor float64) {
	err := covgate.CheckFloor(pct, floor)
	switch {
	case err == nil:
		fmt.Printf("floor %.1f%%: PASS\n", floor)
	case errors.Is(err, covgate.ErrBelowFloor):
		fmt.Printf("floor %.1f%%: FAIL (%v)\n", floor, err)
	default:
		fmt.Printf("floor %.1f%%: ERROR (%v)\n", floor, err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
parsed total: 88.9%
floor 80.0%: PASS
floor 95.0%: FAIL (coverage 88.9% < floor 95.0%: coverage below floor)
```

### The tests

The parser table covers the padded normal case, extra surrounding whitespace, a
`100.0%` total, and the missing-`total:` error. The floor table covers at, above,
and below, asserting the sentinel with `errors.Is`.

Create `gate_test.go`:

```go
package covgate

import (
	"errors"
	"math"
	"testing"
)

func TestParseTotal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    float64
		wantErr error
	}{
		{
			name:  "padded",
			input: "foo.go:1:\tFind\t100.0%\ntotal:\t\t(statements)\t88.9%\n",
			want:  88.9,
		},
		{
			name:  "leading-space",
			input: "   total:   (statements)   72.5%",
			want:  72.5,
		},
		{
			name:  "full",
			input: "total: (statements) 100.0%",
			want:  100.0,
		},
		{
			name:    "no-total",
			input:   "foo.go:1: Find 100.0%\n",
			wantErr: ErrNoTotal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseTotal(tt.input)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseTotal err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("ParseTotal = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckFloor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		cov, floor float64
		wantErr    bool
	}{
		{"above", 88.9, 80.0, false},
		{"exactly-at", 80.0, 80.0, false},
		{"below", 79.9, 80.0, true},
		{"zero-floor", 0.0, 0.0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := CheckFloor(tt.cov, tt.floor)
			if tt.wantErr {
				if !errors.Is(err, ErrBelowFloor) {
					t.Fatalf("CheckFloor(%v,%v) err = %v, want ErrBelowFloor", tt.cov, tt.floor, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckFloor(%v,%v) = %v, want nil", tt.cov, tt.floor, err)
			}
		})
	}
}

func TestParseAndCheck(t *testing.T) {
	t.Parallel()
	out := "a.go:1: F 100.0%\ntotal:\t(statements)\t91.2%\n"
	pct, err := ParseTotal(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckFloor(pct, 90.0); err != nil {
		t.Errorf("CheckFloor(91.2, 90) = %v, want nil", err)
	}
	if err := CheckFloor(pct, 92.0); !errors.Is(err, ErrBelowFloor) {
		t.Errorf("CheckFloor(91.2, 92) = %v, want ErrBelowFloor", err)
	}
}
```

## Review

The gate is correct when `ParseTotal` returns the percentage from the `total:`
line regardless of alignment padding or a trailing `%`, returns `ErrNoTotal` when
there is no such line, and `CheckFloor` wraps `ErrBelowFloor` exactly when
coverage is under the floor — pinned by the parser and floor tables and the
combined `TestParseAndCheck`.

The mistake to avoid is parsing the total by a fixed line number or field index;
the number of functions above `total:` and the column padding both vary, so index-
based parsing breaks silently on a different build. Scan for the `total:` prefix
and take the last field. And keep the floor a deliberate parameter below 100% — a
regression ratchet, never a target that would reward assertion-free tests. Run
`go test -race` to confirm the parallel tables are clean.

## Resources

- [`go tool cover`](https://pkg.go.dev/cmd/cover) — the `-func` output whose `total:` line this parses.
- [`os/exec`](https://pkg.go.dev/os/exec) — running `go tool cover` from a CI binary.
- [`strconv.ParseFloat`](https://pkg.go.dev/strconv#ParseFloat) and [`strings.Fields`](https://pkg.go.dev/strings#Fields) — the robust parse primitives.

---

Back to [05-merge-unit-integration-coverage.md](05-merge-unit-integration-coverage.md) | Next: [07-error-branch-coverage-repository.md](07-error-branch-coverage-repository.md)
