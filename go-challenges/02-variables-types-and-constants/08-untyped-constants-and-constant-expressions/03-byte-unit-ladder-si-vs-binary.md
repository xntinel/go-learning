# Exercise 3: SI vs Binary Byte Units For Quota Display And Metrics

A quota dashboard shows "1.5 GiB of 10 GiB used"; a billing export shows "1.5 GB".
These are different numbers, and mixing the two ladders silently corrupts quota and
billing math. This module defines both the binary ladder (`KiB = 1 << 10`) and the
SI ladder (`KB = 1000`) as untyped constant expressions, formats a byte count in
each, and enforces a per-tenant GiB quota.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
byteunits/                    independent module: example.com/byteunits
  go.mod                      go 1.26
  units.go                    KiB..TiB (1<<n) and KB..GB (1000^n); FormatBinary, FormatSI,
                              QuotaExceeded
  cmd/
    demo/
      main.go                 formats a few sizes both ways; checks a quota
  units_test.go               ladder values, format strings, quota boundary
```

Files: `units.go`, `cmd/demo/main.go`, `units_test.go`.
Implement: both unit ladders as untyped constants; `FormatBinary(n)` and
`FormatSI(n)` returning a human string; `QuotaExceeded(used, quotaGiB)`.
Test: `1023 -> "1023 B"`, `1024 -> "1.0 KiB"`, SI `1500 -> "1.5 KB"`; quota rejects
at `GiB+1` and accepts at exactly `GiB`; `KiB != KB`.
Verify: `go test -count=1 -race ./...`

### Two ladders, one package, never mixed

The binary ladder steps by 1024 (`1 << 10`, `1 << 20`, ...); the SI ladder steps by
1000. They diverge immediately — `KiB` is 1024, `KB` is 1000 — and the gap
compounds: a `TiB` is about 10% larger than a `TB`. When a metrics pipeline labels
something "KB" but computes it with 1024, or a billing job bills GB but measured
GiB, the numbers silently disagree and the error grows with scale. Defining both
ladders once, as clearly named constant expressions, and choosing the ladder
explicitly at each boundary is how you keep the two from bleeding into each other.

The formatters take an `int64` byte count and return a human string. `FormatBinary`
divides by the largest binary unit that fits and prints one decimal (`float64`
division here is intentional — display, not accounting). Below `KiB` it prints raw
bytes with a `B` suffix, so `1023` stays `"1023 B"` rather than rounding to
`"1.0 KiB"`. `QuotaExceeded` does its comparison entirely in integer `int64` space
— `used > quotaGiB * GiB` — because a quota decision must be exact, never a
floating-point approximation.

Create `units.go`:

```go
package byteunits

import "fmt"

// Binary ladder (IEC): powers of 1024.
const (
	B   = 1
	KiB = 1 << 10
	MiB = 1 << 20
	GiB = 1 << 30
	TiB = 1 << 40
)

// SI ladder: powers of 1000.
const (
	KB = 1000
	MB = 1000 * 1000
	GB = 1000 * 1000 * 1000
)

// FormatBinary renders n using the binary (KiB/MiB/...) ladder with one decimal.
func FormatBinary(n int64) string {
	switch {
	case n >= TiB:
		return fmt.Sprintf("%.1f TiB", float64(n)/float64(TiB))
	case n >= GiB:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// FormatSI renders n using the SI (KB/MB/...) ladder with one decimal.
func FormatSI(n int64) string {
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(GB))
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(MB))
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(KB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// QuotaExceeded reports whether used bytes exceed a quota expressed in GiB. The
// comparison is exact int64 arithmetic; the boundary at exactly the quota is OK.
func QuotaExceeded(used int64, quotaGiB int64) bool {
	return used > quotaGiB*GiB
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/byteunits"
)

func main() {
	sizes := []int64{1023, 1024, 1500, 5 * byteunits.MiB}
	for _, n := range sizes {
		fmt.Printf("%9d  binary=%-10s si=%s\n", n, byteunits.FormatBinary(n), byteunits.FormatSI(n))
	}

	used := int64(byteunits.GiB + 1)
	fmt.Printf("quota 1 GiB, used %d bytes, exceeded=%v\n", used, byteunits.QuotaExceeded(used, 1))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
     1023  binary=1023 B     si=1.0 KB
     1024  binary=1.0 KiB    si=1.0 KB
     1500  binary=1.5 KiB    si=1.5 KB
  5242880  binary=5.0 MiB    si=5.2 MB
quota 1 GiB, used 1073741825 bytes, exceeded=true
```

### Tests

`TestLaddersDiffer` asserts `KiB != KB` and `MiB != MB`, catching an accidental
ladder mixup at compile-time-ish granularity. `TestFormat` is table-driven over the
boundary cases the brief calls out. `TestQuotaBoundary` proves the quota accepts
usage at exactly `GiB` and rejects it at `GiB + 1`.

Create `units_test.go`:

```go
package byteunits

import "testing"

func TestLaddersDiffer(t *testing.T) {
	t.Parallel()
	if KiB == KB {
		t.Fatalf("KiB (%d) must not equal KB (%d)", KiB, KB)
	}
	if MiB == MB {
		t.Fatalf("MiB (%d) must not equal MB (%d)", MiB, MB)
	}
	if KiB != 1024 || KB != 1000 {
		t.Fatalf("ladder values drifted: KiB=%d KB=%d", KiB, KB)
	}
}

func TestFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		n          int64
		binary, si string
	}{
		{"just under KiB", 1023, "1023 B", "1.0 KB"},
		{"exactly KiB", 1024, "1.0 KiB", "1.0 KB"},
		{"1500 bytes", 1500, "1.5 KiB", "1.5 KB"},
		{"five MiB", 5 * MiB, "5.0 MiB", "5.2 MB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := FormatBinary(tt.n); got != tt.binary {
				t.Errorf("FormatBinary(%d) = %q, want %q", tt.n, got, tt.binary)
			}
			if got := FormatSI(tt.n); got != tt.si {
				t.Errorf("FormatSI(%d) = %q, want %q", tt.n, got, tt.si)
			}
		})
	}
}

func TestQuotaBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		used int64
		want bool
	}{
		{GiB - 1, false},
		{GiB, false},
		{GiB + 1, true},
	}
	for _, tt := range tests {
		if got := QuotaExceeded(tt.used, 1); got != tt.want {
			t.Errorf("QuotaExceeded(%d, 1) = %v, want %v", tt.used, got, tt.want)
		}
	}
}
```

## Review

The formatters are correct when each picks the largest unit that fits and renders
one decimal, with sub-`KiB`/`KB` values staying in raw bytes. The quota is correct
when it is pure `int64` arithmetic with an inclusive boundary — using a float there
would let rounding decide a billing question. The mistake this module exists to
prevent is the silent one: labeling a value "KB" while dividing by 1024 (or vice
versa), so `TestLaddersDiffer` is a guard rail that fails loudly if the two ladders
are ever made equal by a careless edit.

## Resources

- [Wikipedia: Binary prefix (KiB/MiB, IEC 80000-13)](https://en.wikipedia.org/wiki/Binary_prefix) — why KiB and KB differ.
- [Go Language Specification: Constant expressions](https://go.dev/ref/spec#Constant_expressions) — compile-time evaluation of the ladders.
- [fmt package](https://pkg.go.dev/fmt#hdr-Printing) — `%.1f` and `%d` verbs used by the formatters.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-retry-budget-durations.md](02-retry-budget-durations.md) | Next: [04-permission-bitmask-iota.md](04-permission-bitmask-iota.md)
