# Exercise 7: Shrinking a hot in-memory record for cache footprint

A rate-limiter bucket held for every active key, or an order-book level held for
every price, is a struct you keep by the millions — so its size is a capacity
decision. This module takes a bloated bucket (full `time.Time` timestamps, three
scattered `bool`s) and produces a compact version: second-granularity `uint32`
timestamps, three flags packed into one `uint8` bitset, fields ordered by
alignment. It measures the bytes saved and proves the compact form preserves the
data.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test.

## What you'll build

```text
compactrecord/             independent module: example.com/compactrecord
  go.mod                   go 1.26
  bucket.go                NaiveBucket, CompactBucket, bitset flags, Compact/Naive
  cmd/
    demo/
      main.go              prints both sizes and aggregate savings over 1M records
  bucket_test.go           Compact < Naive; bitset round-trip; conversion fuzz test
```

- Files: `bucket.go`, `cmd/demo/main.go`, `bucket_test.go`.
- Implement: a naive bucket with `time.Time` timestamps and scattered `bool`s, a compact bucket with `uint32` seconds and a `uint8` flag bitset ordered by alignment, and `Compact`/`Naive` conversions.
- Test: assert `Sizeof(CompactBucket) < Sizeof(NaiveBucket)`, that the bitset round-trips each flag, and that the conversion is lossless (a fuzz/table round-trip).
- Verify: `go test -count=1 -race ./...`

### What makes the naive record fat, and how the compact one slims it

`NaiveBucket` is written the obvious way: a `string` key, two `time.Time`
timestamps, two `int64` counters, and three separate `bool` flags. On a 64-bit
platform a `time.Time` is 24 bytes (a wall-clock `uint64`, a monotonic/ext
`int64`, and a `*Location` pointer), so two of them are 48 bytes by themselves.
The three trailing `bool`s cost three bytes of data but drag trailing padding.
The whole struct comes to 88 bytes.

Three changes shrink it to 40. First, timestamps: a rate-limiter bucket only
needs second granularity and only spans recent time, so each `time.Time` becomes
a `uint32` of Unix seconds — 4 bytes instead of 24, valid through the year 2106
(the trade-off: you drop sub-second precision, monotonic readings, and the
location, which a bucket does not need). Second, the flags: three `bool`s become
three bits of one `uint8` bitset, with `Set`/`clear`/`has` helpers, so the flag
data is one byte instead of three-plus-padding. Third, ordering: the `string`
(8-aligned) leads, then the `int32`/`uint32` fields (4-aligned), then the single
`uint8`, so nothing pads. Result: 40 bytes, a 48-byte-per-record saving that at a
million buckets is roughly 45 MB of RAM and far fewer cache lines touched on a
sweep.

The conversion must be lossless *at the compact record's granularity*.
`Compact(NaiveBucket)` truncates each timestamp to whole Unix seconds and packs
the flags; `CompactBucket.Naive()` reconstructs a `NaiveBucket` from them. Because
the first compaction normalizes the time to seconds, the round-trip is exact from
then on — `Compact(c.Naive()) == c` — which is precisely what the fuzz test
asserts.

Create `bucket.go`:

```go
// Package compactrecord shrinks a hot rate-limiter bucket from 88 to 40 bytes by
// using uint32 second-timestamps, a uint8 flag bitset, and alignment ordering.
package compactrecord

import "time"

// NaiveBucket is the obvious layout: full time.Time timestamps (24 bytes each on
// 64-bit) and three separate bool flags. On a 64-bit platform it is 88 bytes.
type NaiveBucket struct {
	Key       string
	CreatedAt time.Time
	UpdatedAt time.Time
	Tokens    int64
	Capacity  int64
	Active    bool
	Blocked   bool
	Verified  bool
}

// Flag bits packed into CompactBucket.Flags.
const (
	flagActive uint8 = 1 << iota
	flagBlocked
	flagVerified
)

// CompactBucket is the slim layout: uint32 Unix-second timestamps (valid through
// 2106), int32 counters, and a uint8 flag bitset, ordered largest-align-first.
// On a 64-bit platform it is 40 bytes.
type CompactBucket struct {
	Key       string
	Tokens    int32
	Capacity  int32
	CreatedAt uint32 // Unix seconds
	UpdatedAt uint32 // Unix seconds
	Flags     uint8
}

func (b *CompactBucket) setFlag(mask uint8, on bool) {
	if on {
		b.Flags |= mask
	} else {
		b.Flags &^= mask
	}
}

func (b CompactBucket) hasFlag(mask uint8) bool { return b.Flags&mask != 0 }

// SetActive, SetBlocked, SetVerified toggle the individual flags.
func (b *CompactBucket) SetActive(v bool)   { b.setFlag(flagActive, v) }
func (b *CompactBucket) SetBlocked(v bool)  { b.setFlag(flagBlocked, v) }
func (b *CompactBucket) SetVerified(v bool) { b.setFlag(flagVerified, v) }

// Active, Blocked, Verified report the individual flags.
func (b CompactBucket) Active() bool   { return b.hasFlag(flagActive) }
func (b CompactBucket) Blocked() bool  { return b.hasFlag(flagBlocked) }
func (b CompactBucket) Verified() bool { return b.hasFlag(flagVerified) }

// Compact converts a NaiveBucket to the slim form, truncating timestamps to
// whole Unix seconds and packing the flags into the bitset.
func Compact(n NaiveBucket) CompactBucket {
	c := CompactBucket{
		Key:       n.Key,
		Tokens:    int32(n.Tokens),
		Capacity:  int32(n.Capacity),
		CreatedAt: uint32(n.CreatedAt.Unix()),
		UpdatedAt: uint32(n.UpdatedAt.Unix()),
	}
	c.SetActive(n.Active)
	c.SetBlocked(n.Blocked)
	c.SetVerified(n.Verified)
	return c
}

// Naive reconstructs a NaiveBucket, rebuilding timestamps as UTC second instants.
func (b CompactBucket) Naive() NaiveBucket {
	return NaiveBucket{
		Key:       b.Key,
		CreatedAt: time.Unix(int64(b.CreatedAt), 0).UTC(),
		UpdatedAt: time.Unix(int64(b.UpdatedAt), 0).UTC(),
		Tokens:    int64(b.Tokens),
		Capacity:  int64(b.Capacity),
		Active:    b.Active(),
		Blocked:   b.Blocked(),
		Verified:  b.Verified(),
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unsafe"

	"example.com/compactrecord"
)

func main() {
	naive := unsafe.Sizeof(compactrecord.NaiveBucket{})
	compact := unsafe.Sizeof(compactrecord.CompactBucket{})
	fmt.Printf("NaiveBucket   = %d bytes\n", naive)
	fmt.Printf("CompactBucket = %d bytes\n", compact)
	fmt.Printf("saved %d bytes per record\n", naive-compact)

	const n = 1_000_000
	fmt.Printf("at %d records: %d MB -> %d MB (saved %d MB)\n",
		n, n*naive/(1<<20), n*compact/(1<<20), n*(naive-compact)/(1<<20))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (on a 64-bit platform):

```
NaiveBucket   = 88 bytes
CompactBucket = 40 bytes
saved 48 bytes per record
at 1000000 records: 83 MB -> 38 MB (saved 45 MB)
```

### Tests

The size test pins the strict shrink. The bitset test toggles each flag
independently. The fuzz test asserts the conversion is lossless: any compact
record survives a trip through the naive form and back unchanged (with the flag
input masked to the three defined bits, since only those carry meaning).

Create `bucket_test.go`:

```go
package compactrecord

import (
	"testing"
	"unsafe"
)

func TestCompactIsSmaller(t *testing.T) {
	t.Parallel()

	naive := unsafe.Sizeof(NaiveBucket{})
	compact := unsafe.Sizeof(CompactBucket{})
	if compact >= naive {
		t.Fatalf("CompactBucket = %d not smaller than NaiveBucket = %d", compact, naive)
	}
}

func TestBitsetRoundTrips(t *testing.T) {
	t.Parallel()

	var b CompactBucket
	if b.Active() || b.Blocked() || b.Verified() {
		t.Fatal("zero-value flags should all be false")
	}

	b.SetActive(true)
	b.SetVerified(true)
	if !b.Active() || b.Blocked() || !b.Verified() {
		t.Errorf("after set: active=%v blocked=%v verified=%v, want true false true", b.Active(), b.Blocked(), b.Verified())
	}

	b.SetActive(false)
	if b.Active() {
		t.Error("Active should be false after clear")
	}
	if !b.Verified() {
		t.Error("clearing Active must not disturb Verified")
	}
}

func FuzzCompactRoundTrip(f *testing.F) {
	f.Add("k", int32(10), int32(100), uint32(1_700_000_000), uint32(1_700_000_050), uint8(0b101))
	f.Fuzz(func(t *testing.T, key string, tokens, capacity int32, created, updated uint32, flags uint8) {
		// Only three flag bits carry meaning; mask the rest.
		flags &= flagActive | flagBlocked | flagVerified
		c := CompactBucket{
			Key:       key,
			Tokens:    tokens,
			Capacity:  capacity,
			CreatedAt: created,
			UpdatedAt: updated,
			Flags:     flags,
		}
		if got := Compact(c.Naive()); got != c {
			t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, c)
		}
	})
}
```

## Review

The compact record is correct when it is strictly smaller than the naive one and
the conversion loses nothing at second granularity — `Compact(c.Naive()) == c` for
every compact value, which the fuzz test enforces over random inputs. The savings
are real at scale: 48 bytes per record is roughly 45 MB per million buckets and,
more importantly, more buckets per cache line on a sweep. The trade-offs are
explicit and must be justified in review: `uint32` seconds drop sub-second
precision and cap the range at 2106, `int32` counters cap the token count, and the
bitset trades a little code for the byte. Apply this only to genuinely hot records;
a singleton config struct is not worth the readability cost.

## Resources

- [time.Time internals](https://pkg.go.dev/time#Time) — why a `time.Time` is 24 bytes and what `uint32` seconds give up.
- [unsafe.Sizeof](https://pkg.go.dev/unsafe#Sizeof) — measuring the before/after footprint.
- [Go Fuzzing](https://go.dev/doc/security/fuzz/) — the fuzz-test form used to prove the conversion is lossless.

---

Back to [06-atomic-alignment-32bit.md](06-atomic-alignment-32bit.md) | Next: [08-wire-header-padding-trap.md](08-wire-header-padding-trap.md)
