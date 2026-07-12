# Exercise 5: Define Human-Readable Byte-Size Limits for Request Bodies and Uploads

Body-size limits are everywhere in backend code — the value you pass to
`http.MaxBytesReader`, the ceiling on an upload, the buffer you refuse to grow
past. Writing them as `4194304` is unreadable and error-prone. This module
builds a size-limit config with the classic `1 << (10 * iota)` unit ladder,
correctly typed as `int64` so a multi-gigabyte limit survives a 32-bit target.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
sizelimit/                      module: example.com/sizelimit
  go.mod                        go 1.26
  sizelimit.go                  KiB/MiB/GiB units, MaxRequestBody/MaxUploadSize, FormatBytes
  cmd/
    demo/
      main.go                   prints the limits and a few formatted sizes
  sizelimit_test.go             unit values, limit values, FormatBytes table, no overflow
```

Files: `sizelimit.go`, `cmd/demo/main.go`, `sizelimit_test.go`.
Implement: `KiB`, `MiB`, `GiB` via `1 << (10 * iota)`; `MaxRequestBody` and `MaxUploadSize` as `int64` limits; a `FormatBytes` helper.
Test: `KiB == 1024`, `MiB == 1048576`, `GiB == 1073741824`; `MaxRequestBody` equals its intended multiple; a `FormatBytes` table; and a `> 2 GiB` limit does not overflow.
Verify: `go test -count=1 ./...`

## Why the shift idiom, and why int64

The unit ladder is the canonical `iota` shift pattern:

```go
const (
	_          = iota            // discard the iota==0 slot
	KiB int64 = 1 << (10 * iota) // iota==1: 1<<10 = 1024
	MiB                          // iota==2: 1<<20 = 1048576
	GiB                          // iota==3: 1<<30 = 1073741824
)
```

The blank identifier `_` throws away the `iota == 0` slot, because `1 << 0 == 1`
byte is not a unit anyone wants. From the second line, the expression `int64 =
1 << (10 * iota)` is implicitly repeated for `MiB` and `GiB`, each with the next
`iota`, giving the powers of `1024` exactly. Two correctness points:

First, use binary `1024`, not decimal `1000`. A KiB is `2^10`; a limit written
against `1000` is silently 2.4% off at the gigabyte scale, which matters when a
client's "1 GiB" upload is rejected 24 MiB early.

Second — and this is the failure mode the type annotation prevents — declare the
units as `int64`, not the default `int`. On a 64-bit build `int` is 64 bits and
it does not matter, but the moment the code is cross-compiled for a 32-bit target
(an ARM edge device, a WASM build, a legacy runtime), `int` is 32 bits and any
limit at or above `2 GiB` overflows to a negative number. `MaxUploadSize = 5 *
GiB` would wrap. Pinning the constants to `int64` makes them correct on every
target. Note that the *arithmetic* `1 << 30` is computed with arbitrary precision
at compile time regardless — it is the *type of the destination* that must hold
the result, which is why the annotation goes on the constant.

`MaxRequestBody` and `MaxUploadSize` compose from the units, staying `int64`.
`FormatBytes` renders a byte count for logs, choosing the largest unit that keeps
the number at least 1.

Create `sizelimit.go`:

```go
package sizelimit

import "fmt"

// Binary size units. The blank slot discards iota==0 (1<<0 == 1 byte); the
// int64 type keeps multi-gigabyte limits correct on 32-bit targets.
const (
	_         = iota
	KiB int64 = 1 << (10 * iota)
	MiB
	GiB
)

// Body-size limits, suitable for http.MaxBytesReader and upload guards.
const (
	MaxRequestBody int64 = 4 * MiB
	MaxUploadSize  int64 = 5 * GiB
)

// FormatBytes renders n using the largest binary unit that keeps it >= 1.
func FormatBytes(n int64) string {
	switch {
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
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sizelimit"
)

func main() {
	fmt.Printf("KiB=%d MiB=%d GiB=%d\n", sizelimit.KiB, sizelimit.MiB, sizelimit.GiB)
	fmt.Printf("max request body: %s\n", sizelimit.FormatBytes(sizelimit.MaxRequestBody))
	fmt.Printf("max upload size:  %s\n", sizelimit.FormatBytes(sizelimit.MaxUploadSize))
	fmt.Printf("1536 bytes -> %s\n", sizelimit.FormatBytes(1536))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
KiB=1024 MiB=1048576 GiB=1073741824
max request body: 4.0 MiB
max upload size:  5.0 GiB
1536 bytes -> 1.5 KiB
```

## Tests

`TestUnitValues` pins the exact byte counts. `TestLimits` checks the composed
limits. `TestFormatBytes` is a table across unit boundaries. `TestNoOverflow`
asserts `MaxUploadSize` is positive and larger than `2 GiB` — the regression
guard against the `int` overflow that a wrong type would introduce.

Create `sizelimit_test.go`:

```go
package sizelimit

import (
	"testing"
)

func TestUnitValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  int64
		want int64
	}{
		{"KiB", KiB, 1024},
		{"MiB", MiB, 1048576},
		{"GiB", GiB, 1073741824},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Fatalf("%s = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

func TestLimits(t *testing.T) {
	t.Parallel()

	if MaxRequestBody != 4*MiB {
		t.Fatalf("MaxRequestBody = %d, want %d", MaxRequestBody, 4*MiB)
	}
	if MaxUploadSize != 5*GiB {
		t.Fatalf("MaxUploadSize = %d, want %d", MaxUploadSize, 5*GiB)
	}
}

func TestFormatBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{1536, "1.5 KiB"},
		{4 * MiB, "4.0 MiB"},
		{1610612736, "1.5 GiB"},
	}
	for _, tt := range tests {
		if got := FormatBytes(tt.n); got != tt.want {
			t.Fatalf("FormatBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestNoOverflow(t *testing.T) {
	t.Parallel()

	// A > 2 GiB limit must stay positive; it would wrap negative if the
	// constants were typed int on a 32-bit target.
	if MaxUploadSize <= 0 {
		t.Fatalf("MaxUploadSize overflowed: %d", MaxUploadSize)
	}
	if MaxUploadSize <= 2*GiB {
		t.Fatalf("MaxUploadSize = %d, want > 2 GiB", MaxUploadSize)
	}
}
```

## Review

The limits are correct when each unit is an exact power of `1024` and every
constant is `int64`. The overflow guard is not busywork: it is the difference
between a limit that works and one that silently wraps negative on a 32-bit
build, causing `http.MaxBytesReader` to reject every request. `FormatBytes`
mirrors the ladder in reverse for readable logs. If a value looks off by a few
percent, check that you used `1024`, not `1000`.

## Resources

- [Go Specification: Iota](https://go.dev/ref/spec#Iota)
- [Go Specification: Constants](https://go.dev/ref/spec#Constants)
- [net/http: MaxBytesReader](https://pkg.go.dev/net/http#MaxBytesReader)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-rbac-bitmask-permission-set.md](04-rbac-bitmask-permission-set.md) | Next: [06-retry-backoff-duration-constants.md](06-retry-backoff-duration-constants.md)
