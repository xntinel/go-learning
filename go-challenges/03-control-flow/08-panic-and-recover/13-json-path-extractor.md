# Exercise 13: JSON Path Extractor: Internal Must-Helpers and Panic as a Long Jump With Context

**Nivel: Intermedio** — validacion rapida (un test corto).

Walking a dot-separated path through a `map[string]any` decoded from unknown
JSON means every segment can fail two different ways: the container isn't an
object, or the key isn't present. Threading a returned error through every
segment of that walk is a lot of `if err != nil` for what is really "stop
resolving, this path doesn't exist." This module builds `Get`, whose internal
`mustMap`/`mustKey` helpers panic with a private, path-carrying error the
moment a segment fails, and whose single exported boundary recovers once and
returns exactly that breadcrumb to the caller.

## What you'll build

```text
pathvalue/                  independent module: example.com/pathvalue
  go.mod                    go 1.24
  path.go                   pathError (private), mustMap, mustKey, Get
  path_test.go               valid path, missing key, non-object intermediate, root not object
```

Files: `path.go`, `path_test.go`.
Implement: private `mustMap(v any, breadcrumb string) map[string]any` and `mustKey(m map[string]any, key, breadcrumb string) any`, each panicking a private `*pathError` on failure; exported `Get(data any, path string) (any, error)` that recovers exactly that sentinel type and re-panics anything else.
Test: one table-driven test covering a valid multi-segment path resolving; a single-segment path resolving without walking anything; a missing key naming the full breadcrumb in the error; a non-object intermediate value naming the failing parent segment; and a non-object root being reported against a `"<root>"` label.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/pathvalue
cd ~/go-exercises/pathvalue
go mod init example.com/pathvalue
go mod edit -go=1.24
```

Create `path.go`:

```go
package pathvalue

import (
	"fmt"
	"strings"
)

// pathError is private: it never crosses this package's boundary as a panic
// value, only as the error Get returns. Get's recover classifies on this
// exact type; anything else is a real bug and is re-panicked, never
// misreported as "bad path."
type pathError struct {
	Path   string
	Reason string
}

func (e *pathError) Error() string {
	return fmt.Sprintf("pathvalue: %s: %s", e.Path, e.Reason)
}

// mustMap asserts v is a JSON object (decoded as map[string]any) and panics
// with the breadcrumb accumulated so far if it is not. Internal only: never
// exported, never called outside this file.
func mustMap(v any, breadcrumb string) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		panic(&pathError{Path: breadcrumb, Reason: fmt.Sprintf("not an object (got %T)", v)})
	}
	return m
}

// mustKey looks up key in m and panics with the breadcrumb if it is absent.
func mustKey(m map[string]any, key, breadcrumb string) any {
	v, ok := m[key]
	if !ok {
		panic(&pathError{Path: breadcrumb, Reason: fmt.Sprintf("missing key %q", key)})
	}
	return v
}

// Get walks a dot-separated path ("a.b.c") through nested
// map[string]any values, exactly the shape json.Unmarshal produces for an
// unknown schema. Writing this with a returned error at every segment means
// an "if err != nil" after every single lookup; instead the internal
// mustMap/mustKey helpers panic the moment a segment fails, carrying the
// exact breadcrumb where it went wrong, and Get recovers once at its single
// exported boundary. Panic here is a long jump with a payload, not a
// substitute for the package's own error contract: only this file ever
// panics, and only Get ever recovers.
func Get(data any, path string) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			pe, ok := r.(*pathError)
			if !ok {
				panic(r) // not our own sentinel: a real bug, keep it fatal
			}
			result, err = nil, pe
		}
	}()

	segments := strings.Split(path, ".")
	cur := data
	breadcrumb := "" // path resolved so far, i.e. the parent of the current segment
	for _, seg := range segments {
		parentLabel := breadcrumb
		if parentLabel == "" {
			parentLabel = "<root>"
		}
		m := mustMap(cur, parentLabel)

		if breadcrumb == "" {
			breadcrumb = seg
		} else {
			breadcrumb = breadcrumb + "." + seg
		}
		cur = mustKey(m, seg, breadcrumb)
	}
	return cur, nil
}
```

Create `path_test.go`:

```go
package pathvalue

import (
	"errors"
	"strings"
	"testing"
)

func sampleDoc() map[string]any {
	return map[string]any{
		"user": map[string]any{
			"name": "ana",
			"address": map[string]any{
				"city": "lima",
			},
		},
	}
}

func TestGet(t *testing.T) {
	tests := []struct {
		name     string
		data     any
		path     string
		wantVal  any
		validate func(t *testing.T, err error) // nil means "expect wantVal, no error"
	}{
		{
			name:    "valid multi-segment path resolves",
			data:    sampleDoc(),
			path:    "user.address.city",
			wantVal: "lima",
		},
		{
			name:    "single-segment path resolves without walking",
			data:    map[string]any{"key": 42},
			path:    "key",
			wantVal: 42,
		},
		{
			name: "missing key names the full breadcrumb",
			data: sampleDoc(),
			path: "user.phone",
			validate: func(t *testing.T, err error) {
				if !strings.Contains(err.Error(), `"phone"`) {
					t.Fatalf("err = %v, want it to name the missing key", err)
				}
				if !strings.Contains(err.Error(), "user.phone") {
					t.Fatalf("err = %v, want the full breadcrumb user.phone", err)
				}
			},
		},
		{
			name: "non-object intermediate names the failing parent",
			data: sampleDoc(),
			path: "user.name.first", // user.name is a string, not an object
			validate: func(t *testing.T, err error) {
				if !strings.Contains(err.Error(), "user.name") {
					t.Fatalf("err = %v, want it to identify user.name as the failing parent", err)
				}
				if !strings.Contains(err.Error(), "not an object") {
					t.Fatalf("err = %v, want a not-an-object reason", err)
				}
			},
		},
		{
			name: "non-object root is reported against <root>",
			data: "just a string",
			path: "a",
			validate: func(t *testing.T, err error) {
				var pe *pathError
				if !errors.As(err, &pe) {
					t.Fatalf("err is %T, want *pathError", err)
				}
				if pe.Path != "<root>" {
					t.Fatalf("pe.Path = %q, want %q", pe.Path, "<root>")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Get(tt.data, tt.path)

			if tt.validate == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				if got != tt.wantVal {
					t.Fatalf("got = %v, want %v", got, tt.wantVal)
				}
				return
			}

			if err == nil {
				t.Fatal("err = nil, want a path error")
			}
			tt.validate(t, err)
		})
	}
}
```

## Review

`Get` is correct when every failure names precisely where the walk broke —
the missing key's full breadcrumb, or the parent segment that turned out not
to be an object — and when the panic never becomes visible to anything
outside this file. `mustMap` and `mustKey` are private for exactly that
reason: no other package can panic with `*pathError`, and no caller can come
to depend on the panic itself escaping, because `Get` is the only place that
ever calls `recover`. The type assertion in the recover, `r.(*pathError)`,
is the load-bearing safety check: it succeeds only for this package's own
control-flow panic, and the `ok` guard re-panics anything else, so a genuine
bug in `mustMap`/`mustKey` is never misreported to a caller as "your path
doesn't exist." Compare the breadcrumb tracking here to hand-threading an
error through every loop iteration — the panic carries the accumulated
context for free, precisely because it unwinds through frames that would
otherwise each need their own `if err != nil`.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — recover's goroutine- and defer-scoped behavior.
- [encoding/json: Unmarshal into any](https://pkg.go.dev/encoding/json#Unmarshal) — why unknown JSON decodes into exactly this `map[string]any` shape.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-must-style-config.md](12-must-style-config.md) | Next: [14-job-runner-report.md](14-job-runner-report.md)
