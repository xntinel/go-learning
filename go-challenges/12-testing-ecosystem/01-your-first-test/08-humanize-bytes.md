# Exercise 8: HumanizeBytes: A String-Returning Unit Test

Logs and metrics render byte counts for humans: `1536` should read as `1.5 KiB`,
not a raw number. The unit is a pure formatter, and testing it reinforces the
string got/want discipline — exact outputs, `%q` messages, one assertion per
rendering case.

## What you'll build

```text
humanbytes/                independent module: example.com/humanbytes
  go.mod
  bytes.go                 func HumanizeBytes(n int64) string
  bytes_test.go            TestHumanizeBytes (discrete %q assertions), ExampleHumanizeBytes
  cmd/
    demo/
      main.go              humanizes a spread of byte counts
```

- Files: `bytes.go`, `bytes_test.go`, `cmd/demo/main.go`.
- Implement: `HumanizeBytes(n int64) string` — `0` yields `"0 B"`, `1536` yields `"1.5 KiB"`, large values in MiB/GiB, binary (1024) units.
- Test: exact formatted strings with `%q` messages across a byte, a fractional KiB, and a GiB.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...`.

### Binary units and the precision choice

Storage and memory are counted in binary multiples: 1 KiB is 1024 bytes, 1 MiB is
1024 KiB, and so on — the IEC `KiB`/`MiB`/`GiB` names, distinct from the decimal
`KB`/`MB` that mean powers of 1000. Mixing the two is a real source of "why does
my 500 GB disk show 465 GiB" confusion, so the formatter is explicit about using
1024.

The algorithm finds the right unit by dividing by 1024 until the remaining
quotient is below 1024, tracking which unit that lands on. Below 1024 bytes there
is no fraction to show, so `n` renders as a plain integer with the `B` suffix
(`0` becomes `"0 B"`, `512` becomes `"512 B"`). At or above 1024, the value is
divided by the unit's divisor and formatted with `strconv.FormatFloat(value, 'f',
1, 64)` — fixed-point, one decimal place. One decimal is the deliberate precision
choice: enough to distinguish `1.5 KiB` from `1.0 KiB` without the noise of
`1.50000 KiB`. `1536 / 1024 = 1.5`, so `1536` renders `"1.5 KiB"`.

The test asserts exact strings, because a formatter's contract *is* its exact
output — a stray space, a wrong suffix, or `2` decimals instead of `1` is a bug a
downstream dashboard will show. Each case (a raw byte count, a fractional KiB, a
whole GiB) is a separate `t.Errorf` with a `%q` message, so every rendering bug
surfaces in one run and the quotes make a trailing-space or suffix difference
visible.

Create `bytes.go`:

```go
package humanbytes

import (
	"fmt"
	"strconv"
)

// HumanizeBytes renders a byte count using binary (1024) IEC units: "0 B",
// "512 B", "1.5 KiB", "3.0 GiB". Values below 1 KiB render as a whole number of
// bytes; larger values use one decimal place.
func HumanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	value := float64(n) / float64(div)
	return strconv.FormatFloat(value, 'f', 1, 64) + " " + units[exp]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/humanbytes"
)

func main() {
	for _, n := range []int64{0, 512, 1536, 1048576, 5368709120} {
		fmt.Printf("%11d -> %s\n", n, humanbytes.HumanizeBytes(n))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
          0 -> 0 B
        512 -> 512 B
       1536 -> 1.5 KiB
    1048576 -> 1.0 MiB
 5368709120 -> 5.0 GiB
```

### The tests

Create `bytes_test.go`:

```go
package humanbytes

import (
	"fmt"
	"testing"
)

func TestHumanizeBytes(t *testing.T) {
	t.Parallel()

	// A raw byte count: no fraction, "B" suffix. %q shows the exact string.
	if got, want := HumanizeBytes(0), "0 B"; got != want {
		t.Errorf("HumanizeBytes(0) = %q, want %q", got, want)
	}
	if got, want := HumanizeBytes(512), "512 B"; got != want {
		t.Errorf("HumanizeBytes(512) = %q, want %q", got, want)
	}

	// A fractional KiB: the one-decimal precision choice.
	if got, want := HumanizeBytes(1536), "1.5 KiB"; got != want {
		t.Errorf("HumanizeBytes(1536) = %q, want %q", got, want)
	}

	// A whole GiB: correct unit selection at scale.
	if got, want := HumanizeBytes(5368709120), "5.0 GiB"; got != want {
		t.Errorf("HumanizeBytes(5368709120) = %q, want %q", got, want)
	}
}

func ExampleHumanizeBytes() {
	fmt.Println(HumanizeBytes(1536))
	// Output: 1.5 KiB
}
```

## Review

The formatter is correct when byte counts below 1024 render as a whole number
with `B`, and larger counts pick the right IEC unit and show exactly one decimal:
`1536` is `"1.5 KiB"`, `5368709120` is `"5.0 GiB"`. The `%q` failure message is
what makes a formatting bug legible — a missing space or a `KB`-versus-`KiB` slip
is obvious inside quotes and invisible without them. The one-decimal
`FormatFloat` precision is a contract, not an accident; change it and every
assertion here breaks, which is the test doing its job. Gate with `gofmt -l .`,
`go vet ./...`, and `go test -count=1 -race ./...`.

## Resources

- [strconv.FormatFloat](https://pkg.go.dev/strconv#FormatFloat) — fixed-point formatting with an explicit precision.
- [Wikipedia: Binary prefix (KiB/MiB/GiB)](https://en.wikipedia.org/wiki/Binary_prefix) — the IEC units versus decimal SI.
- [fmt package](https://pkg.go.dev/fmt) — the `%q` verb for exact string diffs.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-config-apply-defaults.md](07-config-apply-defaults.md) | Next: [09-blackbox-featureflag-eval.md](09-blackbox-featureflag-eval.md)
