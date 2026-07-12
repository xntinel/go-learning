# Exercise 12: Build Pipeline — Holding Every Stage's Resource Open Until the End

**Nivel: Intermedio** — validacion rapida (un test corto).

Not every multi-resource loop is a leak. A build pipeline where a later stage
reads an earlier stage's cache genuinely needs every opened resource to stay
alive until the whole run finishes. This module collects each stage's resource
into a slice as it opens and closes all of them together, in reverse, in one
final deferred loop — legitimate retention, not the loop-leak anti-pattern.

## What you'll build

```text
pipeline/                   independent module: example.com/pipeline
  go.mod
  pipeline/pipeline.go       Layer fake; Stage; Run (accumulate then close-in-reverse)
  pipeline/pipeline_test.go  table test: all succeed vs. a middle stage fails
```

- Files: `pipeline/pipeline.go`, `pipeline/pipeline_test.go`.
- Implement: `Run(stages []Stage) (closedOrder []string, err error)` that opens each stage's `*Layer` in order, appending it to a slice, and defers a single closure that closes every accumulated layer in reverse index order when `Run` returns, whether it succeeded or failed partway.
- Test: one table test with all stages succeeding (closed order is the exact reverse of open order) and a middle stage failing (only the layers actually opened before the failure get closed, still in reverse).

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/12-pipeline-stage-resources-reverse-close/pipeline
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/12-pipeline-stage-resources-reverse-close
go mod edit -go=1.24
```

Create `pipeline/pipeline.go`:

```go
package pipeline

import "fmt"

// Layer is a fake per-stage resource: a build cache that a later stage may
// need to read from while it is still open.
type Layer struct {
	Name   string
	Closed bool
}

func (l *Layer) Close() { l.Closed = true }

// Stage opens one layer. Open receives every layer opened by earlier stages,
// because a real stage (a linker reading an earlier compiler's cache, say)
// may need them.
type Stage struct {
	Name string
	Open func(prev []*Layer) (*Layer, error)
}

// Run executes stages in order. Unlike a loop that must release each
// resource at the end of its own iteration (the classic defer-in-a-loop leak),
// this pipeline genuinely needs every opened layer to stay alive until the
// whole run finishes, since a later stage may read an earlier one. So layers
// are appended to a slice as they open, and a single deferred loop closes all
// of them together, in reverse acquisition order, when Run returns -- on
// success or on a mid-pipeline failure alike.
func Run(stages []Stage) (closedOrder []string, err error) {
	var layers []*Layer
	defer func() {
		for i := len(layers) - 1; i >= 0; i-- {
			layers[i].Close()
			closedOrder = append(closedOrder, layers[i].Name)
		}
	}()

	for _, s := range stages {
		l, serr := s.Open(layers)
		if serr != nil {
			err = fmt.Errorf("stage %s: %w", s.Name, serr)
			return
		}
		layers = append(layers, l)
	}
	return
}
```

Create `pipeline/pipeline_test.go`:

```go
package pipeline

import (
	"errors"
	"slices"
	"testing"
)

func mkStage(name string, fail bool) Stage {
	return Stage{
		Name: name,
		Open: func(prev []*Layer) (*Layer, error) {
			if fail {
				return nil, errors.New("open failed")
			}
			return &Layer{Name: name}, nil
		},
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		stages     []Stage
		wantErr    bool
		wantClosed []string
	}{
		{
			name: "all stages succeed, closed in reverse",
			stages: []Stage{
				mkStage("compile", false),
				mkStage("link", false),
				mkStage("package", false),
			},
			wantErr:    false,
			wantClosed: []string{"package", "link", "compile"},
		},
		{
			name: "middle stage fails, only opened layers close in reverse",
			stages: []Stage{
				mkStage("compile", false),
				mkStage("link", true),
				mkStage("package", false), // never opened
			},
			wantErr:    true,
			wantClosed: []string{"compile"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closed, err := Run(tt.stages)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(closed, tt.wantClosed) {
				t.Fatalf("closed = %v, want %v", closed, tt.wantClosed)
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

The key difference from a defer-in-a-loop bug is intent: here the layers slice
is deliberately accumulated across the whole loop because later stages
genuinely need earlier ones alive, so a single deferred closure at the end —
not one defer per iteration — is the correct shape, and it still gives LIFO
release order by walking the slice backward. Note that `closedOrder` is a
named return set by the deferred closure after the `return` statement inside
the loop already assigned `err`; the deferred assignment to `closedOrder`
still lands, because a deferred function can modify named returns right up
until the function actually hands control back to its caller.

## Resources

- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [11-config-snapshot-restore-defer.md](11-config-snapshot-restore-defer.md) | Next: [13-multi-sink-close-errors-join.md](13-multi-sink-close-errors-join.md)
