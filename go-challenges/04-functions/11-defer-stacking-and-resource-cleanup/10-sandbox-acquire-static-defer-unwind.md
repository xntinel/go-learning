# Exercise 10: Sandbox Setup — Unwinding a Fixed Chain of Plain Defers

**Nivel: Intermedio** — validacion rapida (un test corto).

A worker sandbox needs three things in order: a disk quota reservation, a
scratch directory, and a process-table slot. The count is fixed and known at
compile time, so there is no need for a runtime cleanup stack — a plain
`defer` after each successful acquisition, guarded by one shared success flag,
is enough to unwind exactly what was acquired so far if a later step fails.

## What you'll build

```text
sandbox/                    independent module: example.com/sandbox
  go.mod
  sandbox/sandbox.go         Resource fake; acquireQuota/Workdir/Slot; Prepare
  sandbox/sandbox_test.go    table test over which step fails, asserting trace order
```

- Files: `sandbox/sandbox.go`, `sandbox/sandbox_test.go`.
- Implement: `Prepare(failQuota, failWorkdir, failSlot bool, trace *[]string) (*Sandbox, error)` that acquires quota, then workdir, then slot; after each success it registers a plain `defer` releasing that resource unless a shared `ok` flag has been set; `ok` flips to `true` only after the last acquisition succeeds.
- Test: one table test over which step (if any) fails, asserting the exact `trace` of acquire/release calls and that the returned `*Sandbox` is nil on error and non-nil on success.

Set up the module:

```bash
go mod edit -go=1.24
```

Create `sandbox/sandbox.go`:

```go
package sandbox

import "fmt"

// Resource is a fake acquirable thing: a disk quota reservation, a scratch
// directory, or a process-table slot. Close is idempotent.
type Resource struct {
	Name   string
	Closed bool
}

func (r *Resource) Close() {
	r.Closed = true
}

func acquireQuota(fail bool, trace *[]string) (*Resource, error) {
	if fail {
		return nil, fmt.Errorf("quota: exhausted")
	}
	*trace = append(*trace, "acquire-quota")
	return &Resource{Name: "quota"}, nil
}

func acquireWorkdir(fail bool, trace *[]string) (*Resource, error) {
	if fail {
		return nil, fmt.Errorf("workdir: mkdir failed")
	}
	*trace = append(*trace, "acquire-workdir")
	return &Resource{Name: "workdir"}, nil
}

func acquireSlot(fail bool, trace *[]string) (*Resource, error) {
	if fail {
		return nil, fmt.Errorf("slot: table full")
	}
	*trace = append(*trace, "acquire-slot")
	return &Resource{Name: "slot"}, nil
}

// Sandbox bundles the three resources a worker needs, all acquired together.
type Sandbox struct {
	Quota   *Resource
	Workdir *Resource
	Slot    *Resource
}

// Prepare acquires quota, then workdir, then slot, in that order. Because N is
// fixed and known at compile time, no cleanup-stack abstraction is needed: a
// plain defer statement follows each successful acquisition, guarded by a
// shared "ok" flag set true only once every resource is acquired. If a later
// acquisition fails, the defers already registered for the earlier ones fire
// in reverse (LIFO) automatically; if everything succeeds, ok flips and every
// defer becomes a no-op, handing the live bundle back to the caller.
func Prepare(failQuota, failWorkdir, failSlot bool, trace *[]string) (sb *Sandbox, err error) {
	q, err := acquireQuota(failQuota, trace)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			q.Close()
			*trace = append(*trace, "release-quota")
		}
	}()

	w, err := acquireWorkdir(failWorkdir, trace)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !ok {
			w.Close()
			*trace = append(*trace, "release-workdir")
		}
	}()

	s, err := acquireSlot(failSlot, trace)
	if err != nil {
		return nil, err
	}

	ok = true
	return &Sandbox{Quota: q, Workdir: w, Slot: s}, nil
}
```

Create `sandbox/sandbox_test.go`:

```go
package sandbox

import (
	"slices"
	"testing"
)

func TestPrepare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                        string
		failQuota, failWork, failSl bool
		wantErr                     bool
		wantTrace                   []string
	}{
		{
			name:      "quota fails, nothing to release",
			failQuota: true,
			wantErr:   true,
			wantTrace: nil,
		},
		{
			name:      "workdir fails, quota unwinds",
			failWork:  true,
			wantErr:   true,
			wantTrace: []string{"acquire-quota", "release-quota"},
		},
		{
			name:      "slot fails, workdir then quota unwind",
			failSl:    true,
			wantErr:   true,
			wantTrace: []string{"acquire-quota", "acquire-workdir", "release-workdir", "release-quota"},
		},
		{
			name:      "all succeed, nothing released",
			wantErr:   false,
			wantTrace: []string{"acquire-quota", "acquire-workdir", "acquire-slot"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var trace []string
			sb, err := Prepare(tt.failQuota, tt.failWork, tt.failSl, &trace)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(trace, tt.wantTrace) {
				t.Fatalf("trace = %v, want %v", trace, tt.wantTrace)
			}
			if tt.wantErr && sb != nil {
				t.Fatalf("sandbox = %+v, want nil on error", sb)
			}
			if !tt.wantErr && sb == nil {
				t.Fatal("sandbox = nil, want non-nil on success")
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

Each `defer` here is registered immediately after the resource it guards is
acquired, and every one of them checks the same shared `ok` flag before doing
anything. That single flag is the whole mechanism: it stays `false` until the
very last acquisition succeeds, so a failure at any point leaves it `false` and
every defer registered so far fires, releasing exactly the resources already
acquired in reverse order. On success it flips once, and every defer becomes a
no-op. No `Cleanup` type, `Push`, or `Run` is needed because the number of
resources is fixed and known when you write the function — that generic stack
earns its keep only when the set of things to undo is decided at runtime.

## Resources

- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Effective Go: Defer](https://go.dev/doc/effective_go#defer)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-graceful-shutdown-layered-defer.md](09-graceful-shutdown-layered-defer.md) | Next: [11-config-snapshot-restore-defer.md](11-config-snapshot-restore-defer.md)
