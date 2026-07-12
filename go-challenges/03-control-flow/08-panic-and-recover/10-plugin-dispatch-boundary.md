# Exercise 10: Plugin Dispatcher: Isolating a Third-Party Handler's Panic at One Call Boundary

**Nivel: Intermedio** — validacion rapida (un test corto).

A plugin system routes a call by name to a handler someone else wrote — a
webhook processor, a report exporter, a rules engine extension. That handler
is not your code and you cannot audit every path through it before every
release. This module builds `Dispatcher.Invoke`, which registers named
handlers and recovers any panic a handler raises at the single point where
your code hands control to theirs, turning it into a typed error instead of a
process crash.

## What you'll build

```text
plugindispatch/            independent module: example.com/plugindispatch
  go.mod                   go 1.24
  dispatch.go              Handler, CallError (Error+Unwrap), Dispatcher, Invoke
  dispatch_test.go         happy path, error-panic, string-panic, unregistered, reuse
```

Files: `dispatch.go`, `dispatch_test.go`.
Implement: `Dispatcher` with `Register(name string, h Handler)` and `Invoke(name, input string) (string, error)`, plus `*CallError` (`Error`+`Unwrap`) carrying the plugin name and the recovered value.
Test: a healthy plugin returns normally; a plugin panicking with an error yields a `*CallError` whose `Unwrap` reaches the original via `errors.Is`; a plugin panicking with a bare string still yields a `*CallError`; an unregistered name is a plain error, never a `*CallError`, because that call never reached plugin code; a dispatcher survives one panicking call and still serves the next.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

Create `dispatch.go`:

```go
package plugindispatch

import "fmt"

// Handler is a plugin function registered under a name. Plugin authors are a
// separate team; their code is trusted to do its job, not trusted to never panic.
type Handler func(input string) (string, error)

// CallError carries a panic recovered from a plugin call, identified by the
// plugin name that produced it. It implements Unwrap so errors.Is/As reach an
// underlying error value.
type CallError struct {
	Plugin string
	Value  any
}

func (e *CallError) Error() string {
	return fmt.Sprintf("plugin %q panicked: %v", e.Plugin, e.Value)
}

func (e *CallError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// Dispatcher routes named calls to registered plugin handlers.
type Dispatcher struct {
	handlers map[string]Handler
}

func New() *Dispatcher {
	return &Dispatcher{handlers: make(map[string]Handler)}
}

func (d *Dispatcher) Register(name string, h Handler) {
	d.handlers[name] = h
}

// Invoke calls the named plugin. A panic anywhere inside the plugin is
// recovered here, at the single boundary between the dispatcher and
// third-party code, and turned into a *CallError. An unregistered name is a
// plain error, not a CallError: that failure never entered plugin code.
func (d *Dispatcher) Invoke(name, input string) (output string, err error) {
	h, ok := d.handlers[name]
	if !ok {
		return "", fmt.Errorf("plugin %q not registered", name)
	}
	defer func() {
		if r := recover(); r != nil {
			output = ""
			err = &CallError{Plugin: name, Value: r}
		}
	}()
	return h(input)
}
```

Create `dispatch_test.go`:

```go
package plugindispatch

import (
	"errors"
	"testing"
)

func TestInvoke(t *testing.T) {
	sentinel := errors.New("bad plugin state")

	d := New()
	d.Register("ok", func(input string) (string, error) {
		return "echo:" + input, nil
	})
	d.Register("boom-error", func(input string) (string, error) {
		panic(sentinel)
	})
	d.Register("boom-string", func(input string) (string, error) {
		panic("plugin exploded")
	})

	tests := []struct {
		name       string
		plugin     string
		wantOutput string
		wantErr    bool
		wantCall   bool
		wantSent   bool
	}{
		{name: "healthy plugin returns normally", plugin: "ok", wantOutput: "echo:x"},
		{name: "plugin panics with an error", plugin: "boom-error", wantErr: true, wantCall: true, wantSent: true},
		{name: "plugin panics with a string", plugin: "boom-string", wantErr: true, wantCall: true},
		{name: "unregistered plugin is a plain error", plugin: "missing", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := d.Invoke(tt.plugin, "x")

			if tt.wantErr && err == nil {
				t.Fatalf("Invoke(%q) err = nil, want error", tt.plugin)
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Invoke(%q) err = %v, want nil", tt.plugin, err)
				}
				if out != tt.wantOutput {
					t.Fatalf("output = %q, want %q", out, tt.wantOutput)
				}
				return
			}

			var ce *CallError
			isCallErr := errors.As(err, &ce)
			if isCallErr != tt.wantCall {
				t.Fatalf("errors.As(*CallError) = %v, want %v (err: %v)", isCallErr, tt.wantCall, err)
			}
			if tt.wantSent && !errors.Is(err, sentinel) {
				t.Fatalf("errors.Is(err, sentinel) = false, want true (err: %v)", err)
			}
			if out != "" {
				t.Fatalf("output on failure = %q, want empty", out)
			}
		})
	}

	// The table above already exercises this dispatcher through two panicking
	// calls (boom-error, boom-string) followed by more calls: recover is
	// re-armed on every Invoke, not consumed once and left disabled.
	if out, err := d.Invoke("ok", "y"); err != nil || out != "echo:y" {
		t.Fatalf("dispatcher did not survive earlier panics: out=%q err=%v", out, err)
	}
}
```

## Review

`Invoke` is correct when every plugin outcome — success, panic with an error,
panic with a bare value, and a lookup miss — surfaces as a distinct, typed
result instead of three of those four crashing the process. The `defer`
belongs directly inside `Invoke`, wrapping only the call to `h(input)`; it is
re-armed on every invocation, which is why a panicking plugin does not disable
the dispatcher for the next call. Notice the boundary is deliberately narrow:
an unregistered name never reaches the `defer` at all, because that failure
is the dispatcher's own contract violation, not a plugin's — conflating the
two would hide which side of the boundary actually broke. `CallError.Unwrap`
is what makes a plugin's own sentinel errors still reachable with
`errors.Is`/`errors.As` after crossing the panic/recover boundary; without it,
a panic with a well-typed error degrades into an opaque `any`.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the mechanism this boundary relies on.
- [errors: Is and As](https://pkg.go.dev/errors) — why `Unwrap` matters once a panic value becomes an error.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-panic-during-defer-cleanup.md](09-panic-during-defer-cleanup.md) | Next: [11-template-render-guard.md](11-template-render-guard.md)
