# Exercise 6: Breaking an Import Cycle with Dependency Inversion

An import cycle is a hard compile error, and it usually shows up right when two
subsystems start collaborating: the `store` wants to write audit records, and the
`audit` package wants to read from the store. If each imports the other, the build
fails with `import cycle not allowed`. The wrong fix is to merge them into one
package and lose the boundary. The right fix — the one senior engineers reach for
reflexively — is dependency inversion: the consumer declares a small interface it
owns, and the provider satisfies it without importing the consumer.

This module is self-contained. Nothing here imports another exercise.

## What you'll build

```text
inversion/                         module: example.com/inversion
  go.mod
  store/store.go                   package store: declares AuditSink interface (consumer-owned) + Store
  audit/audit.go                   package audit: Logger.Record satisfies the interface; imports NOT store
  store/store_test.go              injects a fake AuditSink; asserts Save records through the seam
  cmd/demo/main.go                 wires audit.Logger into store, saves, prints the audit trail
```

- Files: `store/store.go`, `audit/audit.go`, `store/store_test.go`, `cmd/demo/main.go`.
- Implement: `store` owns `AuditSink` and calls it from `Save`; `audit.Logger` has a matching `Record` method and imports nothing from `store`.
- Test: inject a fake sink into a `Store` and assert `Save` emits the expected audit event through the interface.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/inversion/store ~/go-exercises/inversion/audit ~/go-exercises/inversion/cmd/demo
cd ~/go-exercises/inversion
go mod init example.com/inversion
go mod edit -go=1.26
```

### The cycle you are avoiding

The naive design has `store` import `audit` (to call `audit.Log(...)` on every
write) and `audit` import `store` (to read the record it is logging about). That is
a cycle, and `go build` rejects it outright:

```text
package example.com/inversion/store
	imports example.com/inversion/audit
	imports example.com/inversion/store: import cycle not allowed
```

You cannot break it by adding a third "types" package if that package is itself
imported by both in a way that reintroduces the edge, and merging `store` and
`audit` into one package throws away the separation you wanted in the first place.

### The inversion: the consumer owns the interface

The fix flips who depends on whom. `store` is the *consumer* of auditing — it is
the one that needs "record this event" to happen. So `store` declares a small
interface, `AuditSink`, describing exactly the behavior it needs, and depends on
that interface. It does not import `audit` at all. `audit` provides a concrete
`Logger` whose method set structurally satisfies `AuditSink` — and because Go
interfaces are satisfied implicitly, `audit` does not need to import `store` to
"implement" it. The dependency now points one way: `audit` -> nothing,
`store` -> its own interface, and the wiring code (`main`) imports both to connect
them. The cycle is gone, and as a bonus `store` is now trivially testable with a
fake sink.

Keep the interface small — one method — which is idiomatic Go: declare the
narrowest interface the consumer actually uses, at the consumer.

Create `store/store.go`:

```go
package store

import "fmt"

// AuditSink is the audit behavior the store needs. It is declared here, in the
// consumer, so the store depends on an interface it owns rather than importing a
// concrete audit package (which would create an import cycle).
type AuditSink interface {
	Record(event string)
}

// Store persists key/value pairs and records each write to an AuditSink.
type Store struct {
	sink  AuditSink
	items map[string]string
}

// New returns a Store that records writes to sink.
func New(sink AuditSink) *Store {
	return &Store{sink: sink, items: make(map[string]string)}
}

// Save stores value under key and records an audit event through the sink.
func (s *Store) Save(key, value string) {
	s.items[key] = value
	s.sink.Record(fmt.Sprintf("save key=%s", key))
}

// Get returns the stored value and whether it was present.
func (s *Store) Get(key string) (string, bool) {
	v, ok := s.items[key]
	return v, ok
}
```

Create `audit/audit.go`. Note it imports nothing from `store`:

```go
package audit

// Logger is a concrete audit sink. Its Record method structurally satisfies
// store.AuditSink, so the store can use it without audit importing store.
type Logger struct {
	events []string
}

// Record appends an audit event.
func (l *Logger) Record(event string) {
	l.events = append(l.events, event)
}

// Events returns the recorded audit trail.
func (l *Logger) Events() []string {
	return l.events
}
```

### The demo wires the two together

`main` is the composition root: it is the one place that imports both concrete
packages and connects them. `audit.Logger` is passed where a `store.AuditSink` is
expected — the compiler checks the method set matches.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/inversion/audit"
	"example.com/inversion/store"
)

func main() {
	log := &audit.Logger{}
	s := store.New(log)

	s.Save("user:1", "ada")
	s.Save("user:2", "grace")

	for _, e := range log.Events() {
		fmt.Println(e)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
save key=user:1
save key=user:2
```

### Tests

The seam that broke the cycle also makes the store testable in isolation: inject a
fake `AuditSink` and assert `Save` calls it. The fake is defined in the test — no
need for the real `audit` package at all, which proves `store` truly depends only
on the interface.

Create `store/store_test.go`:

```go
package store

import (
	"reflect"
	"testing"
)

// fakeSink records the events it receives, standing in for any AuditSink.
type fakeSink struct {
	events []string
}

func (f *fakeSink) Record(event string) {
	f.events = append(f.events, event)
}

func TestSaveRecordsAuditEvent(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	s := New(sink)

	s.Save("k", "v")

	want := []string{"save key=k"}
	if !reflect.DeepEqual(sink.events, want) {
		t.Fatalf("audit events = %v, want %v", sink.events, want)
	}
}

func TestSaveStoresValue(t *testing.T) {
	t.Parallel()

	s := New(&fakeSink{})
	s.Save("k", "v")

	if got, ok := s.Get("k"); !ok || got != "v" {
		t.Fatalf("Get(k) = %q,%v, want v,true", got, ok)
	}
}
```

## Review

The design is correct when `store` compiles without importing `audit`, `audit`
compiles without importing `store`, and `Save` records exactly one event per write
through the interface. The whole lesson is *who owns the interface*: declaring
`AuditSink` in the consumer (`store`) is what inverts the dependency and dissolves
the cycle — declaring it in `audit`, or in a shared package that both import, often
just relocates the problem. The fake sink in the test is the proof that the seam is
real: if `store` still reached into `audit`, you could not substitute a local fake.
Do not "fix" a cycle by merging packages; that trades a compile error for a lost
boundary. Run `go vet` and `go build ./...` — the absence of the cycle is itself
the primary assertion.

## Resources

- [Go Spec: Package initialization and cycles](https://go.dev/ref/spec#Import_declarations) — import declarations and why cycles are rejected.
- [Go Code Review Comments: interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — declare interfaces in the consuming package.
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces) — implicit satisfaction, the mechanism that makes inversion work.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-observability-side-effect-imports.md](05-observability-side-effect-imports.md) | Next: [07-build-constraints-env-impl.md](07-build-constraints-env-impl.md)
