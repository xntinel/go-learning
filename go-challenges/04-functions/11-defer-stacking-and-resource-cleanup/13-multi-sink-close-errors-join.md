# Exercise 13: Multi-Sink Write — Joining Three Independently Deferred Closes

**Nivel: Intermedio** — validacion rapida (un test corto).

A report that fans out to a primary store, an audit log, and a metrics
counter has three independent things to close, not one. This module defers
each sink's `Close` separately — three deferred closures, each folding its
error into the same named return via `errors.Join` — and shows they still run
in LIFO order and all three always run, however the write itself failed.

## What you'll build

```text
multisink/                  independent module: example.com/multisink
  go.mod
  multisink/multisink.go     Sink fake; WriteAll (three independent deferred Close+Join)
  multisink/multisink_test.go  table test: success, a write error, a close error
```

- Files: `multisink/multisink.go`, `multisink/multisink_test.go`.
- Implement: `WriteAll(primary, audit, metrics *Sink, line string) (err error)` that writes `line` to all three sinks in order and defers one closure per sink — registered primary, then audit, then metrics, so they run metrics-first, audit-second, primary-third — each joining its `Close` error into `err` via `errors.Join`.
- Test: one table test covering a clean success, a write failure on one sink (all three still close), and a `Close` failure on one sink (joined with any write error, or alone if the write succeeded); assert with `errors.Is` and that every sink's `Closed` field ended up `true`.

Set up the module:

```bash
go mod edit -go=1.24
```

Create `multisink/multisink.go`:

```go
package multisink

import (
	"errors"
	"fmt"
)

// Sink is a fake write destination: a primary store, an audit log, a metrics
// counter. WriteErr/CloseErr let a test inject a failure on either call.
type Sink struct {
	Name     string
	Writes   []string
	Closed   bool
	WriteErr error
	CloseErr error
}

func (s *Sink) Write(line string) error {
	if s.WriteErr != nil {
		return s.WriteErr
	}
	s.Writes = append(s.Writes, line)
	return nil
}

func (s *Sink) Close() error {
	s.Closed = true
	return s.CloseErr
}

// WriteAll writes line to primary, then audit, then metrics, and defers each
// sink's Close independently -- one deferred closure per sink, each joining
// its Close error into the single named return err via errors.Join. Because
// the three defers are registered in acquisition order, they run in reverse
// (LIFO): metrics closes first, then audit, then primary. Every sink is
// closed exactly once no matter which write failed, and every Close error is
// preserved alongside any write error -- none of them silently disappears the
// way a bare `defer sink.Close()` would.
func WriteAll(primary, audit, metrics *Sink, line string) (err error) {
	defer func() { err = errors.Join(err, metrics.Close()) }()
	defer func() { err = errors.Join(err, audit.Close()) }()
	defer func() { err = errors.Join(err, primary.Close()) }()

	if werr := primary.Write(line); werr != nil {
		return fmt.Errorf("primary write: %w", werr)
	}
	if werr := audit.Write(line); werr != nil {
		return fmt.Errorf("audit write: %w", werr)
	}
	if werr := metrics.Write(line); werr != nil {
		return fmt.Errorf("metrics write: %w", werr)
	}
	return nil
}
```

Create `multisink/multisink_test.go`:

```go
package multisink

import (
	"errors"
	"testing"
)

func TestWriteAll(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("audit disk full")
	closeErr := errors.New("metrics flush failed")

	tests := []struct {
		name    string
		primary *Sink
		audit   *Sink
		metrics *Sink
		wantErr error // nil means "no error expected"
	}{
		{
			name:    "success closes all three, no error",
			primary: &Sink{Name: "primary"},
			audit:   &Sink{Name: "audit"},
			metrics: &Sink{Name: "metrics"},
			wantErr: nil,
		},
		{
			name:    "audit write fails, all three still close",
			primary: &Sink{Name: "primary"},
			audit:   &Sink{Name: "audit", WriteErr: writeErr},
			metrics: &Sink{Name: "metrics"},
			wantErr: writeErr,
		},
		{
			name:    "metrics close fails, joined with nil write error",
			primary: &Sink{Name: "primary"},
			audit:   &Sink{Name: "audit"},
			metrics: &Sink{Name: "metrics", CloseErr: closeErr},
			wantErr: closeErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := WriteAll(tt.primary, tt.audit, tt.metrics, "line-1")

			if tt.wantErr == nil && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want errors.Is %v", err, tt.wantErr)
			}
			if !tt.primary.Closed || !tt.audit.Closed || !tt.metrics.Closed {
				t.Fatalf("not all sinks closed: primary=%v audit=%v metrics=%v",
					tt.primary.Closed, tt.audit.Closed, tt.metrics.Closed)
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

Each of the three deferred closures reads and rewrites the same named `err`,
so they compose exactly like the single-resource `errors.Join` pattern except
there are three of them stacked. Their registration order determines their
run order (LIFO), which is why metrics — registered last — closes first; the
practical effect is that all three always run regardless of where the write
loop returned, and no single `Close` error can hide another. This is the shape
to reach for whenever a function owns more than one independent closer and
must report every failure, not just the first one it notices.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join)
- [errors.Is](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [12-pipeline-stage-resources-reverse-close.md](12-pipeline-stage-resources-reverse-close.md) | Next: [14-staged-write-discard-unless-committed.md](14-staged-write-discard-unless-committed.md)
