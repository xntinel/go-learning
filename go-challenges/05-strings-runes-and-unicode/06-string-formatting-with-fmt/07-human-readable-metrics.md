# Exercise 7: Human-Readable Metrics — Widths, Precision, and Flags

A status dashboard renders bytes as `1.5 GiB`, ratios as `99.25%`, and IDs as
`00000042`. Getting these to line up and read cleanly is the flag/width/precision
grammar of `fmt` verbs doing exactly what it is for. This exercise builds the
humanizers and pins each verb combination against boundary values.

This module is fully self-contained: its own `go mod init`, code, demo, and tests.

## What you'll build

```text
metricsfmt/                independent module: example.com/metricsfmt
  go.mod                   go 1.24
  metricsfmt.go            HumanBytes; Percent; PaddedID; Signed
  cmd/
    demo/
      main.go              runnable demo: a small metrics panel
  metricsfmt_test.go       per-verb/flag boundary tables + humanizer thresholds
```

- Files: `metricsfmt.go`, `cmd/demo/main.go`, `metricsfmt_test.go`.
- Implement: `HumanBytes(int64) string` (binary units, one decimal); `Percent(ratio float64) string` (`%6.2f%%`); `PaddedID(int) string` (`%08d`); `Signed(int) string` (`%+d`).
- Test: tables pinning exact strings for each verb/flag across boundary values (0, negative, very large, sub-unit); the byte humanizer across KiB/MiB/GiB thresholds; percentage rounds to two decimals; padded IDs keep fixed width.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The grammar behind each formatter

Every verb here is `%[flags][width][.precision]verb`, and each formatter uses one
piece of that grammar deliberately:

- `HumanBytes` divides by 1024 until the value is under 1024, then renders with
  `%.1f` (precision: one digit after the decimal) plus the unit. Below 1024 it
  prints the exact integer byte count with a `B` suffix — no decimal, because
  `512 B` reads better than `0.5 KiB`. The unit ladder is binary (KiB/MiB/GiB),
  matching how memory and disk are actually measured; using SI (1000) here would
  misreport a `1024`-byte page as `1.0 KB` vs the true `1 KiB` and drift by 2.4%
  per step at GiB scale.
- `Percent` multiplies the ratio by 100 and formats with `%6.2f%%`: width 6
  (so values from `0.00` to `100.00` right-align in a fixed column), precision 2
  (two decimals), and `%%` for the literal percent sign that must sit against the
  number. Rounding is IEEE-754 round-to-nearest on the scaled value.
- `PaddedID` uses `%08d`: the `0` flag zero-pads and width 8 fixes the field to
  eight columns, so `42` becomes `00000042` and sorts lexically the same as
  numerically. A value already eight or more digits wide is printed in full (width
  is a *minimum*, never a truncation).
- `Signed` uses `%+d`: the `+` flag forces an explicit sign, so a delta reads
  `+3` / `-3` / `+0` — useful in a diff or a trend column where the sign carries
  meaning.

The one honest caveat is float rounding: `%.2f` rounds the nearest representable
double, so a value whose decimal literal is not exactly representable can round in
the direction the *stored* value points, not the direction the decimal literal
suggests. The tests below use ratios that scale to clean two-decimal values to keep
the golden strings stable; in production, round explicitly if you need
decimal-exact behavior.

Create `metricsfmt.go`:

```go
package metricsfmt

import (
	"fmt"
	"strconv"
)

// HumanBytes renders a byte count with binary units (KiB, MiB, ...) and one
// decimal, or an exact integer with a B suffix below 1024.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	f := float64(n)
	i := -1
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// Percent renders a ratio in [0,1] as a fixed-width two-decimal percentage, e.g.
// 0.9925 becomes " 99.25%". Width 6 keeps a column of values right-aligned.
func Percent(ratio float64) string {
	return fmt.Sprintf("%6.2f%%", ratio*100)
}

// PaddedID renders an integer id zero-padded to eight columns: 42 becomes
// "00000042". Wider ids are printed in full.
func PaddedID(id int) string {
	return fmt.Sprintf("%08d", id)
}

// Signed renders an integer with an explicit sign: +3, -3, +0.
func Signed(n int) string {
	return fmt.Sprintf("%+d", n)
}
```

### The runnable demo

The demo prints a small metrics panel: memory used and total, an error rate, a
request-id, and a trend delta.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metricsfmt"
)

func main() {
	fmt.Printf("mem_used   %s\n", metricsfmt.HumanBytes(1610612736))
	fmt.Printf("mem_total  %s\n", metricsfmt.HumanBytes(1099511627776))
	fmt.Printf("small      %s\n", metricsfmt.HumanBytes(512))
	fmt.Printf("error_rate %s\n", metricsfmt.Percent(0.0333))
	fmt.Printf("success    %s\n", metricsfmt.Percent(0.9925))
	fmt.Printf("request_id %s\n", metricsfmt.PaddedID(42))
	fmt.Printf("trend      %s\n", metricsfmt.Signed(-3))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
mem_used   1.5 GiB
mem_total  1.0 TiB
small      512 B
error_rate   3.33%
success     99.25%
request_id 00000042
trend      -3
```

### Tests

Each formatter gets a table pinning exact output across boundary values. The byte
humanizer table walks the unit thresholds (just under 1024, exactly 1024, and each
higher unit) so an off-by-one in the ladder is caught. The percentage table covers
0, a sub-unit, and 100%. The ID and sign tables cover zero, negative, and
already-wide values.

Create `metricsfmt_test.go`:

```go
package metricsfmt

import (
	"fmt"
	"testing"
)

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{1610612736, "1.5 GiB"},
		{1099511627776, "1.0 TiB"},
		{1125899906842624, "1.0 PiB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if got := HumanBytes(tt.n); got != tt.want {
				t.Fatalf("HumanBytes(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

func TestPercent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ratio float64
		want  string
	}{
		{0, "  0.00%"},
		{0.5, " 50.00%"},
		{0.9925, " 99.25%"},
		{1, "100.00%"},
		{0.0333, "  3.33%"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			got := Percent(tt.ratio)
			if got != tt.want {
				t.Fatalf("Percent(%v) = %q, want %q", tt.ratio, got, tt.want)
			}
			if len(got) != 7 { // width 6 + the % sign
				t.Fatalf("Percent(%v) width = %d, want 7", tt.ratio, len(got))
			}
		})
	}
}

func TestPaddedID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id   int
		want string
	}{
		{0, "00000000"},
		{42, "00000042"},
		{12345678, "12345678"},
		{123456789, "123456789"}, // wider than 8: printed in full, not truncated
	}
	for _, tt := range tests {
		if got := PaddedID(tt.id); got != tt.want {
			t.Fatalf("PaddedID(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestSigned(t *testing.T) {
	t.Parallel()

	tests := []struct {
		n    int
		want string
	}{
		{3, "+3"},
		{-3, "-3"},
		{0, "+0"},
	}
	for _, tt := range tests {
		if got := Signed(tt.n); got != tt.want {
			t.Fatalf("Signed(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func Example() {
	fmt.Println(HumanBytes(1536))
	fmt.Println(Percent(0.9925))
	fmt.Println(PaddedID(42))
	// Output:
	// 1.5 KiB
	//  99.25%
	// 00000042
}
```

## Review

Each formatter is correct when its output matches the golden strings across the
boundary values, and the tests pin exactly that. The byte humanizer's ladder is the
subtle part: below 1024 it must print an exact integer with `B` (not a misleading
`0.5 KiB`), at each threshold it steps to the next binary unit, and `%.1f` fixes one
decimal. `%6.2f%%` is width-6, precision-2, with a literal percent — the `%%` is
the only way to get a `%` glued to the number. `%08d` zero-pads to a fixed eight
columns but never truncates a wider value, and `%+d` forces the sign. The one thing
to stay honest about is float rounding: `%.2f` rounds the stored double, so pick
test values that scale to clean decimals and round explicitly when you need
decimal-exact output. Run `go test -race`; these are pure functions with no shared
state.

## Resources

- [`fmt` package — printing](https://pkg.go.dev/fmt#hdr-Printing) — the flag/width/precision grammar and every verb.
- [`strconv.FormatInt`](https://pkg.go.dev/strconv#FormatInt) — the integer conversion behind the sub-1024 branch.
- [`strconv.FormatFloat`](https://pkg.go.dev/strconv#FormatFloat) — for decimal-exact float rendering when you need it.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-format-string-injection.md](08-format-string-injection.md)
