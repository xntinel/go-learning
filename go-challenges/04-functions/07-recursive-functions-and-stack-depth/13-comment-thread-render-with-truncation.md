# Exercise 13: Render a Comment Thread with Graceful Depth Truncation

**Nivel: Intermedio** — validacion rapida (un test corto).

A discussion thread — a code review, a forum post — nests replies inside
replies with no fixed limit. This module renders one indented line per
comment, but past a maximum display depth it collapses the remaining subtree
into a single "N more replies" line instead of erroring, because a display
concern calls for graceful truncation, not rejection.

This module is fully self-contained: its own `go mod init`, the renderer and
the tests inline.

## What you'll build

```text
commentthread/               independent module: example.com/commentthread
  go.mod                      go 1.24
  commentthread.go            type Comment; func Render
  commentthread_test.go        full depth, truncated, single leaf comment
```

Files: `commentthread.go`, `commentthread_test.go`.
Implement: `type Comment struct { Author, Body string; Replies []*Comment }`
and `func Render(c *Comment, maxDepth int) []string`.
Test: a four-level thread rendered in full at a generous `maxDepth`, the same
thread truncated at `maxDepth = 1` with a correct "more replies" count, and a
single leaf comment producing exactly one line with no truncation marker.
Verify: `go test -count=1 ./...`

```bash
mkdir -p go-solutions/04-functions/07-recursive-functions-and-stack-depth/13-comment-thread-render-with-truncation
cd go-solutions/04-functions/07-recursive-functions-and-stack-depth/13-comment-thread-render-with-truncation
go mod edit -go=1.24
```

### Truncate gracefully, do not reject

Every other depth-bounded exercise in this lesson returns a sentinel error the
instant the cap is crossed, because those are safety guards over data whose
depth might be adversarial. Rendering a comment thread is a different kind of
bound: the thread itself is legitimate, deeply nested content a real user
wrote, and cutting it off with an error would just be a broken page. So
`Render` keeps recursing exactly like a plain tree walk until it reaches
`maxDepth`, and only then stops descending — folding everything below that
point into a count via a small second recursion, `countAll`, rather than
discarding it silently or failing the whole render.

Create `commentthread.go`:

```go
package commentthread

import (
	"fmt"
	"strings"
)

// Comment is one node of a threaded discussion: a reply chain of arbitrary
// depth, exactly like a forum or code-review comment tree.
type Comment struct {
	Author  string
	Body    string
	Replies []*Comment
}

// Render returns c and its replies as indented lines, two spaces per depth
// level. This is a display concern, not a safety guard: replies beyond
// maxDepth are collapsed into a single "N more replies" line instead of being
// rejected, unlike the hard errors used to bound recursion over untrusted
// data elsewhere in this lesson.
func Render(c *Comment, maxDepth int) []string {
	return render(c, 0, maxDepth)
}

func render(c *Comment, depth int, maxDepth int) []string {
	indent := strings.Repeat("  ", depth)
	lines := []string{fmt.Sprintf("%s%s: %s", indent, c.Author, c.Body)}

	if depth >= maxDepth {
		if n := countAll(c.Replies); n > 0 {
			lines = append(lines, fmt.Sprintf("%s  ... %d more replies", indent, n))
		}
		return lines
	}

	for _, r := range c.Replies {
		lines = append(lines, render(r, depth+1, maxDepth)...)
	}
	return lines
}

// countAll returns the total number of comments in the given forest,
// including every nested reply.
func countAll(cs []*Comment) int {
	n := len(cs)
	for _, c := range cs {
		n += countAll(c.Replies)
	}
	return n
}
```

### Tests

`sampleThread` builds a four-comment chain (root, reply, reply-to-reply,
reply-to-that). One test renders it at a depth generous enough to show every
line; a second renders it at `maxDepth = 1` and checks the truncation line
reports the correct count of collapsed descendants; a third confirms a single
leaf comment produces one line with no truncation marker at all.

Create `commentthread_test.go`:

```go
package commentthread

import (
	"reflect"
	"testing"
)

func sampleThread() *Comment {
	return &Comment{
		Author: "alice", Body: "root",
		Replies: []*Comment{
			{
				Author: "bob", Body: "reply1",
				Replies: []*Comment{
					{
						Author: "carol", Body: "reply1.1",
						Replies: []*Comment{
							{Author: "dave", Body: "reply1.1.1"},
						},
					},
				},
			},
		},
	}
}

func TestRenderFullDepth(t *testing.T) {
	got := Render(sampleThread(), 10)
	want := []string{
		"alice: root",
		"  bob: reply1",
		"    carol: reply1.1",
		"      dave: reply1.1.1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Render() = %#v, want %#v", got, want)
	}
}

func TestRenderTruncatesBeyondMaxDepth(t *testing.T) {
	got := Render(sampleThread(), 1)
	want := []string{
		"alice: root",
		"  bob: reply1",
		"    ... 2 more replies",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Render() = %#v, want %#v", got, want)
	}
}

func TestRenderLeafHasNoTruncationLine(t *testing.T) {
	leaf := &Comment{Author: "solo", Body: "no replies"}
	got := Render(leaf, 0)
	want := []string{"solo: no replies"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Render() = %#v, want %#v", got, want)
	}
}
```

Run it: `go test -count=1 ./...`

## Review

`render` is the familiar depth-first recursion, but the stopping condition is
a display choice rather than a rejection: at `depth >= maxDepth` it stops
descending and reports how much it chose not to show, via the independent
`countAll` recursion over the collapsed subtree. Contrasting this with the
earlier `ErrMaxDepthExceeded` exercises makes the underlying decision
explicit: bound depth with an error when the data might be hostile, bound it
with graceful truncation when the data is legitimate and the limit is about
what to display, not what to trust.

## Resources

- [strings.Repeat](https://pkg.go.dev/strings#Repeat)
- [Go Specification: Slice types](https://go.dev/ref/spec#Slice_types)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-storage-prefix-tree-size-rollup.md](12-storage-prefix-tree-size-rollup.md) | Next: [14-permission-inheritance-ancestor-walk.md](14-permission-inheritance-ancestor-walk.md)
