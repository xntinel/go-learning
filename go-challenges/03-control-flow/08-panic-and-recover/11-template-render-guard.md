# Exercise 11: Report Field Renderer: Recovering a Panicking Transform Without Leaking Partial Output

**Nivel: Intermedio** — validacion rapida (un test corto).

A report renderer walks a list of fields, applying a named transform to each
raw value — uppercase a name, truncate a code, format a percentage. Transforms
are small functions other teams contribute to a shared registry, and one
sloppy transform (a string slice with no bounds check, a division with no
zero check) must not corrupt the report or crash the job that builds it. This
module builds `Renderer.Render`, which recovers a panicking transform per
field and, on failure, discards whatever was already rendered rather than
handing back a truncated report.

## What you'll build

```text
fieldrender/                independent module: example.com/fieldrender
  go.mod                    go 1.24
  render.go                 Transform, Field, RenderError, Renderer, Render
  render_test.go             happy path, panic recovered, partial output discarded, unknown transform
```

Files: `render.go`, `render_test.go`.
Implement: `Renderer` with `Register(name string, fn Transform)` and `Render(fields []Field, data map[string]string) (string, error)`, plus `*RenderError` (`Error`+`Unwrap`) naming the failing field and transform.
Test: one table-driven test covering a mix of pass-through and transformed fields rendering correctly; a transform that panics (a bounds-unchecked slice) yielding a `*RenderError` naming the field; a panic partway through a multi-field render discarding the already-rendered output rather than returning it truncated; and an unknown transform name reporting a plain configuration error, not a `*RenderError`.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

Create `render.go`:

```go
package fieldrender

import "fmt"

// Transform maps a field's raw string value to its rendered form. Transforms
// are written by whoever owns the report, not by whoever owns the renderer —
// exactly the kind of third-party code a boundary must not trust blindly.
type Transform func(value string) (string, error)

// Field describes one column of a rendered report: which raw value to pull
// and which registered Transform, if any, to apply to it.
type Field struct {
	Name      string
	Transform string // "" means pass the raw value through untouched
}

// RenderError carries a panic recovered from a single field's transform,
// identified by field and transform name.
type RenderError struct {
	Field     string
	Transform string
	Value     any
}

func (e *RenderError) Error() string {
	return fmt.Sprintf("field %q transform %q panicked: %v", e.Field, e.Transform, e.Value)
}

func (e *RenderError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// Renderer applies registered transforms to build a report line.
type Renderer struct {
	transforms map[string]Transform
}

func NewRenderer() *Renderer {
	return &Renderer{transforms: make(map[string]Transform)}
}

func (r *Renderer) Register(name string, fn Transform) {
	r.transforms[name] = fn
}

// Render concatenates every field's rendered value, in order. If any field's
// transform panics or returns an error, Render stops and returns an empty
// string plus that error: earlier fields already rendered are discarded
// rather than handed back as a corrupt partial report.
func (r *Renderer) Render(fields []Field, data map[string]string) (string, error) {
	var out string
	for _, f := range fields {
		val, err := r.renderField(f, data)
		if err != nil {
			return "", err
		}
		out += val
	}
	return out, nil
}

// renderField is the recover boundary: one field's transform is untrusted
// code, and its panic must not take down the whole report.
func (r *Renderer) renderField(f Field, data map[string]string) (out string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			out = ""
			err = &RenderError{Field: f.Name, Transform: f.Transform, Value: rec}
		}
	}()

	raw := data[f.Name]
	if f.Transform == "" {
		return raw, nil
	}
	fn, ok := r.transforms[f.Transform]
	if !ok {
		return "", fmt.Errorf("field %q: unknown transform %q", f.Name, f.Transform)
	}
	return fn(raw)
}
```

Create `render_test.go`:

```go
package fieldrender

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func newTestRenderer() *Renderer {
	r := NewRenderer()
	r.Register("upper", func(v string) (string, error) {
		return strings.ToUpper(v), nil
	})
	r.Register("trunc5", func(v string) (string, error) {
		return v[:5], nil // no bounds check: panics on a short string
	})
	r.Register("pctOf100", func(v string) (string, error) {
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", err
		}
		return strconv.Itoa(100 / n), nil // panics on n == 0
	})
	return r
}

func TestRender(t *testing.T) {
	tests := []struct {
		name      string
		fields    []Field
		data      map[string]string
		wantOut   string
		wantPanic bool // a *RenderError, i.e. a recovered panic
		wantField string
		wantPlain bool // an error, but not a *RenderError
	}{
		{
			name:    "pass-through and transformed fields concatenate",
			fields:  []Field{{Name: "name", Transform: "upper"}, {Name: "id"}},
			data:    map[string]string{"name": "ana", "id": "42"},
			wantOut: "ANA42",
		},
		{
			name:      "panicking transform is recovered",
			fields:    []Field{{Name: "code", Transform: "trunc5"}},
			data:      map[string]string{"code": "ab"},
			wantPanic: true,
			wantField: "code",
		},
		{
			name: "a later panic discards earlier rendered output",
			fields: []Field{
				{Name: "name", Transform: "upper"}, // renders fine, but must not leak
				{Name: "ratio", Transform: "pctOf100"},
			},
			data:      map[string]string{"name": "ana", "ratio": "0"},
			wantPanic: true,
			wantField: "ratio",
		},
		{
			name:      "unknown transform is a plain configuration error",
			fields:    []Field{{Name: "x", Transform: "does-not-exist"}},
			data:      map[string]string{"x": "y"},
			wantPlain: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRenderer()
			out, err := r.Render(tt.fields, tt.data)

			if !tt.wantPanic && !tt.wantPlain {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				if out != tt.wantOut {
					t.Fatalf("out = %q, want %q", out, tt.wantOut)
				}
				return
			}

			if err == nil {
				t.Fatal("err = nil, want an error")
			}
			if out != "" {
				t.Fatalf("out = %q, want empty on failure", out)
			}
			var re *RenderError
			isRenderErr := errors.As(err, &re)
			if isRenderErr == tt.wantPlain {
				t.Fatalf("errors.As(*RenderError) = %v, want %v (err: %v)", isRenderErr, !tt.wantPlain, err)
			}
			if tt.wantField != "" && re.Field != tt.wantField {
				t.Fatalf("RenderError.Field = %q, want %q", re.Field, tt.wantField)
			}
		})
	}
}
```

## Review

The renderer is correct when a panicking transform never crashes the whole
report and never hands back a partially-built one: `Render` returns `""` plus
an error the instant any field fails, so a caller can never mistake three
good fields plus silence for "the report is fine." The recover lives in
`renderField`, one field wide — narrower than wrapping the whole loop, which
would only ever report the first failure and could leave `out` holding
whatever had been concatenated so far if the caller (wrongly) tried to salvage
it. `RenderError` naming both the field and the transform is what lets an
operator go straight to the offending registered function instead of
re-running the whole report to find out which of five transforms broke. Note
the unknown-transform case deliberately stays a plain error: that is a
configuration mistake made before any transform ran, not a panic this
boundary recovered.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the mechanism and the deferred-recover pattern.
- [errors: Is and As](https://pkg.go.dev/errors) — reaching an underlying error through a wrapping type like `RenderError`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-plugin-dispatch-boundary.md](10-plugin-dispatch-boundary.md) | Next: [12-must-style-config.md](12-must-style-config.md)
