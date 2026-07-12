# Exercise 5: Extract a request ID from a large payload without pinning the whole buffer

Slicing a string is free, but the slice keeps the whole backing array alive. Pull
a 20-byte request ID out of a 2 MB body, cache it, and you have leaked two
megabytes. This module builds the extraction the safe way with `strings.Clone`
and proves — via the backing pointer — that the reference to the big buffer is
actually severed.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
reqid/                      independent module: example.com/reqid
  go.mod                    go 1.26
  reqid.go                  ExtractRequestID (cloned) and extractLeaky (raw slice)
  cmd/
    demo/
      main.go               extract an id from a large synthetic body
  reqid_test.go             correctness + backing-pointer retention proof
```

Files: `reqid.go`, `cmd/demo/main.go`, `reqid_test.go`.
Implement: `ExtractRequestID(payload string) (string, bool)` that finds an
`X-Request-Id: <id>` token and returns a cloned id; and an unexported
`extractLeaky` returning the raw slice for contrast.
Test: present/absent/malformed id, plus a retention test comparing
`unsafe.StringData` pointers to prove the clone severed the reference.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/01-string-basics/05-substring-retention/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/01-string-basics/05-substring-retention
```

## Why the slice leaks, and how Clone fixes it

`payload[start:end]` returns a string header pointing *into* `payload`'s backing
array. The extracted id shares storage with the whole body. As long as the id is
reachable — cached, stored in a struct, captured by a goroutine — the garbage
collector cannot free the body, because reachability is all-or-nothing for a
backing array: keeping any byte keeps every byte. Extract a short id from a 2 MB
request and stash it in a per-connection cache and you have pinned 2 MB per
connection. This is a real production memory leak that never shows up in a unit
test using tiny inputs; it only appears as steadily climbing RSS under load.

`strings.Clone(s)` copies the bytes of `s` into a fresh, minimally sized
allocation and returns a string over *that*. The returned id no longer points into
the body, so once the body goes out of scope the GC reclaims all 2 MB. That is the
one job of `Clone`: sever a small survivor from a large buffer. (It is documented
to allocate nothing for the empty string, so an empty extraction stays cheap.)

To make the difference provable rather than asserted, the test uses
`unsafe.StringData`, which returns a pointer to a string's first byte. For the
leaky version the id's pointer lies *inside* the payload's byte range; for the
cloned version it points into a different allocation entirely, so the pointers
differ. Comparing them is a concrete demonstration that `Clone` moved the bytes.

Create `reqid.go`:

```go
package reqid

import "strings"

const marker = "X-Request-Id: "

// ExtractRequestID finds "X-Request-Id: <id>" in payload and returns the id,
// cloned so it does not retain payload's backing array. ok is false when the
// marker is absent or the id is empty.
func ExtractRequestID(payload string) (string, bool) {
	id, ok := extractLeaky(payload)
	if !ok {
		return "", false
	}
	// Clone severs the reference to payload's (possibly huge) backing array.
	return strings.Clone(id), true
}

// extractLeaky returns the id as a raw sub-slice of payload. It is retained here
// only to contrast against the Clone-based version in the tests: the returned
// string pins the entire payload backing array.
func extractLeaky(payload string) (string, bool) {
	_, after, found := strings.Cut(payload, marker)
	if !found {
		return "", false
	}
	// The id runs to the next CR/LF or end of string.
	id := after
	if i := strings.IndexAny(after, "\r\n"); i >= 0 {
		id = after[:i]
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", false
	}
	return id, true
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/reqid"
)

func main() {
	// A large body with the header buried inside it.
	body := strings.Repeat("padding padding padding\n", 50000) +
		"X-Request-Id: 7f3a9c2e-req\r\n" +
		strings.Repeat("more body\n", 50000)

	fmt.Printf("body size: %d bytes\n", len(body))
	if id, ok := reqid.ExtractRequestID(body); ok {
		fmt.Printf("request id: %s (len %d)\n", id, len(id))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
body size: 1700028 bytes
request id: 7f3a9c2e-req (len 12)
```

## Tests

The correctness table covers a present id, an absent marker, and a malformed
(empty) id. The retention test is the teaching artifact: it builds a large body,
extracts the id both ways, and compares the backing pointers via
`unsafe.StringData`. The leaky slice must point *into* the payload; the cloned id
must point *elsewhere* — proving the reference was severed.

Create `reqid_test.go`:

```go
package reqid

import (
	"fmt"
	"strings"
	"testing"
	"unsafe"
)

func TestExtractRequestID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload string
		wantID  string
		wantOK  bool
	}{
		{"present crlf", "a\r\nX-Request-Id: abc-123\r\nb", "abc-123", true},
		{"present eol", "head\nX-Request-Id: xyz", "xyz", true},
		{"absent", "no id here at all", "", false},
		{"empty id", "X-Request-Id: \r\nrest", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id, ok := ExtractRequestID(tc.payload)
			if id != tc.wantID || ok != tc.wantOK {
				t.Fatalf("ExtractRequestID(%q) = %q,%v; want %q,%v",
					tc.payload, id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// within reports whether p points at a byte inside s's backing array.
func within(p *byte, s string) bool {
	if len(s) == 0 {
		return false
	}
	base := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	addr := uintptr(unsafe.Pointer(p))
	return addr >= base && addr < base+uintptr(len(s))
}

func TestCloneSeversReference(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("x", 1<<20) + "X-Request-Id: req-42\r\n" // ~1 MB

	leaky, ok := extractLeaky(payload)
	if !ok {
		t.Fatal("extractLeaky failed to find id")
	}
	cloned, ok := ExtractRequestID(payload)
	if !ok {
		t.Fatal("ExtractRequestID failed to find id")
	}
	if leaky != cloned {
		t.Fatalf("id mismatch: leaky=%q cloned=%q", leaky, cloned)
	}

	// The leaky slice must point inside the big payload; the clone must not.
	if !within(unsafe.StringData(leaky), payload) {
		t.Fatal("leaky id does not point into payload; test assumption broken")
	}
	if within(unsafe.StringData(cloned), payload) {
		t.Fatal("cloned id still points into payload; Clone did not sever the reference")
	}
}

func ExampleExtractRequestID() {
	id, ok := ExtractRequestID("GET /x\r\nX-Request-Id: r-1\r\n")
	fmt.Println(id, ok)
	// Output: r-1 true
}
```

## Review

The extractor is correct when it returns the id for a present marker and reports
`false` for an absent marker or an empty id. The retention test is what makes the
lesson real: `extractLeaky`'s result points inside the payload's backing array —
so it pins the whole megabyte — while `ExtractRequestID`'s `Clone`d result points
into a separate allocation, freeing the payload once it is out of scope. Reach for
`strings.Clone` precisely in this situation — a small survivor of a large buffer —
and not indiscriminately, since cloning everything wastes memory. Run
`go test -race`; the functions are pure and share no state.

## Resources

- [strings.Clone (pkg.go.dev)](https://pkg.go.dev/strings#Clone)
- [unsafe.StringData (pkg.go.dev)](https://pkg.go.dev/unsafe#StringData)
- [The Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)
- [Go issue 53003: strings.Clone rationale](https://github.com/golang/go/issues/53003)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-case-insensitive-lookup.md](04-case-insensitive-lookup.md) | Next: [06-rune-length-validation.md](06-rune-length-validation.md)
