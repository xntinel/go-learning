# Exercise 4: Fix a Shared-Backing-Array Append Bug in a Request Pipeline

A middleware chain derives per-request metric labels from a shared base set and
appends a request-specific label. Because `append` onto a base slice that has
spare capacity writes in place, two requests clobber each other's labels through
the shared backing array. This exercise reproduces that corruption and fixes it
by cloning the base (or bounding its capacity) before appending.

Self-contained module: own `go mod init`, own demo, own tests.

## What you'll build

```text
reqlabels/                 independent module: example.com/reqlabels
  go.mod                   go 1.26
  labels.go                Derive (clone-then-append), deriveShared (buggy)
  cmd/
    demo/
      main.go              two requests derive labels; neither sees the other
  labels_test.go           isolation test, shared-backing corruption test
```

Files: `labels.go`, `cmd/demo/main.go`, `labels_test.go`.
Implement: `Derive(base, reqLabel)` that clones `base` then appends; a buggy `deriveShared` that appends onto `base` directly.
Test: derive two request label sets from one base, append different labels, assert neither sees the other and `base` is unchanged; a negative sub-test reproduces the corruption.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reqlabels/cmd/demo
cd ~/go-exercises/reqlabels
go mod init example.com/reqlabels
```

### Why the base slice's spare capacity is the trap

A service builds a base set of labels once at startup — `env=prod`, `region=us` —
and each request appends its own `request_id=...` before emitting metrics. The
natural code is `append(base, reqLabel)`. It looks pure: `append` returns a new
slice, so surely each request gets its own. It is not pure. If `base` was built
with `make([]string, 0, N)` or is a sub-slice of a larger array, it has spare
capacity past its length. `append(base, reqLabel)` sees that spare slot, writes
`reqLabel` into it *in the shared backing array*, and returns a slice whose extra
element is that same slot. The next request does the identical thing — `base` still
has the same length, so its `append` writes into the *same* slot — and now both
request slices' last element is the second request's label. Request A's metrics
are tagged with request B's id. Under concurrency this is also a data race; even
single-threaded it is logical corruption `-race` cannot always catch.

The fix is to stop sharing before the append. `slices.Clone(base)` gives each
request its own backing array, so its `append` writes into private memory. The
capacity-bounding alternative `base[:len(base):len(base)]` sets `cap == len`, which
forces the *append itself* to reallocate; it avoids one copy in the common case
where nothing else needs the base afterward. Cloning is the clearer default when
in doubt, because it also protects against a consumer overwriting an existing
element, not just against append.

Create `labels.go`:

```go
package reqlabels

import "slices"

// Derive returns base plus one request-specific label, on a fresh backing array
// so concurrent or sequential requests never share the appended slot.
func Derive(base []string, reqLabel string) []string {
	out := slices.Clone(base)
	return append(out, reqLabel)
}

// deriveShared is the buggy variant used only in tests: appending onto base
// directly reuses base's spare capacity, so two requests clobber each other.
func deriveShared(base []string, reqLabel string) []string {
	return append(base, reqLabel)
}
```

### The runnable demo

The demo builds a base with spare capacity (the realistic startup pattern), then
derives labels for two requests and prints both to show each keeps its own request
id and the base is unchanged.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/reqlabels"
)

func main() {
	// Base built with spare capacity, as a startup label set often is.
	base := make([]string, 0, 4)
	base = append(base, "env=prod", "region=us")

	a := reqlabels.Derive(base, "request_id=A")
	b := reqlabels.Derive(base, "request_id=B")

	fmt.Println("request A:", a)
	fmt.Println("request B:", b)
	fmt.Println("base:     ", base)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request A: [env=prod region=us request_id=A]
request B: [env=prod region=us request_id=B]
base:      [env=prod region=us]
```

### Tests

`TestDeriveIsolatesRequests` derives two label sets and asserts each keeps its own
label and the base is unchanged. `TestSharedBackingCorruption` uses the buggy
`deriveShared` on a base with spare capacity and asserts request A's last label was
overwritten by request B — making the shared-backing failure explicit.

Create `labels_test.go`:

```go
package reqlabels

import (
	"slices"
	"testing"
)

func baseWithSpareCap() []string {
	base := make([]string, 0, 4)
	return append(base, "env=prod", "region=us")
}

func TestDeriveIsolatesRequests(t *testing.T) {
	t.Parallel()

	base := baseWithSpareCap()
	a := Derive(base, "request_id=A")
	b := Derive(base, "request_id=B")

	if a[len(a)-1] != "request_id=A" {
		t.Fatalf("request A last label = %q, want request_id=A", a[len(a)-1])
	}
	if b[len(b)-1] != "request_id=B" {
		t.Fatalf("request B last label = %q, want request_id=B", b[len(b)-1])
	}
	if !slices.Equal(base, []string{"env=prod", "region=us"}) {
		t.Fatalf("base mutated: %v", base)
	}
}

func TestSharedBackingCorruption(t *testing.T) {
	t.Parallel()

	base := baseWithSpareCap()
	a := deriveShared(base, "request_id=A")
	b := deriveShared(base, "request_id=B")

	// Both appends targeted the same spare slot in base's backing array, so
	// request A's last label was overwritten by request B's.
	if a[len(a)-1] != "request_id=B" {
		t.Fatalf("expected shared-backing corruption: a last = %q, want request_id=B", a[len(a)-1])
	}
	_ = b
}

func TestDeriveManyRequestsAreIndependent(t *testing.T) {
	t.Parallel()

	base := baseWithSpareCap()
	got := make([][]string, 3)
	for i := range got {
		got[i] = Derive(base, "request_id="+string(rune('A'+i)))
	}
	for i, want := range []string{"request_id=A", "request_id=B", "request_id=C"} {
		if got[i][len(got[i])-1] != want {
			t.Fatalf("request %d last label = %q, want %q", i, got[i][len(got[i])-1], want)
		}
	}
}
```

## Review

The pipeline is correct when each request's labels are independent:
`TestDeriveIsolatesRequests` and `TestDeriveManyRequestsAreIndependent` prove no
request sees another's label and the base never changes. The negative
`TestSharedBackingCorruption` demonstrates the exact bug — two `append`s onto a
base with spare capacity land in the same slot — so the reason `Derive` clones is
concrete. The lesson to carry: `append` returning a slice is not evidence of
isolation; only a fresh backing array (clone) or a capacity bound that forces
reallocation (`base[:len(base):len(base)]`) makes it safe. Run `-race`; in a real
concurrent middleware the shared-slot write is a data race as well as logical
corruption.

## Resources

- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices)
- [Go Specification: Slice expressions (full slice expression)](https://go.dev/ref/spec#Slice_expressions)
- [slices package (`Clone`)](https://pkg.go.dev/slices#Clone)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-three-index-buffer-handoff.md](03-three-index-buffer-handoff.md) | Next: [05-scanner-token-must-be-copied.md](05-scanner-token-must-be-copied.md)
