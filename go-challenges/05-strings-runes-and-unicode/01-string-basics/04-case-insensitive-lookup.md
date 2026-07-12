# Exercise 4: Match HTTP methods and header names without allocating via strings.EqualFold

Case-insensitive comparison happens on every request: is this method allowed, does
this header name match. The obvious `ToLower(a) == ToLower(b)` allocates two
strings per call. This module builds the allocation-free version with
`strings.EqualFold` and proves the difference with a measured allocation test.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
casematch/                  independent module: example.com/casematch
  go.mod                    go 1.26
  casematch.go              MethodAllowed, HeaderCanonicalMatch
  cmd/
    demo/
      main.go               check a mixed-case method and header name
  casematch_test.go         mixed-case tables + zero-alloc assertion
```

Files: `casematch.go`, `cmd/demo/main.go`, `casematch_test.go`.
Implement: `MethodAllowed(method string, allowed []string) bool` and
`HeaderCanonicalMatch(name, want string) bool`.
Test: mixed-case method and header inputs, plus an `AllocsPerRun` assertion that
the `EqualFold` path is zero-allocation while a `ToLower` reference allocates.
Verify: `go test -count=1 -race ./...`

## Why EqualFold, not ToLower, on the request path

`strings.ToLower(a) == strings.ToLower(b)` looks harmless, but each `ToLower`
allocates a *new* string holding the lowercased bytes. On a per-request path — a
router checking the method on every call, middleware matching a header name — that
is two throwaway allocations per comparison, multiplied by request volume, feeding
the garbage collector for no reason. It is also subtly wrong for some Unicode: not
every case relationship is captured by naive lowercasing.

`strings.EqualFold(a, b)` compares the two strings under Unicode simple
case-folding *without building any intermediate string*. It walks both inputs rune
by rune and compares folded runes in place, so it allocates nothing and it is
Unicode-aware. For ASCII tokens like HTTP methods and header names it is the exact
right tool, and its zero-allocation property is measurable — which is what the
test below pins.

`MethodAllowed` compares the incoming method against an allow-list with
`EqualFold` so `get`, `GET`, and `Get` all match `"GET"`. `HeaderCanonicalMatch`
does the same for a header name so `content-type` matches `Content-Type`. In real
code `net/http` canonicalizes header names for you via `textproto.CanonicalMIMEHeaderKey`
and exposes method constants like `http.MethodGet`; this exercise implements the
comparison directly to make the allocation trade-off visible.

Create `casematch.go`:

```go
package casematch

import "strings"

// MethodAllowed reports whether method matches any entry of allowed,
// case-insensitively, with no per-call allocation.
func MethodAllowed(method string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(method, a) {
			return true
		}
	}
	return false
}

// HeaderCanonicalMatch reports whether an incoming header name matches want,
// case-insensitively (HTTP header field names are case-insensitive).
func HeaderCanonicalMatch(name, want string) bool {
	return strings.EqualFold(name, want)
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/casematch"
)

func main() {
	allowed := []string{"GET", "POST", "DELETE"}
	fmt.Println(casematch.MethodAllowed("get", allowed))
	fmt.Println(casematch.MethodAllowed("Put", allowed))
	fmt.Println(casematch.HeaderCanonicalMatch("content-type", "Content-Type"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
true
false
true
```

## Tests

The tables cover mixed-case matches and non-matches. The allocation test measures
both paths side by side: `MethodAllowed` (via `EqualFold`) must report zero
allocations, while a `ToLower`-based reference implementation allocates — making
the trade-off an assertion rather than a claim. The measured functions close over
pre-built values so the measurement captures only the comparison, not slice
construction.

Create `casematch_test.go`:

```go
package casematch

import (
	"fmt"
	"strings"
	"testing"
)

func TestMethodAllowed(t *testing.T) {
	t.Parallel()

	allowed := []string{"GET", "POST", "DELETE"}
	tests := []struct {
		method string
		want   bool
	}{
		{"GET", true},
		{"get", true},
		{"Get", true},
		{"post", true},
		{"PUT", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := MethodAllowed(tc.method, allowed); got != tc.want {
			t.Fatalf("MethodAllowed(%q) = %v, want %v", tc.method, got, tc.want)
		}
	}
}

func TestHeaderCanonicalMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, want string
		match      bool
	}{
		{"Content-Type", "Content-Type", true},
		{"content-type", "Content-Type", true},
		{"CONTENT-TYPE", "content-type", true},
		{"X-Request-Id", "X-Request-ID", true},
		{"Content-Length", "Content-Type", false},
	}
	for _, tc := range tests {
		if got := HeaderCanonicalMatch(tc.name, tc.want); got != tc.match {
			t.Fatalf("HeaderCanonicalMatch(%q,%q) = %v, want %v", tc.name, tc.want, got, tc.match)
		}
	}
}

// toLowerMatch is the allocating reference the lesson warns against.
func toLowerMatch(a, b string) bool { return strings.ToLower(a) == strings.ToLower(b) }

func TestEqualFoldIsZeroAlloc(t *testing.T) {
	// No t.Parallel(): testing.AllocsPerRun must not run during a parallel test.
	foldAllocs := testing.AllocsPerRun(1000, func() {
		_ = HeaderCanonicalMatch("content-type", "Content-Type")
	})
	if foldAllocs != 0 {
		t.Fatalf("EqualFold path allocated %.0f times; want 0", foldAllocs)
	}

	lowerAllocs := testing.AllocsPerRun(1000, func() {
		_ = toLowerMatch("content-type", "Content-Type")
	})
	if lowerAllocs == 0 {
		t.Fatalf("ToLower reference allocated 0 times; expected it to allocate, so the comparison is not meaningful")
	}
}

func ExampleMethodAllowed() {
	fmt.Println(MethodAllowed("delete", []string{"GET", "DELETE"}))
	// Output: true
}
```

## Review

The matchers are correct when they treat ASCII case as insignificant — `get`
matches `GET`, `content-type` matches `Content-Type` — and when they do so without
allocating. The allocation test is the point of the exercise: it asserts the
`EqualFold` path is zero-allocation and that the `ToLower` reference is not, so the
"why not ToLower" advice is enforced by the test rather than left as a comment.
Note the `X-Request-Id` versus `X-Request-ID` case still matches because folding
is per-rune. Keep the allocation test non-parallel — `testing.AllocsPerRun` sets GOMAXPROCS to
1 and must not be called inside a parallel test — and run `go test -race`.

## Resources

- [strings.EqualFold (pkg.go.dev)](https://pkg.go.dev/strings#EqualFold)
- [net/http method constants (pkg.go.dev)](https://pkg.go.dev/net/http#pkg-constants)
- [textproto.CanonicalMIMEHeaderKey (pkg.go.dev)](https://pkg.go.dev/net/textproto#CanonicalMIMEHeaderKey)
- [RFC 9110 §5.1 Field Names (case-insensitive)](https://www.rfc-editor.org/rfc/rfc9110#name-field-names)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-bounded-builder.md](03-bounded-builder.md) | Next: [05-substring-retention.md](05-substring-retention.md)
