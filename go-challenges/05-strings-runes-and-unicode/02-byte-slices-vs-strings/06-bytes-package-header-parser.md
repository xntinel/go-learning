# Exercise 6: Allocation-Free HTTP Header Parsing with the bytes Package

A proxy parses a raw header block off the wire before deciding how to route a
request. Converting the buffer to a string to parse it would allocate on every
request. This exercise parses directly on `[]byte` using the `bytes` package —
which mirrors `strings` function for function — so the scan does zero string
allocation, converting to `string` only for the header names and values the caller
actually keeps.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
hdrparse/                   independent module: example.com/hdrparse
  go.mod                    go 1.25
  hdrparse.go               Header; ParseBlock, (Headers).Get (case-insensitive), scanNoAlloc
  cmd/
    demo/
      main.go               parses a raw header block and looks up fields
  hdrparse_test.go          parse table, case-insensitive lookup, zero-alloc scan
```

- Files: `hdrparse.go`, `cmd/demo/main.go`, `hdrparse_test.go`.
- Implement: `ParseBlock([]byte) Headers` splitting a raw block into name/value pairs with `bytes.Cut` and `bytes.TrimSpace`, and a `Get(name)` that matches case-insensitively with `bytes.EqualFold`; plus a `scanNoAlloc` that counts fields without allocating a string.
- Test: mixed-case names, leading/trailing spaces, values containing colons, folded whitespace, empty lines; case-insensitive lookup; an `AllocsPerRun` assertion that the scan performs zero string allocations.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/hdrparse/cmd/demo
cd ~/go-exercises/hdrparse
go mod init example.com/hdrparse
go mod edit -go=1.25
```

### The bytes package is the strings package, for buffers

The data you get off a socket or out of a file is `[]byte`. If your first move is
`string(buf)` so you can use `strings.Split`, `strings.TrimSpace`, and
`strings.EqualFold`, you have paid a copy of the whole buffer before parsing a
single field. You do not need to: `bytes` mirrors `strings` one-to-one —
`bytes.Cut`, `bytes.TrimSpace`, `bytes.EqualFold`, `bytes.ToLower`, `bytes.Index`,
`bytes.Split` have identical semantics to their `strings` twins but take and return
`[]byte`. Read-only parsing of wire data stays entirely on `[]byte`, and you convert
to `string` only for the few tokens you keep.

`ParseBlock` splits the block into lines on `\n`, and each line into a name and
value on the first `:` with `bytes.Cut` — the first colon, so a value that itself
contains colons (a timestamp, a URL) is preserved intact, matching HTTP semantics.
It trims each side with `bytes.TrimSpace` to drop the optional whitespace around the
value, and skips empty lines (the blank line that terminates a header block). Only
at the point of storing a kept field does it convert to `string`, which is the copy
and the ownership boundary.

Lookup is case-insensitive because HTTP field names are: `Content-Type`,
`content-type`, and `CONTENT-TYPE` are the same header. `Get` compares with
`bytes.EqualFold` when scanning the raw pairs, or against the stored names — folding
avoids both a `ToLower` allocation and a case-sensitivity bug. This mirrors what
`net/textproto` and `net/http.Header` do (they canonicalize names via
`textproto.CanonicalMIMEHeaderKey`); here we fold at lookup instead of canonicalizing
at insert, which keeps the parse allocation-free for the common "parse then look up
one or two fields" path.

`scanNoAlloc` is the proof object: it walks the block counting fields using only
`bytes` operations and never converts to `string`, so `AllocsPerRun` can pin it at
zero. It is the shape of a hot-path scanner that decides routing from a couple of
fields without materializing the rest.

Create `hdrparse.go`:

```go
package hdrparse

import "bytes"

// Header is one parsed field. Name and Value are owned strings.
type Header struct {
	Name  string
	Value string
}

// Headers is a parsed header block preserving order and duplicates.
type Headers []Header

// ParseBlock parses a raw header block (lines separated by \n, name and value
// separated by the first colon) into Headers. Empty lines are skipped. Scanning
// stays on []byte; only kept names and values are converted to string.
func ParseBlock(block []byte) Headers {
	var hs Headers
	for len(block) > 0 {
		var line []byte
		line, block, _ = bytes.Cut(block, []byte("\n"))
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		name, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			continue
		}
		hs = append(hs, Header{
			Name:  string(bytes.TrimSpace(name)),
			Value: string(bytes.TrimSpace(value)),
		})
	}
	return hs
}

// Get returns the value of the first header whose name matches name
// case-insensitively, and whether it was found.
func (hs Headers) Get(name string) (string, bool) {
	want := []byte(name)
	for _, h := range hs {
		if bytes.EqualFold([]byte(h.Name), want) {
			return h.Value, true
		}
	}
	return "", false
}

// scanNoAlloc counts non-empty header lines using only byte-indexed operations,
// without ever converting to string OR allocating a separator slice. It exists to
// demonstrate an allocation-free scan (see TestScanZeroAlloc).
func scanNoAlloc(block []byte) int {
	n := 0
	rest := block
	for len(rest) > 0 {
		var line []byte
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			line, rest = rest, nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if bytes.IndexByte(line, ':') >= 0 {
			n++
		}
	}
	return n
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/hdrparse"
)

func main() {
	raw := []byte(
		"Host: example.com\r\n" +
			"Content-Type:  application/json \r\n" +
			"X-Request-Start: t=1700000000:123\r\n" +
			"\r\n",
	)
	hs := hdrparse.ParseBlock(raw)
	for _, h := range hs {
		fmt.Printf("%s = %q\n", h.Name, h.Value)
	}

	if v, ok := hs.Get("content-type"); ok {
		fmt.Printf("lookup content-type -> %s\n", v)
	}
	if _, ok := hs.Get("authorization"); !ok {
		fmt.Println("lookup authorization -> absent")
	}
}
```

Note the raw block uses `\r\n` line endings; `bytes.TrimSpace` strips the trailing
`\r`, so the parser handles CRLF and LF alike.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Host = "example.com"
Content-Type = "application/json"
X-Request-Start = "t=1700000000:123"
lookup content-type -> application/json
lookup authorization -> absent
```

### Tests

`TestParseBlock` drives the table: mixed-case names, leading/trailing spaces, a
value containing colons, CRLF endings, and empty lines. `TestCaseInsensitiveGet`
proves `EqualFold` lookup finds a header regardless of the case used to query.
`TestScanZeroAlloc` pins the operational claim — the `[]byte`-only scan performs zero
allocations, which converting to `string` first could not.

Create `hdrparse_test.go`:

```go
package hdrparse

import "testing"

func TestParseBlock(t *testing.T) {
	t.Parallel()
	raw := []byte(
		"Host: example.com\r\n" +
			"content-type:  application/json \r\n" +
			"X-Trace: a:b:c\r\n" +
			"\r\n" +
			"Malformed line without colon\r\n" +
			"X-Empty:\r\n",
	)
	hs := ParseBlock(raw)
	want := []Header{
		{"Host", "example.com"},
		{"content-type", "application/json"},
		{"X-Trace", "a:b:c"},
		{"X-Empty", ""},
	}
	if len(hs) != len(want) {
		t.Fatalf("got %d headers, want %d: %+v", len(hs), len(want), hs)
	}
	for i := range want {
		if hs[i] != want[i] {
			t.Fatalf("header %d = %+v, want %+v", i, hs[i], want[i])
		}
	}
}

func TestCaseInsensitiveGet(t *testing.T) {
	t.Parallel()
	hs := ParseBlock([]byte("Content-Type: text/plain\nX-Api-Key: abc\n"))
	for _, q := range []string{"content-type", "CONTENT-TYPE", "Content-Type"} {
		v, ok := hs.Get(q)
		if !ok || v != "text/plain" {
			t.Fatalf("Get(%q) = %q,%v; want text/plain,true", q, v, ok)
		}
	}
	if _, ok := hs.Get("missing"); ok {
		t.Fatal("Get(missing) reported found")
	}
}

func TestScanZeroAlloc(t *testing.T) {
	// No t.Parallel(): AllocsPerRun must not run under a parallel test.
	block := []byte("Host: example.com\nContent-Type: application/json\nX-Trace: a:b:c\n\n")
	if n := scanNoAlloc(block); n != 3 {
		t.Fatalf("scanNoAlloc = %d, want 3", n)
	}
	allocs := testing.AllocsPerRun(1000, func() {
		_ = scanNoAlloc(block)
	})
	if allocs != 0 {
		t.Fatalf("scanNoAlloc allocated %.2f/op; want 0", allocs)
	}
}
```

## Review

The parser is correct when it splits on the *first* colon (so `a:b:c` stays a
single value), trims the optional whitespace, skips blank lines, and matches names
case-insensitively — all pinned by the table and the lookup test. Splitting on the
first colon is what preserves values that legitimately contain colons, a detail a
naive `bytes.Split(line, ":")` would get wrong.

The point of the exercise is that none of this needs a `string` conversion during
the scan: `bytes.Cut`, `bytes.TrimSpace`, and `bytes.EqualFold` do the work on the
buffer directly, and `TestScanZeroAlloc` proves the scan allocates nothing. In
production, `net/http` already parses headers for you; reach for hand-rolled `bytes`
parsing when you are below `net/http` — a custom wire protocol, a proxy inspecting a
raw block — where staying on `[]byte` is the whole performance point.

## Resources

- [`bytes`](https://pkg.go.dev/bytes) — `Cut`, `TrimSpace`, `EqualFold`, `Index`, `Split`, the `strings` mirror.
- [`net/textproto.CanonicalMIMEHeaderKey`](https://pkg.go.dev/net/textproto#CanonicalMIMEHeaderKey) — how the standard library canonicalizes header names.
- [`strings.EqualFold`](https://pkg.go.dev/strings#EqualFold) — the string twin, for comparison.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-constant-time-token-compare.md](07-constant-time-token-compare.md)
