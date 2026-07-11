# Exercise 11: Route Table Hot-Reload — Snapshot and Restore Around a Risky Mutation

**Nivel: Intermedio** — validacion rapida (un test corto).

Some `defer` cleanup has nothing to close: it has to put a value back. A live
routing table mutated in place by a hot reload is one such case — if the batch
of changes turns out invalid, or panics partway through, the table must end up
exactly as it was, not half-mutated. This module builds that snapshot/restore
around an in-memory route table.

## What you'll build

```text
routetable/                 independent module: example.com/routetable
  go.mod
  routetable/routetable.go   RouteTable, Op; Apply (snapshot before, restore on err/panic)
  routetable/routetable_test.go  table test (valid ops, invalid op midway) + a panic-restore test
```

- Files: `routetable/routetable.go`, `routetable/routetable_test.go`.
- Implement: `Apply(rt *RouteTable, ops []Op) (err error)` that snapshots `rt`'s current routes, applies each `Op` in place, and in a deferred closure restores the pre-`Apply` snapshot if `err != nil` or the batch panicked (re-raising the panic after restoring).
- Test: a table test asserting a fully valid batch is kept and an invalid op (empty host) midway restores the original map untouched; a separate test asserting a panic mid-batch also restores the original map before the panic propagates.

Set up the module:

```bash
mkdir -p ~/go-exercises/routetable/routetable
cd ~/go-exercises/routetable
go mod init example.com/routetable
go mod edit -go=1.24
```

Create `routetable/routetable.go`:

```go
package routetable

import "fmt"

// RouteTable is an in-memory hot-reloadable routing table, the kind a load
// balancer or API gateway keeps live in a running process.
type RouteTable struct {
	routes map[string]string
}

// New builds a table seeded from initial.
func New(initial map[string]string) *RouteTable {
	rt := &RouteTable{routes: make(map[string]string, len(initial))}
	for k, v := range initial {
		rt.routes[k] = v
	}
	return rt
}

// Lookup returns the backend for host, if any.
func (rt *RouteTable) Lookup(host string) (string, bool) {
	v, ok := rt.routes[host]
	return v, ok
}

// snapshot returns a deep copy of the current routes.
func (rt *RouteTable) snapshot() map[string]string {
	cp := make(map[string]string, len(rt.routes))
	for k, v := range rt.routes {
		cp[k] = v
	}
	return cp
}

// Op is one change: Backend == "" means delete Host, otherwise set/replace it.
type Op struct {
	Host    string
	Backend string
}

// Apply mutates rt in place with ops, one at a time. Before touching anything
// it takes a snapshot of the live map. If a later op turns out to be invalid
// (an empty host) or the batch panics partway through, the deferred closure
// restores rt to that snapshot -- undoing whatever partial mutation already
// happened to the live map -- and then surfaces the error or re-raises the
// panic. On full success the snapshot is simply discarded and the mutated map
// stays live. This is temp-state save/restore: unlike closing a resource,
// there is nothing to release, only a prior value to put back.
func Apply(rt *RouteTable, ops []Op) (err error) {
	before := rt.snapshot()

	defer func() {
		if p := recover(); p != nil {
			rt.routes = before
			panic(p)
		}
		if err != nil {
			rt.routes = before
		}
	}()

	for _, op := range ops {
		if op.Host == "" {
			return fmt.Errorf("apply route: empty host")
		}
		if op.Host == "__panic__" {
			panic("corrupt op")
		}
		if op.Backend == "" {
			delete(rt.routes, op.Host)
			continue
		}
		rt.routes[op.Host] = op.Backend
	}
	return nil
}
```

Create `routetable/routetable_test.go`:

```go
package routetable

import (
	"maps"
	"testing"
)

func TestApply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		initial map[string]string
		ops     []Op
		wantErr bool
		wantMap map[string]string
	}{
		{
			name:    "all valid ops are kept",
			initial: map[string]string{"a.example.com": "svc-1"},
			ops: []Op{
				{Host: "b.example.com", Backend: "svc-2"},
				{Host: "a.example.com", Backend: ""}, // delete
			},
			wantErr: false,
			wantMap: map[string]string{"b.example.com": "svc-2"},
		},
		{
			name:    "empty host midway restores original map",
			initial: map[string]string{"a.example.com": "svc-1"},
			ops: []Op{
				{Host: "b.example.com", Backend: "svc-2"},
				{Host: "", Backend: "svc-3"},
				{Host: "c.example.com", Backend: "svc-4"}, // never applied
			},
			wantErr: true,
			wantMap: map[string]string{"a.example.com": "svc-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := New(tt.initial)
			err := Apply(rt, tt.ops)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !maps.Equal(rt.routes, tt.wantMap) {
				t.Fatalf("routes = %v, want %v", rt.routes, tt.wantMap)
			}
		})
	}
}

func TestApplyPanicRestoresOriginalMap(t *testing.T) {
	t.Parallel()

	rt := New(map[string]string{"a.example.com": "svc-1"})

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = Apply(rt, []Op{
			{Host: "b.example.com", Backend: "svc-2"},
			{Host: "__panic__", Backend: "svc-3"},
		})
	}()

	want := map[string]string{"a.example.com": "svc-1"}
	if !maps.Equal(rt.routes, want) {
		t.Fatalf("routes = %v, want %v (restored)", rt.routes, want)
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The deferred closure here does two jobs depending on how `Apply` exits: on a
panic it restores `rt.routes` to `before` and then re-raises with `panic(p)`
so the caller still sees the panic; on a normal return it checks the named
`err` and restores only if `Apply` is failing. On the success path neither
branch fires, and the mutated map — already live in `rt.routes` — is simply
left in place. The snapshot is cheap here because the table is small; the same
shape applies to any in-place mutation of a shared live value where "undo"
means restoring a prior value rather than releasing a handle.

## Resources

- [maps.Equal](https://pkg.go.dev/maps#Equal)
- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [10-sandbox-acquire-static-defer-unwind.md](10-sandbox-acquire-static-defer-unwind.md) | Next: [12-pipeline-stage-resources-reverse-close.md](12-pipeline-stage-resources-reverse-close.md)
