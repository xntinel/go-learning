# Exercise 14: Resolve Inherited Permissions by Walking Up an Ancestor Chain

**Nivel: Intermedio** — validacion rapida (un test corto).

A resource hierarchy — organization, project, folder, file — lets a
permission set on an ancestor apply to everything beneath it until something
more specific overrides it. Resolving the *effective* permission for a
resource means walking upward, toward the root, rather than downward through
children, and a corrupted parent pointer must not send that walk into an
infinite loop.

This module is fully self-contained: its own `go mod init`, the resolver and
the tests inline.

## What you'll build

```text
permresolve/                 independent module: example.com/permresolve
  go.mod                      go 1.24
  permresolve.go               type Resource; func Resolve
  permresolve_test.go          inheritance chain, root default, cycle
```

Files: `permresolve.go`, `permresolve_test.go`.
Implement: `type Resource struct { ID, ParentID, Permission string }` and
`func Resolve(resources map[string]Resource, id string) (string, error)`.
Test: a four-level chain checking explicit permissions, plain inheritance, and
override at an intermediate node; a root with no explicit permission
defaulting to `"deny"`; a missing resource ID; and a cyclic parent chain
detected via `ErrCycle`.
Verify: `go test -count=1 ./...`

```bash
mkdir -p ~/go-exercises/permresolve
cd ~/go-exercises/permresolve
go mod init example.com/permresolve
go mod edit -go=1.24
```

### Recursing upward needs a simpler cycle test than recursing over a graph

This lesson's dependency-cycle detector uses three colors because a
dependency graph branches: a node can be reached by two different forward
paths, and that must read as a diamond, not a cycle. An ancestor chain has no
such branching — each resource has exactly one parent, so the walk is a
straight line, not a tree. That means a single "have I already visited this
ID during this walk" set is the entire cycle test: if `Resolve` is asked to
visit an ID it has already visited while climbing the same chain, the parent
pointers must loop, and there is no diamond case to rule out.

Create `permresolve.go`:

```go
package permresolve

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned when a resource ID is missing from the registry,
// including a parent ID left dangling by a bad migration.
var ErrNotFound = errors.New("permresolve: resource not found")

// ErrCycle is returned when the parent chain loops back on itself. A single
// visited set is enough here, unlike the three-color DFS used for a
// branching dependency graph elsewhere in this lesson: an ancestor chain is a
// simple linked list, not a tree with fan-out, so "have I seen this ID before
// on this walk" is the whole cycle test.
var ErrCycle = errors.New("permresolve: cycle in parent chain")

// Resource is one node in a resource hierarchy (a folder, a project, an
// organization). An empty Permission means "inherit from ParentID"; an empty
// ParentID means "this is the root".
type Resource struct {
	ID         string
	ParentID   string
	Permission string
}

// Resolve returns the effective permission for id by walking up the parent
// chain until it finds an explicit permission or reaches the root. A root
// with no explicit permission defaults to "deny".
func Resolve(resources map[string]Resource, id string) (string, error) {
	return resolve(resources, id, make(map[string]bool))
}

func resolve(resources map[string]Resource, id string, visited map[string]bool) (string, error) {
	if visited[id] {
		return "", fmt.Errorf("%s: %w", id, ErrCycle)
	}
	visited[id] = true

	r, ok := resources[id]
	if !ok {
		return "", fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	if r.Permission != "" {
		return r.Permission, nil
	}
	if r.ParentID == "" {
		return "deny", nil
	}
	return resolve(resources, r.ParentID, visited)
}
```

### Tests

The table walks a four-level chain (`org` -> `project` -> `folder` -> `file`)
checking an explicit permission at the root, plain inheritance through a node
with none, an override at `folder`, and a leaf inheriting that override. A
separate root with no explicit permission checks the `"deny"` default, and a
missing ID checks `ErrNotFound`. A second test builds a three-node cycle and
checks `ErrCycle`.

Create `permresolve_test.go`:

```go
package permresolve

import (
	"errors"
	"testing"
)

func TestResolve(t *testing.T) {
	resources := map[string]Resource{
		"org":                {ID: "org", ParentID: "", Permission: "read"},
		"project":            {ID: "project", ParentID: "org", Permission: ""},
		"folder":             {ID: "folder", ParentID: "project", Permission: "write"},
		"file":               {ID: "file", ParentID: "folder", Permission: ""},
		"orphan-no-explicit": {ID: "orphan-no-explicit", ParentID: "", Permission: ""},
	}

	tests := []struct {
		name    string
		id      string
		want    string
		wantErr error
	}{
		{"explicit at root", "org", "read", nil},
		{"inherits from grandparent", "project", "read", nil},
		{"explicit override wins", "folder", "write", nil},
		{"inherits from parent's explicit", "file", "write", nil},
		{"root with no explicit permission defaults to deny", "orphan-no-explicit", "deny", nil},
		{"missing resource", "does-not-exist", "", ErrNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(resources, tc.id)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Resolve(%q) error = %v, want %v", tc.id, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q) error = %v, want nil", tc.id, err)
			}
			if got != tc.want {
				t.Errorf("Resolve(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

func TestResolveDetectsCycle(t *testing.T) {
	resources := map[string]Resource{
		"a": {ID: "a", ParentID: "b"},
		"b": {ID: "b", ParentID: "c"},
		"c": {ID: "c", ParentID: "a"},
	}

	_, err := Resolve(resources, "a")
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("Resolve() error = %v, want %v", err, ErrCycle)
	}
}
```

Run it: `go test -count=1 ./...`

## Review

`Resolve` recurses in the opposite direction from every other exercise in
this lesson: toward the root of a chain instead of out toward the leaves of a
tree. The base cases are symmetric with that direction — an explicit
permission or an unparented root stop the climb — and the visited set is the
minimum machinery needed to guarantee termination on a chain that a bad
migration or a bug left cyclic. The table's five inheritance cases and the
separate cycle test between them pin down both halves: what the resolver
returns when the data is sane, and that it fails safely, not infinitely, when
it is not.

## Resources

- [Go Specification: Map types](https://go.dev/ref/spec#Map_types)
- [errors.Is](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-comment-thread-render-with-truncation.md](13-comment-thread-render-with-truncation.md) | Next: [15-binary-tree-insertion-recursive-balance.md](15-binary-tree-insertion-recursive-balance.md)
