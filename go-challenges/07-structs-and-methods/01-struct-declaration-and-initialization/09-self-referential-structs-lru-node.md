# Exercise 9: A Self-Referential Node for an LRU Cache List

The intrusive doubly-linked list behind an LRU cache is built from a `Node` that
points at other `Node`s. A struct that refers to its own type must do so through a
**pointer** — a value field of its own type would give the struct infinite size
and fail to compile. This module builds that node and a small list, unlinks a
node, and shows the difference between value equality and pointer identity.

Fully self-contained: own `go mod init`, inline code, own demo and tests.

## What you'll build

```text
lrunode/                    independent module: example.com/lrunode
  go.mod                    go 1.24
  node.go                   type Node{Key,Value string; prev,next *Node}; List; PushFront; Unlink
  cmd/
    demo/
      main.go               builds a 3-node list, unlinks the middle, prints order
  node_test.go              re-stitch on unlink; nil head prev; identity vs value equality
```

- Files: `node.go`, `cmd/demo/main.go`, `node_test.go`.
- Implement: a `Node` with `Key`/`Value` payload and `prev`/`next *Node` links; a `List` with `PushFront` and `Unlink`, and an `Equal` helper comparing payloads.
- Test: build a 3-node list, unlink the middle node, assert `prev`/`next` re-stitch; assert a fresh head's `prev` is nil; assert two distinct `Node`s with equal payloads are `!=` by pointer but `Equal` by payload. Prose shows the compile error if `next` were a value `Node`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/lrunode/cmd/demo
cd ~/go-exercises/lrunode
go mod init example.com/lrunode
go mod edit -go=1.24
```

### Why the links must be pointers

A `Node` that stored its neighbor **by value** —

```
// invalid recursive type: Node contains itself, so it has infinite size.
type Node struct {
	Key   string
	Value string
	next  Node // compile error: invalid recursive type Node
}
```

— cannot compile. To hold a `Node` by value, the compiler must know its size; but
its size would include the size of the `next Node`, which includes its `next Node`,
forever. The compiler reports `invalid recursive type`. The fix is a **pointer**:
`next *Node` is a fixed-size machine word regardless of what it points at, so the
recursion terminates and the type has a definite size. The natural zero value of a
pointer field is `nil`, which is exactly what you want to mean "no next node" and
to represent the empty list (a `List` whose `head`/`tail` are nil).

The list here is a minimal doubly-linked list — the shape inside every LRU cache,
where `PushFront` marks an entry most-recently-used and `Unlink` removes an evicted
or moved node. `Unlink` re-stitches the neighbors: the removed node's predecessor's
`next` is set to the removed node's successor, and vice versa, with nil checks at
the ends because a boundary node has a nil neighbor. Getting those four pointer
updates right is the whole skill; the test pins them.

The second idea is **identity versus value equality**. Two separate `Node` values
can carry the same `Key`/`Value` payload yet live at different addresses. Comparing
the `*Node` pointers asks "is this the same node object" (identity); comparing
payloads with an `Equal` helper asks "do these carry the same data" (value
equality). An LRU cache cares about identity when it unlinks *the* node for a key;
a test asserting two constructions match cares about payload equality.

Create `node.go`:

```go
package node

// Node is a doubly-linked list node. The links are *Node, not Node: a value
// field of its own type would make Node infinitely sized and fail to compile.
type Node struct {
	Key   string
	Value string
	prev  *Node
	next  *Node
}

// Next and Prev expose the links for reading (unexported fields, exported API).
func (n *Node) Next() *Node { return n.next }
func (n *Node) Prev() *Node { return n.prev }

// Equal reports payload (value) equality, distinct from pointer identity.
func (n *Node) Equal(other *Node) bool {
	if n == nil || other == nil {
		return n == other
	}
	return n.Key == other.Key && n.Value == other.Value
}

// List is an intrusive doubly-linked list, the shape behind an LRU cache. Its
// zero value is an empty list (head and tail nil).
type List struct {
	head *Node
	tail *Node
}

// PushFront inserts a new node at the front (most-recently-used position) and
// returns it.
func (l *List) PushFront(key, value string) *Node {
	n := &Node{Key: key, Value: value}
	n.next = l.head
	if l.head != nil {
		l.head.prev = n
	}
	l.head = n
	if l.tail == nil {
		l.tail = n
	}
	return n
}

// Unlink removes n from the list, re-stitching its neighbors.
func (l *List) Unlink(n *Node) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		l.head = n.next // n was the head
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		l.tail = n.prev // n was the tail
	}
	n.prev = nil
	n.next = nil
}

// Head returns the front node, or nil for an empty list.
func (l *List) Head() *Node { return l.head }

// Keys walks the list front-to-back and returns the keys in order.
func (l *List) Keys() []string {
	var out []string
	for n := l.head; n != nil; n = n.next {
		out = append(out, n.Key)
	}
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/lrunode"
)

func main() {
	var l node.List
	l.PushFront("c", "3")
	mid := l.PushFront("b", "2")
	l.PushFront("a", "1")

	fmt.Println("before:", strings.Join(l.Keys(), " "))
	fmt.Println("head prev nil:", l.Head().Prev() == nil)

	l.Unlink(mid)
	fmt.Println("after unlink b:", strings.Join(l.Keys(), " "))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before: a b c
head prev nil: true
after unlink b: a c
```

### Tests

`TestUnlinkReStitches` builds a 3-node list, unlinks the middle node, and asserts
the remaining two are directly linked (the predecessor's `next` and successor's
`prev` now point at each other), and that the unlinked node's own links are
cleared. `TestHeadPrevIsNil` asserts a fresh head's `prev` is the nil zero value.
`TestIdentityVsValueEquality` constructs two distinct nodes with the same payload
and asserts they are `!=` by pointer but `Equal` by payload.

Create `node_test.go`:

```go
package node

import (
	"fmt"
	"testing"
)

func TestUnlinkReStitches(t *testing.T) {
	t.Parallel()
	var l List
	l.PushFront("c", "3")
	b := l.PushFront("b", "2")
	a := l.PushFront("a", "1")

	l.Unlink(b)

	// a and c are now adjacent.
	if a.Next() == nil || a.Next().Key != "c" {
		t.Fatalf("a.Next() = %v, want node c", a.Next())
	}
	if a.Next().Prev() != a {
		t.Fatal("c.prev should re-stitch back to a")
	}
	// The unlinked node's links are cleared.
	if b.Next() != nil || b.Prev() != nil {
		t.Fatal("unlinked node b should have nil links")
	}
	// List order reflects the removal.
	got := l.Keys()
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("keys = %v, want [a c]", got)
	}
}

func TestUnlinkHeadAndTail(t *testing.T) {
	t.Parallel()
	var l List
	l.PushFront("y", "2")
	head := l.PushFront("x", "1")

	l.Unlink(head) // remove the head
	if l.Head() == nil || l.Head().Key != "y" {
		t.Fatalf("head after unlink = %v, want y", l.Head())
	}
	if l.Head().Prev() != nil {
		t.Fatal("new head prev should be nil")
	}
}

func TestHeadPrevIsNil(t *testing.T) {
	t.Parallel()
	var l List
	l.PushFront("only", "1")
	if l.Head().Prev() != nil {
		t.Fatal("a fresh head's prev should be the nil zero value")
	}
}

func TestIdentityVsValueEquality(t *testing.T) {
	t.Parallel()
	n1 := &Node{Key: "k", Value: "v"}
	n2 := &Node{Key: "k", Value: "v"}

	if n1 == n2 {
		t.Fatal("distinct nodes must differ by pointer identity")
	}
	if !n1.Equal(n2) {
		t.Fatal("nodes with the same payload must be Equal by value")
	}
	if !n1.Equal(n1) {
		t.Fatal("a node must equal itself")
	}
}

func ExampleList() {
	var l List
	l.PushFront("b", "2")
	l.PushFront("a", "1")
	fmt.Println(l.Keys())
	// Output: [a b]
}
```

## Review

The node is correct when self-reference goes through `*Node`: value links would be
an `invalid recursive type` compile error, and pointer links give a fixed-size type
whose `nil` zero value means "no neighbor." `Unlink` is correct when it re-stitches
all four boundaries — predecessor's `next`, successor's `prev`, and the head/tail
of the list when the removed node was at an end — which the tests pin by unlinking
a middle node and a head node. The identity-versus-value distinction is the last
idea: `n1 == n2` on pointers asks whether they are the same node, while
`n1.Equal(n2)` asks whether their payloads match, and an LRU cache needs both — the
map keyed by `Key` finds the node, and pointer identity is what `Unlink` operates
on. For production, `container/list` provides a ready doubly-linked list; hand-rolling
the node is worth it for an intrusive LRU where the node lives inside the cached
value. Run `go test -race` and `go vet`.

## Resources

- [Go Spec: struct types (recursive types)](https://go.dev/ref/spec#Struct_types) — why self-reference must be through a pointer.
- [Go Spec: pointer types](https://go.dev/ref/spec#Pointer_types) — the fixed-size pointer that terminates the recursion.
- [`container/list`](https://pkg.go.dev/container/list) — the standard doubly-linked list to compare against.
- [Effective Go: pointers vs values](https://go.dev/doc/effective_go#pointers_vs_values) — identity versus value semantics.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../02-struct-tags-and-json-encoding/00-concepts.md](../02-struct-tags-and-json-encoding/00-concepts.md)
