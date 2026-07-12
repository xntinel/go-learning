# Exercise 16: Stream XML Elements with Nesting Depth Guard

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

`encoding/xml`'s `Decoder.Token` reads one XML token at a time without
loading the whole document into memory — a good defense against a huge
payload. But nothing about token streaming stops a document from nesting
elements a million levels deep, and a recursive-descent parser that follows
every nested `StartElement` with another function call will exhaust the
goroutine stack on such a document just as surely as a naive one that
buffers everything. The fix is the same idea as JSON depth guarding: recurse,
but refuse to go past a maximum depth.

This module is fully self-contained: its own `go mod init`, the flattener
inline, its own demo and tests.

## What you'll build

```text
xmlflatten/                  independent module: example.com/xmlflatten
  go.mod                      go 1.24
  xmlflatten.go                type Registry; func Flatten
  xmlflatten_test.go           counts, empty doc, over-depth, exact-limit, malformed XML
  cmd/
    demo/
      main.go                  flattens a small order document, prints counts
```

- Files: `xmlflatten.go`, `cmd/demo/main.go`, `xmlflatten_test.go`.
- Implement: `type Registry map[string]int` and
  `func Flatten(r io.Reader, maxDepth int) (Registry, error)` that streams
  `r` via `xml.Decoder.Token` and recurses one level per nested element,
  rejecting documents deeper than `maxDepth` with `ErrTooDeep`.
- Test: nested element counts are correct; an empty document returns an
  empty registry; a document nested past the limit is rejected; a document
  at exactly the limit is accepted; malformed XML (mismatched tags) surfaces
  the decoder's error.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Streaming bounds memory; recursion depth still needs its own bound

`xml.Decoder.Token` solves one problem: it never holds the whole document
in memory, because it hands you tokens one at a time as it reads. That is
orthogonal to a second problem — how deep the *call stack* goes while
processing those tokens. A recursive-descent flattener naturally mirrors
the document's nesting: every `StartElement` it sees causes one more
function call, and that call does not return until the matching
`EndElement` is read. A document with ten thousand elements nested inside
each other, one per level, produces ten thousand stacked calls before any
of them returns — regardless of how memory-efficient the token reads
themselves are. Streaming and stack depth are two separate resources, and
guarding one does not guard the other.

The guard here works the same way the JSON depth guard elsewhere in this
lesson does: thread a `depth` counter through the recursive calls, and
refuse to descend further once it exceeds the caller's limit. The check
happens at the top of `descend`, before any more tokens for that subtree
are read — so an attacker cannot make the flattener do meaningfully more
work than `maxDepth` allows, no matter how deep the actual document goes.

Create `xmlflatten.go`:

```go
// Package xmlflatten streams an XML document token by token and flattens its
// nested elements into a flat element-name registry, using recursive descent
// bounded by a maximum nesting depth so a maliciously (or accidentally) deep
// document cannot exhaust the goroutine stack.
package xmlflatten

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
)

// ErrTooDeep is returned when an element nests deeper than the configured
// maximum, before any further tokens for that subtree are read.
var ErrTooDeep = errors.New("xmlflatten: nesting depth exceeds limit")

// Registry counts how many times each element name appears anywhere in the
// document, at any depth.
type Registry map[string]int

// Flatten streams r as XML and returns a Registry of element name counts. It
// rejects documents that nest more than maxDepth levels deep with ErrTooDeep
// rather than continuing to recurse.
func Flatten(r io.Reader, maxDepth int) (Registry, error) {
	dec := xml.NewDecoder(r)
	registry := make(Registry)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return registry, nil
		}
		if err != nil {
			return nil, fmt.Errorf("xmlflatten: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		registry[start.Name.Local]++
		if err := descend(dec, registry, 1, maxDepth); err != nil {
			return nil, err
		}
	}
}

// descend consumes tokens belonging to the element whose StartElement was
// already read by the caller, recursing one level for every nested child
// StartElement it encounters, until the matching EndElement closes it.
// depth is the nesting level of the element just entered (the document root
// is depth 1); once depth exceeds maxDepth, descend refuses to recurse
// further and returns ErrTooDeep instead of reading the rest of the subtree.
func descend(dec *xml.Decoder, registry Registry, depth, maxDepth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: depth %d", ErrTooDeep, depth)
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("xmlflatten: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			registry[t.Name.Local]++
			if err := descend(dec, registry, depth+1, maxDepth); err != nil {
				return err
			}
		case xml.EndElement:
			return nil
		}
	}
}
```

### The runnable demo

The demo flattens a small order document — a customer and two line items —
and prints each element name's count in sorted order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"
	"strings"

	"example.com/xmlflatten"
)

func main() {
	doc := strings.NewReader(`
<order>
	<customer><name>Ada</name></customer>
	<items>
		<item><sku>A1</sku></item>
		<item><sku>A2</sku></item>
	</items>
</order>`)

	registry, err := xmlflatten.Flatten(doc, 10)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%s: %d\n", name, registry[name])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
customer: 1
item: 2
items: 1
name: 1
order: 1
sku: 2
```

### Tests

`TestFlattenCountsNestedElements` checks the basic counting over a small
nested document. `TestFlattenEmptyDocument` checks the zero-token case
returns cleanly. `TestFlattenRejectsOverDepthDocument` builds a document
nested 20 levels deep against a limit of 10 and expects `ErrTooDeep`.
`TestFlattenAcceptsDocumentAtExactLimit` is the boundary case — a document
exactly as deep as the limit must be accepted, not rejected. `TestFlattenMalformedXML`
checks that a mismatched closing tag surfaces as an error rather than being
silently swallowed.

Create `xmlflatten_test.go`:

```go
package xmlflatten

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFlattenCountsNestedElements(t *testing.T) {
	t.Parallel()

	doc := strings.NewReader(`<a><b><c/><c/></b><b></b></a>`)
	registry, err := Flatten(doc, 10)
	if err != nil {
		t.Fatalf("Flatten() error = %v", err)
	}

	want := Registry{"a": 1, "b": 2, "c": 2}
	for name, count := range want {
		if registry[name] != count {
			t.Errorf("registry[%q] = %d, want %d", name, registry[name], count)
		}
	}
	if len(registry) != len(want) {
		t.Errorf("registry = %v, want %v", registry, want)
	}
}

func TestFlattenEmptyDocument(t *testing.T) {
	t.Parallel()

	registry, err := Flatten(strings.NewReader(""), 10)
	if err != nil {
		t.Fatalf("Flatten() error = %v", err)
	}
	if len(registry) != 0 {
		t.Fatalf("registry = %v, want empty", registry)
	}
}

func TestFlattenRejectsOverDepthDocument(t *testing.T) {
	t.Parallel()

	// Build a document nested 20 levels deep: <l0><l1>...<l19/>...</l1></l0>
	var open, close strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&open, "<l%d>", i)
		fmt.Fprintf(&close, "</l%d>", 19-i)
	}
	doc := strings.NewReader(open.String() + close.String())

	_, err := Flatten(doc, 10)
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("Flatten() error = %v, want %v", err, ErrTooDeep)
	}
}

func TestFlattenAcceptsDocumentAtExactLimit(t *testing.T) {
	t.Parallel()

	// Exactly 3 levels deep: root at depth 1, child at depth 2, grandchild
	// at depth 3.
	doc := strings.NewReader(`<a><b><c/></b></a>`)
	registry, err := Flatten(doc, 3)
	if err != nil {
		t.Fatalf("Flatten() error = %v, want nil at exact limit", err)
	}
	if registry["c"] != 1 {
		t.Fatalf("registry = %v, want c:1", registry)
	}
}

func TestFlattenMalformedXML(t *testing.T) {
	t.Parallel()

	_, err := Flatten(strings.NewReader("<a><b></a>"), 10)
	if err == nil {
		t.Fatal("expected an error for mismatched closing tag")
	}
}
```

Run it: `go test -count=1 ./...`

## Review

`Flatten` is correct when it produces the same element counts a full DOM
parse would, while never holding more than `maxDepth` stack frames of
recursion at once. `TestFlattenRejectsOverDepthDocument` and
`TestFlattenAcceptsDocumentAtExactLimit` together pin down the boundary
exactly: the limit rejects one level past it and accepts right at it,
which is the off-by-one every depth guard risks getting wrong. The mistake
this exercise targets is treating "streams tokens instead of building a
DOM" as if it already solved the resource-exhaustion problem — it solves
the memory half, but a recursive-descent consumer of that stream still
needs its own explicit depth bound, or an attacker-supplied document with
deep, otherwise-tiny nesting will exhaust the stack exactly as it would
against a buffering parser.

## Resources

- [encoding/xml package (Decoder.Token)](https://pkg.go.dev/encoding/xml#Decoder.Token)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [errors.Is and error wrapping with %w](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-binary-tree-insertion-recursive-balance.md](15-binary-tree-insertion-recursive-balance.md) | Next: [17-regex-backtracking-memoization-table.md](17-regex-backtracking-memoization-table.md)
