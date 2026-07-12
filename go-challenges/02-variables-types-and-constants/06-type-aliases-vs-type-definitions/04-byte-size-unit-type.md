# Exercise 4: A Duration-Style Unit Type for Byte Sizes in Config

Config files express size limits in human units — an 8MB upload cap, a 512KB
avatar limit — but your handlers need an exact byte count. This exercise builds
`type ByteSize int64` modeled on `time.Duration`: `iota`-shifted typed constants
(`KB`, `MB`, `GB`), a `String()` that renders human-readable sizes, and
`ParseByteSize("10MB")` for loading config. The result is a self-documenting,
mixing-proof primitive.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
bytesize/                 independent module: example.com/bytesize
  go.mod                  go 1.24
  bytesize.go             type ByteSize int64; B/KB/MB/GB/TB constants;
                          String; ParseByteSize; ErrInvalidSize
  cmd/
    demo/
      main.go             loads two config limits, prints bytes and human form
  bytesize_test.go        round-trip, suffix parsing, rejection, boundary tests
```

- Files: `bytesize.go`, `cmd/demo/main.go`, `bytesize_test.go`.
- Implement: `ByteSize` with `iota`-shifted constants `B`,`KB`,`MB`,`GB`,`TB`, a `String()` method, and `ParseByteSize(s string) (ByteSize, error)`.
- Test: `ParseByteSize` then `String` round-trips; suffixes parse case-insensitively; bad input is rejected via a wrapped sentinel; boundary values at exact powers of two.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The time.Duration pattern applied to bytes

`time.Duration` is the archetype of a numeric unit as a defined type: a
`type Duration int64` counting nanoseconds, with typed constants (`time.Second`),
a `String()` that prints `1.5s`, and `time.ParseDuration` as the parse boundary.
`ByteSize` copies that shape. The constants are declared with the classic
`iota`-shift idiom, where each successive line multiplies the shift by 1024:

```go
const (
	B  ByteSize = 1 << (10 * iota) // iota 0 -> 1
	KB                             // iota 1 -> 1<<10 = 1024
	MB                             // iota 2 -> 1<<20
	GB                             // iota 3 -> 1<<30
	TB                             // iota 4 -> 1<<40
)
```

Each constant is a *typed* `ByteSize`, so `5 * MB` is a `ByteSize` (untyped `5`
adopts the operand's type), and `String()` and every method are available on the
result. Because `ByteSize` is defined, not aliased, you cannot pass a raw `int64`
byte count where a `ByteSize` is wanted without a visible conversion — the same
mixing protection `time.Duration` gives you against passing a bare nanosecond
count.

`String()` walks from the largest unit down and renders with
`strconv.FormatFloat(f, 'g', -1, 64)`, which prints the shortest exact decimal
(so `10*MB` is `"10MB"`, `ByteSize(1536*1024)` is `"1.5MB"`). `ParseByteSize`
uppercases the input, matches the longest suffix first (so `KB` wins over a bare
`B`), and multiplies the numeric part by the unit. It deliberately *requires* a
suffix: a bare `"512"` is rejected, because a config value with no unit is
ambiguous and should be a caller error, not a silent "512 bytes".

Create `bytesize.go`:

```go
package bytesize

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidSize is the sentinel returned when a size string cannot be parsed.
var ErrInvalidSize = errors.New("invalid byte size")

// ByteSize is a byte count as a DEFINED integer type, so it cannot be mixed with
// a raw int64 without an explicit conversion. It is modeled on time.Duration.
type ByteSize int64

const (
	B  ByteSize = 1 << (10 * iota) // 1 byte
	KB                             // 1024 bytes
	MB                             // 1048576 bytes
	GB                             // 1073741824 bytes
	TB                             // 1099511627776 bytes
)

// String renders the size using the largest unit that keeps the number >= 1,
// printing the shortest exact decimal (e.g. "10MB", "1.5GB", "512B").
func (b ByteSize) String() string {
	switch {
	case b >= TB:
		return format(b, TB, "TB")
	case b >= GB:
		return format(b, GB, "GB")
	case b >= MB:
		return format(b, MB, "MB")
	case b >= KB:
		return format(b, KB, "KB")
	default:
		return strconv.FormatInt(int64(b), 10) + "B"
	}
}

func format(b, unit ByteSize, suffix string) string {
	return strconv.FormatFloat(float64(b)/float64(unit), 'g', -1, 64) + suffix
}

// ParseByteSize converts a human string like "10MB" or "1.5GB" into a ByteSize.
// Suffix matching is case-insensitive; a unit suffix is required. Anything else
// is rejected with an error wrapping ErrInvalidSize.
func ParseByteSize(s string) (ByteSize, error) {
	up := strings.ToUpper(strings.TrimSpace(s))

	units := []struct {
		suffix string
		unit   ByteSize
	}{
		{"TB", TB}, {"GB", GB}, {"MB", MB}, {"KB", KB}, {"B", B},
	}
	for _, u := range units {
		if !strings.HasSuffix(up, u.suffix) {
			continue
		}
		num := strings.TrimSpace(up[:len(up)-len(u.suffix)])
		f, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, fmt.Errorf("parse size %q: %w", s, ErrInvalidSize)
		}
		return ByteSize(f * float64(u.unit)), nil
	}
	return 0, fmt.Errorf("parse size %q: %w", s, ErrInvalidSize)
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bytesize"
)

func main() {
	uploadLimit, err := bytesize.ParseByteSize("8MB")
	if err != nil {
		panic(err)
	}
	avatarLimit, err := bytesize.ParseByteSize("512KB")
	if err != nil {
		panic(err)
	}

	// Typed constant arithmetic stays in ByteSize.
	buffer := 2 * bytesize.MB

	fmt.Printf("upload limit: %d bytes (%s)\n", int64(uploadLimit), uploadLimit)
	fmt.Printf("avatar limit: %d bytes (%s)\n", int64(avatarLimit), avatarLimit)
	fmt.Printf("read buffer:  %d bytes (%s)\n", int64(buffer), buffer)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
upload limit: 8388608 bytes (8MB)
avatar limit: 524288 bytes (512KB)
read buffer:  2097152 bytes (2MB)
```

### Tests

The tests round-trip `ParseByteSize` through `String` and back, check
case-insensitive suffix handling, reject malformed and unit-less input via the
wrapped sentinel, and pin the boundary values at exact powers of two.

Create `bytesize_test.go`:

```go
package bytesize

import (
	"errors"
	"fmt"
	"testing"
)

func TestConstants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		got  ByteSize
		want int64
	}{
		{B, 1},
		{KB, 1024},
		{MB, 1048576},
		{GB, 1073741824},
		{TB, 1099511627776},
		{5 * MB, 5 * 1048576}, // typed arithmetic stays in ByteSize
	}
	for _, tc := range tests {
		if int64(tc.got) != tc.want {
			t.Errorf("constant = %d, want %d", int64(tc.got), tc.want)
		}
	}
}

func TestString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   ByteSize
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{KB, "1KB"},
		{10 * MB, "10MB"},
		{1536 * KB, "1.5MB"},
		{2 * GB, "2GB"},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("ByteSize(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}

func TestParseByteSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    ByteSize
		wantErr bool
	}{
		{"1B", B, false},
		{"1KB", KB, false},
		{"10MB", 10 * MB, false},
		{"8mb", 8 * MB, false},      // case-insensitive
		{"1.5GB", 1536 * MB, false}, // fractional
		{"  4 KB ", 4 * KB, false},  // surrounding and inner space
		{"512", 0, true},            // unit required
		{"bad", 0, true},            // not a number
		{"", 0, true},               // empty
		{"MB", 0, true},             // no number
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseByteSize(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidSize) {
					t.Fatalf("ParseByteSize(%q) err = %v, want ErrInvalidSize", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseByteSize(%q) unexpected err: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseByteSize(%q) = %d, want %d", tc.in, int64(got), int64(tc.want))
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"512B", "1KB", "10MB", "1.5GB", "2TB"} {
		v, err := ParseByteSize(s)
		if err != nil {
			t.Fatalf("ParseByteSize(%q): %v", s, err)
		}
		back, err := ParseByteSize(v.String())
		if err != nil {
			t.Fatalf("re-parse %q: %v", v.String(), err)
		}
		if back != v {
			t.Errorf("round-trip %q: got %d, want %d", s, int64(back), int64(v))
		}
	}
}

func ExampleByteSize_String() {
	fmt.Println((10 * MB).String())
	// Output: 10MB
}
```

## Review

The type is correct when the constants are exact powers of 1024, `String` prints
the shortest exact decimal with the right unit, and `ParseByteSize` requires a
suffix and round-trips through `String`. The subtle bug to avoid is the `iota`
offset: giving `B` a separate explicit value and then reusing an `iota`-shift
expression on the next line shifts every unit up by one power, so `KB` silently
becomes a megabyte — declare all five with a single `1 << (10 * iota)` expression.
The second trap is matching a bare `"B"` suffix before `"KB"`/`"MB"`; check the
two-letter suffixes first. Because `ByteSize` is defined, `rawInt64Count + MB` will
not compile, which is the mixing protection you wanted.

## Resources

- [`time.Duration`](https://pkg.go.dev/time#Duration) — the defined-numeric-unit pattern this exercise imitates.
- [Effective Go: the `iota` constant generator](https://go.dev/doc/effective_go#constants) — the shift idiom for unit constants.
- [`strconv.FormatFloat`](https://pkg.go.dev/strconv#FormatFloat) — the `'g'` verb that prints the shortest exact decimal.

---

Prev: [03-public-api-package-migration-alias.md](03-public-api-package-migration-alias.md) | Next: [05-header-canonicalization-defined-map.md](05-header-canonicalization-defined-map.md)
