# Exercise 14: Scan Multipart Boundaries Into Zero-Copy Views, Then Clone at the Boundary

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A batch-upload endpoint receives one body containing several sub-payloads
concatenated with a boundary marker — the same shape as HTTP
`multipart/form-data`, or a batched webhook delivery combining several event
bodies into one POST. Splitting that body into parts is a pure scanning
problem, and doing it with zero copies matters: request bodies can run into
megabytes, and most parts only need to be sniffed (content-type detection,
routing) before being discarded. But the one part an ingestion handler
actually persists to object storage cannot stay a view — the connection's
read loop reuses its buffer for the next request the instant this one
returns. This exercise builds a scanner that hands out aliased views by
default and an explicit `Clone` for the one part that has to survive.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
multipart/                  module example.com/multipart
  go.mod                    go 1.24
  multipart.go               Part with Data []byte and Clone(); Scan(body, boundary) []Part
  multipart_test.go          boundary-position table, alias-mutation test, clone-isolation
                             test, the naive-persist contrast, ExampleScan
```

- Files: `multipart.go`, `multipart_test.go`.
- Implement: `Part{ Data []byte }` whose `Data` aliases the scanned body;
  `(Part) Clone() Part` returning `bytes.Clone`d, independent `Data`; `Scan(body,
  boundary []byte) []Part` splitting on every occurrence of `boundary` via
  `bytes.Index`, skipping empty segments (leading, trailing, or back-to-back
  boundaries), with every `Part.Data` a direct sub-slice of `body`.
- Test: multiple boundaries, a boundary at the very start, a boundary at the
  very end, no boundary present, an empty body, and back-to-back boundaries —
  each checked against the exact expected part strings; a test that mutates
  the source buffer and observes an un-cloned `Part` change; a test that
  clones a part first and confirms it does not change; an unexported
  `persistNaive` that forgets to clone at the persistence boundary,
  contrasted against `Clone` on the same buffer-reuse scenario; and
  `ExampleScan` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/multipart
cd ~/go-exercises/multipart
go mod init example.com/multipart
go mod edit -go=1.24
```

### Zero-copy by default, Clone at the boundary the caller crosses

`Scan` walks `body` with repeated `bytes.Index` calls, and every `Part` it
returns is built as `Part{Data: body[start:start+idx]}` — a direct sub-slice
expression, not a copy. This is the deliberate default: for a handler that
only inspects a handful of parts to route or validate the request, copying
every part up front would mean paying for megabytes of allocation on data
that gets thrown away within the same function call. `Scan` itself allocates
nothing beyond the `[]Part` header slice that holds the *views* — the payload
bytes underneath are never touched.

The hazard is exactly the one this lesson's concepts file names directly: an
"ephemeral buffer hands out short-lived views." `body` here is not
necessarily a scanner buffer, but it behaves like one at the system boundary
— it is very often backed by a connection's read buffer, or a buffer drawn
from a `sync.Pool` to avoid a fresh allocation per request, and either way it
gets reused or returned the moment the handler that owns it moves on. A
`Part.Data` that outlives that moment is reading whatever bytes happen to
occupy that region of the array next, exactly like a retained
`bufio.Scanner.Bytes()` result. The concrete failure looks like this:

```go
func persistNaive(p Part) Part {
	return p // forgot Clone(); still aliases whatever buffer p.Data came from
}
```

Nothing about `persistNaive`'s signature says anything is wrong — it type
checks, it compiles, and it passes any test that does not specifically
mutate the source buffer afterward. It is the single most common way this
bug ships: a handler reaches the line where it decides "this part gets
written to storage" and simply hands over the `Part` it already has.

The rule this exercise pins is *where* the copy has to happen: not inside
`Scan` (which would defeat the zero-copy point for every part that never gets
retained), but at the one call site where a specific `Part` is about to
outlive the scan — `persisted := parts[i].Clone()`. `Clone` is a one-line
method, `bytes.Clone(p.Data)`, but its placement is the actual lesson: the
decision to copy belongs to the code that knows which parts survive, not to
the scanner that cannot know that in advance.

Create `multipart.go`:

```go
// Package multipart scans a batch-upload body -- several sub-payloads
// concatenated with a boundary marker, the same shape as HTTP
// multipart/form-data or a batched webhook delivery -- into its individual
// parts without copying the body up front.
package multipart

import "bytes"

// Part is one segment of a scanned body. Data is a zero-copy view: it
// aliases the buffer passed to Scan. A Part is only valid as long as that
// buffer is not mutated or reused; a caller that keeps a Part past the
// scan call must Clone it first.
type Part struct {
	Data []byte
}

// Clone returns a Part whose Data is an independent copy, safe to retain
// after the source buffer is mutated or handed back to a pool for reuse.
// This is the API-boundary tool: an ingestion handler scans zero-copy on
// the hot path, then Clones only the one part it actually persists.
func (p Part) Clone() Part {
	return Part{Data: bytes.Clone(p.Data)}
}

// Scan splits body into Parts wherever boundary occurs, in order. Every
// returned Part.Data is a direct sub-slice of body sharing its backing
// array -- Scan copies no payload bytes, only appends to the returned
// []Part header slice. Empty segments (a boundary at the very start of
// body, two boundaries back to back, or a boundary at the very end) are
// skipped rather than returned as zero-length parts.
//
// An empty boundary is treated as "no delimiter": Scan returns the whole
// non-empty body as a single Part, or no parts at all for an empty body.
//
// Scan does not mutate body and is safe to call concurrently on the same
// body from multiple goroutines, as long as no goroutine mutates body while
// a scan is in progress; the returned Parts alias body exactly as
// documented on the Part type.
func Scan(body, boundary []byte) []Part {
	if len(boundary) == 0 {
		if len(body) == 0 {
			return nil
		}
		return []Part{{Data: body}}
	}

	var parts []Part
	start := 0
	for {
		idx := bytes.Index(body[start:], boundary)
		if idx < 0 {
			if start < len(body) {
				parts = append(parts, Part{Data: body[start:]})
			}
			return parts
		}
		if idx > 0 {
			parts = append(parts, Part{Data: body[start : start+idx]})
		}
		start += idx + len(boundary)
	}
}
```

### Using it

Call `Scan(body, boundary)` once per request body; every `Part` it returns
aliases `body`, so route, validate, or discard as many parts as you like at
zero allocation cost. The moment you decide a specific part must outlive the
current handler call — written to object storage, forwarded to another
goroutine, enqueued for later processing — call `part.Clone()` on that one
part before the underlying buffer can be mutated or reused. `Scan` itself
does not mutate `body` and is safe to call concurrently from multiple
goroutines on the same `body`, as long as nothing else is mutating `body` at
the same time; the `Part`s it returns still carry the aliasing contract
documented on the `Part` type.

The module has no `main.go`, because a boundary scanner is a library, not a
tool. Its executable demonstration is `ExampleScan`: `go test` runs it and
compares its standard output against the `// Output:` comment, so the usage
shown below cannot drift away from the code.

```go
func ExampleScan() {
	body := []byte("orderA|orderB|orderC")
	parts := Scan(body, []byte("|"))

	fmt.Printf("scanned %d parts (zero copy):\n", len(parts))
	for i, p := range parts {
		fmt.Printf("  part %d: %q\n", i, p.Data)
	}

	// The ingestion pipeline only needs to persist part 2; it clones that
	// one part before the shared body buffer gets reused by the next read.
	persisted := parts[2].Clone()

	// The connection's read loop now reuses body for the next message,
	// overwriting the bytes that used to be "orderC".
	copy(body[14:], []byte("XXXXXX"))

	fmt.Println("after the source buffer is reused:")
	fmt.Printf("  uncloned view parts[2]: %q (corrupted, still aliases body)\n", parts[2].Data)
	fmt.Printf("  cloned copy persisted:  %q (unaffected)\n", persisted.Data)

	// Output:
	// scanned 3 parts (zero copy):
	//   part 0: "orderA"
	//   part 1: "orderB"
	//   part 2: "orderC"
	// after the source buffer is reused:
	//   uncloned view parts[2]: "XXXXXX" (corrupted, still aliases body)
	//   cloned copy persisted:  "orderC" (unaffected)
}
```

`parts[2].Data` reading back as the connection's *next* message, rather than
the settlement order it was scanned from, is the exact shape of the bug a
retained ephemeral view produces in production: no panic, no error, just
silently wrong data read some time after the scan that produced it.

### Tests

The boundary-position table is the exercise's core: it walks every shape a
delimiter can take relative to the body — in the middle, at the start, at the
end, absent, and back-to-back — and checks the exact resulting part strings,
so the empty-segment-skipping logic is pinned independently at each position.
`TestScanViewsAliasSourceBuffer` and `TestCloneIsolatesFromSourceMutation`
are the pair that proves the actual lesson: the first shows an un-cloned
`Part` reflecting a mutation to the source body, and the second shows a
cloned `Part` of the same body staying stable across that same mutation.

`persistNaive` is unexported and unreachable from the package API; it exists
so `TestPersistNaiveLeaksSourceReuse` can pin the module's real-world failure
mode in one test: a part "persisted" without calling `Clone` reads back as
whatever bytes the connection's read loop wrote into the reused buffer
afterward, while the same scenario run through `Clone` survives intact. If a
future edit at a call site drops the `.Clone()` call, this is the exact
corruption it would reintroduce.

Create `multipart_test.go`:

```go
package multipart

import (
	"fmt"
	"testing"
)

func partStrings(parts []Part) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p.Data)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestScan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		boundary string
		want     []string
	}{
		{
			name:     "multiple boundaries",
			body:     "orderA|orderB|orderC",
			boundary: "|",
			want:     []string{"orderA", "orderB", "orderC"},
		},
		{
			name:     "boundary at start skips empty leading part",
			body:     "|orderA|orderB",
			boundary: "|",
			want:     []string{"orderA", "orderB"},
		},
		{
			name:     "boundary at end skips empty trailing part",
			body:     "orderA|orderB|",
			boundary: "|",
			want:     []string{"orderA", "orderB"},
		},
		{
			name:     "no boundary present yields a single part",
			body:     "justoneorder",
			boundary: "|",
			want:     []string{"justoneorder"},
		},
		{
			name:     "empty body yields no parts",
			body:     "",
			boundary: "|",
			want:     nil,
		},
		{
			name:     "back to back boundaries produce no empty part between them",
			body:     "orderA||orderB",
			boundary: "|",
			want:     []string{"orderA", "orderB"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := partStrings(Scan([]byte(tc.body), []byte(tc.boundary)))
			if !equalStrings(got, tc.want) {
				t.Fatalf("Scan(%q, %q) = %v, want %v", tc.body, tc.boundary, got, tc.want)
			}
		})
	}
}

// TestScanViewsAliasSourceBuffer is the core lesson: an un-cloned Part is a
// window into body, so mutating body after Scan is observed through the
// Part.
func TestScanViewsAliasSourceBuffer(t *testing.T) {
	t.Parallel()

	body := []byte("orderA|orderB|orderC")
	parts := Scan(body, []byte("|"))
	if len(parts) != 3 {
		t.Fatalf("got %d parts, want 3", len(parts))
	}

	// parts[1].Data views body[7:13] ("orderB"); mutate its first byte
	// through the source buffer.
	body[7] = 'X'
	if parts[1].Data[0] != 'X' {
		t.Fatalf("uncloned part did not observe source mutation: got %q", parts[1].Data)
	}
}

// TestCloneIsolatesFromSourceMutation proves the fix: a Cloned Part does
// not alias body, so mutating body afterward leaves the clone untouched
// while an uncloned Part of the same source keeps reflecting it.
func TestCloneIsolatesFromSourceMutation(t *testing.T) {
	t.Parallel()

	body := []byte("orderA|orderB|orderC")
	parts := Scan(body, []byte("|"))
	if len(parts) != 3 {
		t.Fatalf("got %d parts, want 3", len(parts))
	}

	cloned := parts[2].Clone()
	if string(cloned.Data) != "orderC" {
		t.Fatalf("Clone() = %q, want %q", cloned.Data, "orderC")
	}

	// Mutate body in the region backing parts[2] ("orderC" starts at index 14).
	body[14] = 'Z'

	if parts[2].Data[0] != 'Z' {
		t.Fatalf("uncloned view did not reflect source mutation: got %q", parts[2].Data)
	}
	if cloned.Data[0] == 'Z' {
		t.Fatalf("clone leaked the source mutation: got %q", cloned.Data)
	}
	if string(cloned.Data) != "orderC" {
		t.Fatalf("clone changed after source mutation: got %q, want %q", cloned.Data, "orderC")
	}
}

func TestScanNoDelimiter(t *testing.T) {
	t.Parallel()

	if got := Scan([]byte("solo"), nil); len(got) != 1 || string(got[0].Data) != "solo" {
		t.Fatalf("Scan with empty boundary = %v, want single part %q", got, "solo")
	}
	if got := Scan(nil, nil); got != nil {
		t.Fatalf("Scan(nil, nil) = %v, want nil", got)
	}
}

// persistNaive is the handler-side mistake this module actually guards
// against: reaching the persistence boundary and forgetting the Clone
// call, so the "persisted" Part is still a view into whatever buffer Scan
// was given. It is never exported and never reachable from the package
// API; it exists only so the test below can pin what it gets wrong.
func persistNaive(p Part) Part {
	return p
}

// TestPersistNaiveLeaksSourceReuse reproduces the production shape of the
// bug: a connection's read loop reuses its buffer for the next message the
// instant a handler returns. A part "persisted" via persistNaive reads back
// as the next message's bytes, silently wrong with no panic and no error --
// exactly the failure this exercise is built to make visible in a test
// instead of a customer's stored record.
func TestPersistNaiveLeaksSourceReuse(t *testing.T) {
	t.Parallel()

	body := []byte("orderA|orderB|orderC")
	parts := Scan(body, []byte("|"))
	if len(parts) != 3 {
		t.Fatalf("got %d parts, want 3", len(parts))
	}

	persisted := persistNaive(parts[2])

	// The connection's read loop reuses body for the next message.
	copy(body[14:], []byte("XXXXXX"))

	if string(persisted.Data) == "orderC" {
		t.Fatal("persistNaive unexpectedly survived buffer reuse; want it to alias body and read back corrupted")
	}
	if string(persisted.Data) != "XXXXXX" {
		t.Fatalf("persisted.Data = %q, want it to read back the reused buffer's new bytes %q", persisted.Data, "XXXXXX")
	}

	// The fix -- persisting Clone()'d data instead -- survives the same reuse.
	body2 := []byte("orderA|orderB|orderC")
	parts2 := Scan(body2, []byte("|"))
	safe := parts2[2].Clone()
	copy(body2[14:], []byte("XXXXXX"))
	if string(safe.Data) != "orderC" {
		t.Fatalf("Clone()'d part changed after buffer reuse: got %q, want %q", safe.Data, "orderC")
	}
}

// ExampleScan is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below. It
// scans a body into parts, clones the one part it decides to persist, then
// simulates the connection's read loop reusing the source buffer for the
// next message.
func ExampleScan() {
	body := []byte("orderA|orderB|orderC")
	parts := Scan(body, []byte("|"))

	fmt.Printf("scanned %d parts (zero copy):\n", len(parts))
	for i, p := range parts {
		fmt.Printf("  part %d: %q\n", i, p.Data)
	}

	// The ingestion pipeline only needs to persist part 2; it clones that
	// one part before the shared body buffer gets reused by the next read.
	persisted := parts[2].Clone()

	// The connection's read loop now reuses body for the next message,
	// overwriting the bytes that used to be "orderC".
	copy(body[14:], []byte("XXXXXX"))

	fmt.Println("after the source buffer is reused:")
	fmt.Printf("  uncloned view parts[2]: %q (corrupted, still aliases body)\n", parts[2].Data)
	fmt.Printf("  cloned copy persisted:  %q (unaffected)\n", persisted.Data)

	// Output:
	// scanned 3 parts (zero copy):
	//   part 0: "orderA"
	//   part 1: "orderB"
	//   part 2: "orderC"
	// after the source buffer is reused:
	//   uncloned view parts[2]: "XXXXXX" (corrupted, still aliases body)
	//   cloned copy persisted:  "orderC" (unaffected)
}
```

## Review

`Scan` is correct when every boundary position in the table produces exactly
the expected parts with no spurious empty segments, and the zero-copy
contract holds in both directions: an un-cloned `Part` must reflect a source
mutation (proving it really is a view, not an accidental copy), and a cloned
`Part` must not (proving `Clone` really does detach). `persistNaive` pins the
wrong turn concretely: a handler that forgets the `.Clone()` call at the
persistence boundary compiles, passes any test that never mutates the
source afterward, and then reads back the *next* message's bytes in
production — no panic, no error, just silently wrong data. The mistake this
design also heads off is calling `bytes.Clone` inside `Scan` "to be safe": it
would make every test pass, but it defeats the entire zero-copy point for the
common case of parts that are only sniffed and discarded within the same
request. Copy only the parts that actually cross out of the scan's lifetime,
and do it at that exact call site. `ExampleScan` is the executable
documentation: `go test` verifies its output. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions)
- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone)
- [`bytes.Index`](https://pkg.go.dev/bytes#Index)
- [`mime/multipart`](https://pkg.go.dev/mime/multipart) — the standard library's full implementation of this same boundary-scanning problem.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-chunked-transfer-partial-frame-retain.md](13-chunked-transfer-partial-frame-retain.md) | Next: [15-rabin-rolling-window-chunking.md](15-rabin-rolling-window-chunking.md)
