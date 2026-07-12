# Exercise 3: Hand a Sub-Slice of a Read Buffer With `s[lo:hi:hi]`

A frame parser reads a fixed record into one buffer and hands each field region to
a downstream consumer that may append. If it hands out two-index slices, the
consumer's `append` writes into the shared backing array and stomps the next
field. This exercise uses the full three-index expression `buf[off:end:end]` so
`cap == len` and the consumer's `append` is forced to reallocate — and proves both
the safe path and the corruption the two-index form causes.

Self-contained module: own `go mod init`, own demo, own tests.

## What you'll build

```text
framer/                    independent module: example.com/framer
  go.mod                   go 1.26
  framer.go                type Field; Parse (three-index), parseAliased (two-index bug)
  cmd/
    demo/
      main.go              parse a record, append to one field, show the rest intact
  framer_test.go           isolation test, two-index corruption test, cap==len assertion
```

Files: `framer.go`, `cmd/demo/main.go`, `framer_test.go`.
Implement: `Parse(buf, fields)` returning each field as `buf[off:end:end]`; a buggy `parseAliased` returning `buf[off:end]`.
Test: hand field A via three-index, append to it, assert field B in the buffer is untouched and `cap(handed) == len(handed)`; a negative test shows the two-index version corrupts B.
Verify: `go test -count=1 -race ./...`

### Why three-index, exactly

Reading a record once into a single buffer and slicing out each field is the right
performance move — one allocation, no per-field copies. The hazard is what the
consumer does with a field. A two-index slice `buf[off:end]` inherits the parent
buffer's capacity: its `cap` runs all the way to the end of `buf`, so it has spare
room pointing straight at the *next* field. When the consumer calls
`append(field, x)`, `append` sees spare capacity and writes `x` in place — into
the first byte of the next field, which is still live. The corruption is silent
and non-local: the field that changes is not the one the consumer touched.

The three-index expression `buf[off:end:end]` sets `cap == len == end - off`. The
handed slice has zero spare capacity, so the consumer's first `append` must
allocate a fresh backing array and copy — it cannot reach into `buf` at all. You
have handed a read-only-shaped window: the consumer can still overwrite indices
`[0, len)` of what it was given (three-index is not memory safety, only append
isolation), but it can no longer corrupt neighbors by appending. When the consumer
genuinely must not mutate even its own region, clone instead; three-index is the
cheap tool for the common "consumer appends" case where no copy is otherwise
needed.

Create `framer.go`:

```go
package framer

// Field names a fixed region of a record buffer.
type Field struct {
	Name string
	Off  int
	End  int
}

// Parse returns each field as a three-index sub-slice of buf, so a consumer's
// append reallocates instead of overwriting the adjacent field.
func Parse(buf []byte, fields []Field) map[string][]byte {
	out := make(map[string][]byte, len(fields))
	for _, f := range fields {
		out[f.Name] = buf[f.Off:f.End:f.End]
	}
	return out
}

// parseAliased is the buggy variant used only in tests: two-index slices carry
// the parent buffer's capacity, so a consumer append stomps the next field.
func parseAliased(buf []byte, fields []Field) map[string][]byte {
	out := make(map[string][]byte, len(fields))
	for _, f := range fields {
		out[f.Name] = buf[f.Off:f.End]
	}
	return out
}
```

### The runnable demo

The demo parses a packed record `user=alice`, `role=admin` living back-to-back in
one buffer, appends a `!` marker to the parsed `user` field, and prints the `role`
field to show it is untouched because the three-index `append` reallocated.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/framer"
)

func main() {
	// One buffer, two adjacent fields: "alice" (0:5) then "admin" (5:10).
	buf := []byte("aliceadmin")
	fields := []framer.Field{
		{Name: "user", Off: 0, End: 5},
		{Name: "role", Off: 5, End: 10},
	}

	parsed := framer.Parse(buf, fields)

	// Downstream consumer appends a marker to the user field.
	user := append(parsed["user"], '!')

	fmt.Printf("user handed len=%d cap=%d\n", len(parsed["user"]), cap(parsed["user"]))
	fmt.Printf("user after append: %s\n", user)
	fmt.Printf("role still intact:  %s\n", parsed["role"])
	fmt.Printf("buffer untouched:   %s\n", buf)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user handed len=5 cap=5
user after append: alice!
role still intact:  admin
buffer untouched:   aliceadmin
```

### Tests

`TestThreeIndexIsolatesNextField` appends to the parsed field and asserts the next
field's bytes in the original buffer are untouched, plus `cap == len` on the
handed slice. `TestTwoIndexCorruptsNextField` uses the buggy `parseAliased` and
asserts the append DID overwrite the next field's first byte, making the failure
mode explicit.

Create `framer_test.go`:

```go
package framer

import (
	"bytes"
	"testing"
)

var fields = []Field{
	{Name: "user", Off: 0, End: 5},
	{Name: "role", Off: 5, End: 10},
}

func TestThreeIndexIsolatesNextField(t *testing.T) {
	t.Parallel()

	buf := []byte("aliceadmin")
	parsed := Parse(buf, fields)

	user := parsed["user"]
	if len(user) != cap(user) {
		t.Fatalf("handed field should have cap==len; got len=%d cap=%d", len(user), cap(user))
	}

	_ = append(user, '!') // consumer appends; must reallocate

	if got := string(buf[5:10]); got != "admin" {
		t.Fatalf("next field corrupted by three-index append: got %q, want %q", got, "admin")
	}
	if !bytes.Equal(buf, []byte("aliceadmin")) {
		t.Fatalf("buffer mutated: %q", buf)
	}
}

func TestTwoIndexCorruptsNextField(t *testing.T) {
	t.Parallel()

	buf := []byte("aliceadmin")
	parsed := parseAliased(buf, fields)

	user := parsed["user"]
	if cap(user) == len(user) {
		t.Fatalf("two-index field unexpectedly had cap==len (%d)", cap(user))
	}

	_ = append(user, '!') // consumer appends; stomps the next field in place

	if got := buf[5]; got != '!' {
		t.Fatalf("expected two-index append to corrupt buf[5] to '!', got %q", got)
	}
}

func TestParsePreservesFieldContents(t *testing.T) {
	t.Parallel()

	buf := []byte("aliceadmin")
	parsed := Parse(buf, fields)
	if got := string(parsed["user"]); got != "alice" {
		t.Fatalf("user = %q, want alice", got)
	}
	if got := string(parsed["role"]); got != "admin" {
		t.Fatalf("role = %q, want admin", got)
	}
}
```

## Review

The parser is correct when a consumer appending to one field cannot touch another:
`TestThreeIndexIsolatesNextField` proves the adjacent field survives an `append`
and that the handed slice has `cap == len`. The negative
`TestTwoIndexCorruptsNextField` proves the two-index form does the opposite — its
`append` writes into `buf[5]` — so the reason for the three-index expression is
demonstrated, not merely asserted. The subtle point to internalize: three-index
isolates *append*, not all writes; a consumer can still overwrite the bytes it was
handed. When the region must be fully immutable to the consumer, clone it. Reach
for `buf[off:end:end]` when the consumer only appends and you want to avoid the
copy.

## Resources

- [Go Specification: Slice expressions (full slice expression `a[low:high:max]`)](https://go.dev/ref/spec#Slice_expressions)
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-repository-getall-defensive-clone.md](02-repository-getall-defensive-clone.md) | Next: [04-append-shared-backing-array-corruption.md](04-append-shared-backing-array-corruption.md)
