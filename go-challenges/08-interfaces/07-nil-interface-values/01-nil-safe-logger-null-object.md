# Exercise 1: Nil-safe logger via the Null Object pattern

A `Logger` is the archetypal optional dependency: production wires a real one,
tests want to pass nil and get silence. This module builds a logger abstraction
whose `Service` constructor normalizes a nil `Logger` to a no-op `Null` at the
boundary, so every downstream call site dispatches without a nil guard.

## What you'll build

```text
nillog/                    independent module: example.com/nillog
  go.mod                   go 1.26
  logger.go                Logger interface; Null (no-op); Println; Service; NewService
  cmd/
    demo/
      main.go              nil vs real logger; typed-nil vs nil-interface print
  logger_test.go           table + property tests: no-panic, typed-nil pin
```

- Files: `logger.go`, `cmd/demo/main.go`, `logger_test.go`.
- Implement: a `Logger` interface (`Info`/`Error` with `...any` kv pairs), a `Null` no-op implementation, a `Println` implementation, and `NewService(l Logger)` that maps a nil interface to `Null{}`.
- Test: `NewService(nil)` must not panic on `DoWork`/`Fail`; `TestNilInterfaceValueIsNotTypedNil` pins `var l Logger == nil` but `var l Logger = (*Null)(nil) != nil`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/nillog/cmd/demo
cd ~/go-exercises/nillog
go mod init example.com/nillog
```

### Why the constructor is the only place that touches nil

The `Logger` interface has two methods. `Null` satisfies it with empty bodies —
that is the Null Object: a type whose behavior is "do nothing" but whose method
set fully satisfies the contract, so it can stand in anywhere a real `Logger` is
expected. `Println` is a real implementation that writes to stdout.

`Service` holds a `Logger` field and calls it on every operation. The whole
design rests on one line in `NewService`: `if l == nil { l = Null{} }`. Because
the nil is normalized exactly once, at construction, `DoWork` and `Fail` can
call `s.logger.Info(...)` and `s.logger.Error(...)` unconditionally. There is no
`if s.logger != nil` anywhere in the hot path, and there never needs to be.

Contrast the two nil forms the tests pin. `var l Logger` is a genuine nil
interface: both words nil, `l == nil` true, and calling `l.Info` would panic
because there is no dynamic type to dispatch to. `var l Logger = (*Null)(nil)` is
a typed nil: the dynamic type is `*Null`, the dynamic value is nil, so `l == nil`
is false and calling `l.Info` dispatches fine to `Null`'s method (which ignores
its receiver). `NewService` guards against the *first* form — the one that would
panic — by substituting a usable Null Object.

Create `logger.go`:

```go
package nillog

import "fmt"

// Logger is the optional-dependency contract. Info and Error take a message
// and alternating key/value pairs, matching the shape of structured loggers.
type Logger interface {
	Info(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// Null is the Null Object: it satisfies Logger with no-op methods and no side
// effects. Its methods use a value receiver, so a (*Null)(nil) also satisfies
// Logger and its methods never dereference the receiver.
type Null struct{}

func (Null) Info(msg string, kv ...any)  {}
func (Null) Error(msg string, kv ...any) {}

// Println is a real Logger that writes one line per call to stdout.
type Println struct {
	Prefix string
}

func (p Println) Info(msg string, kv ...any) {
	fmt.Println(p.Prefix, "INFO", msg)
}

func (p Println) Error(msg string, kv ...any) {
	fmt.Println(p.Prefix, "ERROR", msg)
}

// Service depends on a Logger. Its constructor is the single place that copes
// with a nil logger, so no method body ever branches on nil.
type Service struct {
	logger Logger
}

// NewService normalizes a nil Logger to Null{} at the boundary. Every downstream
// call site can then invoke logger methods unconditionally.
func NewService(l Logger) *Service {
	if l == nil {
		l = Null{}
	}
	return &Service{logger: l}
}

func (s *Service) DoWork(name string) {
	s.logger.Info("doing work", "name", name)
}

func (s *Service) Fail(name string, err error) {
	s.logger.Error("work failed", "name", name, "err", err)
}
```

### The runnable demo

The demo runs the two logger paths and then prints the interface-equality facts
directly, so a reader sees the two-word rule play out.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/nillog"
)

func main() {
	// A nil logger is normalized to Null{}: these calls produce no output.
	quiet := nillog.NewService(nil)
	quiet.DoWork("startup")

	// A real logger writes each line.
	svc := nillog.NewService(nillog.Println{Prefix: "[svc]"})
	svc.DoWork("alice")
	svc.Fail("alice", errors.New("boom"))

	// The two-word rule, made visible.
	var nilIface nillog.Logger
	fmt.Println("nil interface == nil:", nilIface == nil)

	var typedNil nillog.Logger = (*nillog.Null)(nil)
	fmt.Println("typed nil == nil:", typedNil == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[svc] INFO doing work
[svc] ERROR work failed
nil interface == nil: true
typed nil == nil: false
```

### Tests

`TestServiceWithNilLoggerDoesNotPanic` is the load-bearing test: it proves the
boundary normalization works by calling every `Service` method after passing
nil. `TestNilInterfaceValueIsNotTypedNil` pins the conceptual invariant. The
table test runs all three logger shapes through the service.

Create `logger_test.go`:

```go
package nillog

import (
	"errors"
	"testing"
)

func TestServiceWithNilLoggerDoesNotPanic(t *testing.T) {
	t.Parallel()

	s := NewService(nil)
	s.DoWork("test")
	s.Fail("test", errors.New("expected"))
}

func TestServiceAcrossLoggerShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		log  Logger
	}{
		{"nil normalizes to Null", nil},
		{"explicit Null", Null{}},
		{"real Println", Println{Prefix: "[test]"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := NewService(tc.log)
			s.DoWork("alice")
			s.Fail("alice", errors.New("boom"))
		})
	}
}

func TestNilInterfaceValueIsNotTypedNil(t *testing.T) {
	t.Parallel()

	var l Logger
	if l != nil {
		t.Fatal("var Logger should be a nil interface")
	}

	var n *Null
	var typed Logger = n
	if typed == nil {
		t.Fatal("var Logger = (*Null)(nil) should not be nil")
	}
}

func TestNullLoggerCanBeUsedDirectly(t *testing.T) {
	t.Parallel()

	var l Logger = Null{}
	l.Info("direct", "k", "v")
	l.Error("direct", "k", "v")
}
```

## Review

The design is correct when nil is handled in exactly one place. `NewService`
substitutes `Null{}` for a nil interface, so `Service.DoWork` and `Service.Fail`
never branch on nil and never panic — that is what
`TestServiceWithNilLoggerDoesNotPanic` proves. The conceptual invariant is that
`var Logger` is a nil interface (both words nil, equals nil) while
`var Logger = (*Null)(nil)` is a typed nil (dynamic type `*Null`, not equal to
nil); `TestNilInterfaceValueIsNotTypedNil` pins both. The mistake to avoid is
treating the nil interface as a valid "silent logger" and nil-checking at every
call site: the moment one guard is forgotten, the nil interface panics on
dispatch. The Null Object is the fix, applied once at the boundary.

## Resources

- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the two-word interface model that underlies this whole lesson.
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces) — interface satisfaction and method sets.
- [The Laws of Reflection](https://go.dev/blog/laws-of-reflection) — how an interface stores a (type, value) pair.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-typed-nil-error-return-bug.md](02-typed-nil-error-return-bug.md)
