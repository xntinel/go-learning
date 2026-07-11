# Exercise 2: Truncate to a Byte Budget Without Splitting a Rune

Storage layers speak bytes: a Postgres `varchar` byte cap, a DynamoDB 400 KB item
limit, a fixed-width binary field, a queue message size. Naively slicing `s[:max]`
to fit the budget splits a multi-byte rune and produces invalid UTF-8 that breaks
the next hop. This module builds the truncation that fits a byte budget while
always landing on a rune boundary.

## What you'll build

```text
truncate/                  independent module: example.com/truncate
  go.mod                   go 1.26
  truncate.go              TruncateBytes, TruncateRunes
  cmd/
    demo/
      main.go              shows a mid-rune cutoff backing off cleanly
  truncate_test.go         validity invariants + adversarial cutoffs + property test
```

Files: `truncate.go`, `cmd/demo/main.go`, `truncate_test.go`.
Implement: `TruncateBytes(s string, max int) string`, `TruncateRunes(s string, maxRunes int) string`.
Test: result is valid UTF-8 and within budget for mid-rune cutoffs; already-fits returns identity; property test over random UTF-8.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/truncate/cmd/demo
cd ~/go-exercises/truncate
go mod init example.com/truncate
```

### The algorithm and why it is correct

The contract of `TruncateBytes(s, max)` is: return the longest prefix of `s` that is
valid UTF-8 and at most `max` bytes. If `s` already fits, return it unchanged (same
backing string, no copy). Otherwise the cut point `max` may land inside a multi-byte
rune, and the job is to back off to the nearest rune boundary at or before `max`.

The mechanism is `utf8.RuneStart(b)`, which reports whether a byte can be the first
byte of an encoded rune — true for ASCII bytes and for UTF-8 lead bytes, false for
continuation bytes (those of the form `10xxxxxx`). Two cases:

- If `s[max]` is a rune start, then the byte just before `max` ended a rune exactly,
  so `s[:max]` is already a clean boundary — return it.
- Otherwise `s[max]` is a continuation byte, meaning a rune that began before `max`
  runs past it. Scan backward from `max-1` to the first rune-start byte and cut
  there, dropping the partial rune entirely. Because any rune is at most
  `utf8.UTFMax` (4) bytes, this scan backs off at most three bytes.

Dropping the whole partial rune — rather than keeping some of its bytes — is what
guarantees validity: the prefix ends on a boundary, so it decodes cleanly. The cost
is that the result can be a few bytes under `max`, which is exactly right: a byte
budget is a maximum, not a target.

`TruncateRunes(s, maxRunes)` is the character-count cap: keep at most `maxRunes`
code points. Because the range index `i` is the byte offset of each rune's start,
the byte position at which the `maxRunes`-th rune begins is a valid slice bound —
`s[:i]` keeps exactly `maxRunes` runes and is valid UTF-8.

Create `truncate.go`:

```go
// truncate.go
package truncate

import "unicode/utf8"

// TruncateBytes returns the longest prefix of s that is valid UTF-8 and at most
// max bytes, never splitting a multi-byte rune. If s already fits, s is returned
// unchanged. A non-positive max yields "".
func TruncateBytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	// s[max] indexes the byte just past the budget. If it starts a rune, the
	// prefix s[:max] ends exactly on a boundary and is clean.
	if utf8.RuneStart(s[max]) {
		return s[:max]
	}
	// Otherwise a rune straddles the cut. Back off to the last rune-start byte
	// at or before max and drop the partial rune whole.
	for i := max - 1; i >= 0; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i]
		}
	}
	return ""
}

// TruncateRunes returns the prefix of s containing at most maxRunes code points.
// If s has no more than maxRunes runes it is returned unchanged; a non-positive
// maxRunes yields "".
func TruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == maxRunes {
			return s[:i]
		}
		count++
	}
	return s
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"unicode/utf8"

	"example.com/truncate"
)

func main() {
	const s = "café" // 5 bytes: c a f then é (0xC3 0xA9)

	naive := s[:4] // splits é: ends in a lone 0xC3 lead byte
	safe := truncate.TruncateBytes(s, 4)

	fmt.Printf("naive s[:4]        valid=%v len=%d\n", utf8.ValidString(naive), len(naive))
	fmt.Printf("TruncateBytes(s,4) = %q valid=%v len=%d\n", safe, utf8.ValidString(safe), len(safe))
	fmt.Printf("TruncateBytes(中中,5) = %q\n", truncate.TruncateBytes("中中", 5))
	fmt.Printf("TruncateRunes(café,2) = %q\n", truncate.TruncateRunes("café", 2))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
naive s[:4]        valid=false len=4
TruncateBytes(s,4) = "caf" valid=true len=3
TruncateBytes(中中,5) = "中"
TruncateRunes(café,2) = "ca"
```

### Tests

The tests assert the two invariants that make the function safe — the result never
exceeds the budget and is always valid UTF-8 — across adversarial cutoffs that land
mid-rune, plus a property test that hammers both invariants over random UTF-8 so a
boundary case the table missed still fails loudly.

Create `truncate_test.go`:

```go
// truncate_test.go
package truncate

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesInvariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"fits already", "hello", 10, "hello"},
		{"exact fit", "hello", 5, "hello"},
		{"ascii cut on boundary", "hello", 3, "hel"},
		{"mid-rune backs off", "café", 4, "caf"},
		{"drops whole 2-byte rune", "café", 3, "caf"},
		{"keeps first cjk drops second", "中中", 5, "中"},
		{"single cjk too big for budget", "中", 2, ""},
		{"zero budget", "café", 0, ""},
		{"negative budget", "café", -1, ""},
	}
	for _, tc := range cases {
		got := TruncateBytes(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("%s: TruncateBytes(%q,%d) = %q, want %q", tc.name, tc.in, tc.max, got, tc.want)
		}
		if len(got) > tc.max && tc.max > 0 {
			t.Errorf("%s: len=%d exceeds max=%d", tc.name, len(got), tc.max)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: result %q is not valid UTF-8", tc.name, got)
		}
	}
}

func TestTruncateBytesReturnsIdentityWhenItFits(t *testing.T) {
	t.Parallel()
	const s = "café 中文"
	got := TruncateBytes(s, len(s))
	if got != s {
		t.Errorf("TruncateBytes(s, len(s)) = %q, want identity %q", got, s)
	}
}

func TestTruncateRunes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		maxRunes int
		want     string
	}{
		{"café", 2, "ca"},
		{"café", 4, "café"},
		{"café", 9, "café"},
		{"中文字", 2, "中文"},
		{"anything", 0, ""},
	}
	for _, tc := range cases {
		got := TruncateRunes(tc.in, tc.maxRunes)
		if got != tc.want {
			t.Errorf("TruncateRunes(%q,%d) = %q, want %q", tc.in, tc.maxRunes, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("TruncateRunes(%q,%d) produced invalid UTF-8", tc.in, tc.maxRunes)
		}
	}
}

func TestTruncateBytesProperty(t *testing.T) {
	t.Parallel()
	// Palette mixing 1-, 2-, and 3-byte runes so cut points routinely land
	// mid-rune.
	palette := []rune{'a', 'é', '€', '中', ' ', '9'}
	for iter := 0; iter < 2000; iter++ {
		var runes []rune
		n := rand.IntN(12)
		for range n {
			runes = append(runes, palette[rand.IntN(len(palette))])
		}
		s := string(runes)
		max := rand.IntN(len(s) + 3) // may exceed len(s)
		got := TruncateBytes(s, max)
		if len(got) > max {
			t.Fatalf("len(TruncateBytes(%q,%d))=%d exceeds max", s, max, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("TruncateBytes(%q,%d)=%q is not valid UTF-8", s, max, got)
		}
	}
}

func ExampleTruncateBytes() {
	// "café" is 5 bytes; a 4-byte budget would split é, so we get 3 bytes.
	safe := TruncateBytes("café", 4)
	fmt.Println(safe, utf8.ValidString(safe))
	// Output: caf true
}
```

## Review

The function is correct when both invariants hold for every input: `len(result) <=
max` and `utf8.ValidString(result)`. The property test is the real proof — it feeds
random mixes of 1-, 2-, and 3-byte runes with random budgets so cut points land
mid-rune constantly, and any prefix that violates either invariant fails
immediately. The mistake to avoid is the seductive one-liner `s[:max]`: it satisfies
the byte budget and produces invalid UTF-8, which the next JSON encode or database
insert turns into corruption or an error. Note that backing off to a rune boundary
means the result can be a couple of bytes under budget — correct, because a byte cap
is a ceiling. `TruncateBytes` assumes valid UTF-8 input; when the source is
untrusted, validate or repair first (the next two modules).

## Resources

- [unicode/utf8: RuneStart, UTFMax, ValidString](https://pkg.go.dev/unicode/utf8) — the boundary test and the 4-byte bound on backoff.
- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings) — why byte slicing splits runes.
- [Amazon DynamoDB item and attribute limits](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/ServiceQuotasReference.html) — a real byte-measured store your truncation feeds.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-formcount-bytes-runes-stats.md](01-formcount-bytes-runes-stats.md) | Next: [03-utf8-input-validation-reject-invalid.md](03-utf8-input-validation-reject-invalid.md)
