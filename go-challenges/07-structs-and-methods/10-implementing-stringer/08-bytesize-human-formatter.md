# Exercise 8: Human-Readable ByteSize Stringer For Ops And Health Output

Health endpoints, memory metrics, and CLI output all need to turn a raw byte count
into something an operator reads at a glance: `1.5KiB`, `1.0GiB`, not
`1610612736`. This module builds a `ByteSize` type whose `String()` renders binary
(IEC) units with correct rounding, a raw-bytes fallback below 1 KiB, and careful
handling of the band boundaries where rounding would otherwise overflow a unit.

Self-contained module: own `go mod init`, code, demo, and tests.

## What you'll build

```text
bytesize/                   independent module: example.com/bytesize
  go.mod
  bytesize.go               type ByteSize int64; IEC-unit String() with rounding
  cmd/
    demo/
      main.go               prints a range of sizes
  bytesize_test.go          boundary table; negative; band-promotion; idempotence
```

- Files: `bytesize.go`, `cmd/demo/main.go`, `bytesize_test.go`.
- Implement: a `ByteSize int64` whose `String()` prints raw `"NB"` below 1 KiB and otherwise the largest IEC unit with one decimal (`KiB`/`MiB`/`GiB`/`TiB`/`PiB`/`EiB`), promoting to the next unit when rounding would reach 1024.0.
- Test: boundaries `0`, `512`, `1024`, `1536`, `1<<20`, `1<<30`, `1<<40`; negatives; the band-promotion case near 1024 KiB; idempotence of repeated `String()` calls.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/10-implementing-stringer/08-bytesize-human-formatter/cmd/demo
cd go-solutions/07-structs-and-methods/10-implementing-stringer/08-bytesize-human-formatter
```

### The rounding trap that motivates the design

The naive version — divide by the unit, print one decimal — has a subtle bug at
the top of each band. A value one byte below 1 MiB is `1048575` bytes; divided by
`KiB` (1024) that is `1023.999…`, which rounds to `1024.0` and prints `1024.0KiB`.
That "overflows the band": a KiB reading should always be below 1024, or it should
have been a MiB reading. The fix is to compute the value in the chosen unit, and if
rounding it to one decimal reaches `1024.0`, promote to the next-larger unit (where
the same magnitude is `1.0`). So `1048575` bytes renders `1.0MiB`, not `1024.0KiB`.

Below 1 KiB the output is the raw byte count with a `B` suffix (`512B`, `0B`), which
is what operators expect for small sizes — no false precision. Negative sizes carry
the sign through (`-1.5KiB`); a delta between two measurements can be negative, and
a health metric that panics or misprints on a negative is worse than useless.
`String()` is a pure function of the receiver, so it is idempotent: calling it twice
yields the identical string, which matters when the same value is logged and also
rendered in a dashboard.

The units are the binary IEC units (`KiB = 1024`, not `KB = 1000`) because memory
and disk metrics are almost always powers of two; mixing the two is a classic
off-by-2.4% reporting error.

Create `bytesize.go`:

```go
package bytesize

import (
	"math"
	"strconv"
)

// ByteSize is a byte count that formats itself in binary (IEC) units.
type ByteSize int64

// The IEC unit ladder. 1<<(10*n) for n = 1..6.
const (
	_            = iota
	KiB ByteSize = 1 << (10 * iota)
	MiB
	GiB
	TiB
	PiB
	EiB
)

type unit struct {
	size   float64
	suffix string
}

// units ascends so the loop can pick the largest unit that fits.
var units = []unit{
	{float64(KiB), "KiB"},
	{float64(MiB), "MiB"},
	{float64(GiB), "GiB"},
	{float64(TiB), "TiB"},
	{float64(PiB), "PiB"},
	{float64(EiB), "EiB"},
}

// String renders the size in the largest IEC unit that keeps the mantissa under
// 1024, with one decimal; below 1 KiB it prints the raw byte count.
func (b ByteSize) String() string {
	n := int64(b)
	neg := n < 0
	abs := n
	if neg {
		abs = -n
	}

	if abs < int64(KiB) {
		return strconv.FormatInt(n, 10) + "B"
	}

	f := float64(abs)
	// Pick the largest unit whose size is <= f.
	i := 0
	for i < len(units)-1 && f >= units[i+1].size {
		i++
	}
	// If rounding would reach 1024.0, promote to the next unit.
	if round1(f/units[i].size) >= 1024 && i < len(units)-1 {
		i++
	}

	s := strconv.FormatFloat(round1(f/units[i].size), 'f', 1, 64)
	if neg {
		s = "-" + s
	}
	return s + units[i].suffix
}

func round1(x float64) float64 {
	return math.Round(x*10) / 10
}
```

### The runnable demo

The demo prints a spread of sizes so the units, the raw fallback, the rounding, and
the sign are all visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bytesize"
)

func main() {
	sizes := []bytesize.ByteSize{
		0,
		512,
		1024,
		1536,
		1 << 20,
		1234567890,
		1 << 40,
		-1536,
	}
	for _, s := range sizes {
		fmt.Printf("%12d = %s\n", int64(s), s)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
           0 = 0B
         512 = 512B
        1024 = 1.0KiB
        1536 = 1.5KiB
     1048576 = 1.0MiB
  1234567890 = 1.1GiB
1099511627776 = 1.0TiB
       -1536 = -1.5KiB
```

### Tests

The table pins each boundary and the raw-vs-unit transition. `TestBandPromotion`
covers the rounding trap: a value just under 1 MiB must render `1.0MiB`, never
`1024.0KiB`. `TestNegative` checks the sign path. `TestIdempotent` asserts repeated
calls return the identical string, the property a pure `String()` must have.

Create `bytesize_test.go`:

```go
package bytesize

import (
	"fmt"
	"strings"
	"testing"
)

func TestStringBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   ByteSize
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1023, "1023B"},
		{1024, "1.0KiB"},
		{1536, "1.5KiB"},
		{1 << 20, "1.0MiB"},
		{1 << 30, "1.0GiB"},
		{1 << 40, "1.0TiB"},
		{1 << 50, "1.0PiB"},
		{1 << 60, "1.0EiB"},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("ByteSize(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}

func TestBandPromotion(t *testing.T) {
	t.Parallel()
	// One byte below 1 MiB must promote to 1.0MiB, not overflow to 1024.0KiB.
	got := ByteSize(1<<20 - 1).String()
	if got != "1.0MiB" {
		t.Fatalf("ByteSize(1MiB-1).String() = %q, want 1.0MiB", got)
	}
	if strings.HasPrefix(got, "1024") {
		t.Fatalf("unit overflowed its band: %q", got)
	}
}

func TestNegative(t *testing.T) {
	t.Parallel()
	tests := map[ByteSize]string{
		-512:       "-512B",
		-1536:      "-1.5KiB",
		-(1 << 30): "-1.0GiB",
	}
	for in, want := range tests {
		if got := in.String(); got != want {
			t.Errorf("ByteSize(%d).String() = %q, want %q", int64(in), got, want)
		}
	}
}

func TestIdempotent(t *testing.T) {
	t.Parallel()
	b := ByteSize(1234567890)
	first := b.String()
	for range 5 {
		if got := b.String(); got != first {
			t.Fatalf("String() not idempotent: %q vs %q", got, first)
		}
	}
}

func ExampleByteSize_String() {
	fmt.Println(ByteSize(1536), ByteSize(1<<30))
	// Output: 1.5KiB 1.0GiB
}
```

## Review

The formatter is correct when every value lands in the right band with the right
sign and no reading ever reaches 1024 in its own unit. The boundary table is the
main proof; `TestBandPromotion` guards the one non-obvious case, where a value near
the top of a band would round up and must be promoted instead. Keep `String()` a
pure function so it is idempotent — `TestIdempotent` enforces that — because the
same size is often formatted more than once per request. Use IEC (power-of-two)
units for memory and disk; reporting `1.05GB` for what is actually `1.0GiB` is a
real and common metrics bug. Below 1 KiB, show the raw count: false decimal
precision on a 512-byte value only misleads.

## Resources

- [strconv.FormatFloat](https://pkg.go.dev/strconv#FormatFloat) — one-decimal formatting of the mantissa.
- [math.Round](https://pkg.go.dev/math#Round) — round-half-away-from-zero for the mantissa.
- [Wikipedia: Binary prefix (IEC units)](https://en.wikipedia.org/wiki/Binary_prefix) — KiB/MiB/GiB versus the decimal SI prefixes.

---

Back to [07-json-api-enum-field.md](07-json-api-enum-field.md) | Next: [09-go-generate-stringer.md](09-go-generate-stringer.md)
