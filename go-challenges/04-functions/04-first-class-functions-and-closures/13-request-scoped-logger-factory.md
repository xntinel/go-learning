# Exercise 13: Per-Request Scoped Logger Built from Derived Closures

**Nivel: Intermedio** — validacion rapida (un test corto).

A request handler wants every log line to carry a request ID, then a
middleware wants to add a user ID, then a handler wants to add an org ID —
each layer adding fields without ever mutating what an earlier layer holds.
`New` and `With` build a `Logger` out of closures that copy their captured
fields forward instead of sharing them, the opposite of the mutate-in-place
pattern the rest of this lesson uses.

## What you'll build

```text
requestlog/                 independent module: example.com/request-scoped-logger
  go.mod                    go 1.24
  logger.go                 Logger{Log, With}; New copies fields on entry
  logger_test.go            table test: siblings from With do not leak
```

- Files: `logger.go`, `logger_test.go`.
- Implement: `type Logger struct { Log func(string) string; With func(key, value string) Logger }`, `New(fields map[string]string) Logger`, where `With` derives a fresh `Logger` over a copied-and-extended field map.
- Test: a table logs through a base `Logger` and two `Logger`s derived from it with different extra fields, and confirms the base is unaffected by either derivation.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/04-first-class-functions-and-closures/13-request-scoped-logger-factory
cd go-solutions/04-functions/04-first-class-functions-and-closures/13-request-scoped-logger-factory
go mod edit -go=1.24
```

### Deriving instead of mutating

Every other closure in this lesson keeps one mutable variable and updates it
in place — a token count, a failure streak, a cache map. `Logger` does the
opposite on purpose: `With` never touches the fields the current `Logger`
closes over. It copies them into a new map, adds one key, and calls `newLogger`
again to build a fresh pair of closures over that copy. The result is that
`base`, `base.With("user", "u1")`, and `base.With("org", "o1")` are three
independent `Logger` values, none of which can see the others' extra field,
even though all three ultimately trace back to the same starting fields. This
is the direct fix for the "returning the internal map" mistake from the
concepts file: `New` itself also copies its input, so a caller who mutates
the map they passed in afterward cannot reach into the `Logger`'s state
either.

`formatLine` sorts the field keys before rendering so the output order is
deterministic across runs — map iteration order is not guaranteed, and a log
line whose field order shuffled between builds would not be usefully greppable.

Create `logger.go`:

```go
package requestlog

import (
	"fmt"
	"sort"
	"strings"
)

// Logger is a self-contained log-line formatter: Log renders msg prefixed
// with the fields captured when the Logger was built, and With derives a
// sibling Logger carrying one extra field. Nothing about the fields is
// exposed except through these two closures.
type Logger struct {
	Log  func(msg string) string
	With func(key, value string) Logger
}

// New returns a Logger seeded with fields. The map is copied so a caller who
// mutates it afterward cannot reach into the Logger's captured state.
func New(fields map[string]string) Logger {
	return newLogger(copyFields(fields))
}

func newLogger(fields map[string]string) Logger {
	var l Logger

	l.Log = func(msg string) string {
		return formatLine(fields, msg)
	}

	l.With = func(key, value string) Logger {
		next := copyFields(fields)
		next[key] = value
		return newLogger(next)
	}

	return l
}

func copyFields(fields map[string]string) map[string]string {
	next := make(map[string]string, len(fields))
	for k, v := range fields {
		next[k] = v
	}
	return next
}

func formatLine(fields map[string]string, msg string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s ", k, fields[k])
	}
	b.WriteString(msg)
	return b.String()
}
```

### Tests

Create `logger_test.go`:

```go
package requestlog

import "testing"

func TestLoggerAccumulatesFieldsWithoutMutatingParent(t *testing.T) {
	base := New(map[string]string{"req": "r1"})
	withUser := base.With("user", "u1")
	withOrg := base.With("org", "o1")

	tests := []struct {
		name string
		l    Logger
		msg  string
		want string
	}{
		{"base only", base, "hello", "req=r1 hello"},
		{"base plus user", withUser, "hi", "req=r1 user=u1 hi"},
		{"base plus org", withOrg, "hi", "org=o1 req=r1 hi"},
		{"base unaffected by siblings", base, "again", "req=r1 again"},
	}

	for _, tc := range tests {
		if got := tc.l.Log(tc.msg); got != tc.want {
			t.Fatalf("%s: Log() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestNewCopiesInputMap(t *testing.T) {
	fields := map[string]string{"req": "r1"}
	l := New(fields)
	fields["req"] = "corrupted"

	if got, want := l.Log("hi"), "req=r1 hi"; got != want {
		t.Fatalf("Log() = %q, want %q (mutating caller's map must not affect Logger)", got, want)
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`base`, `withUser`, and `withOrg` all trace back to the same starting field,
but each holds its own private copy: `With` never writes through to the
`Logger` it was called on, and `New` never keeps a live reference to the
caller's map either. That is the whole point of the table test — logging
through three related `Logger`s in any order produces exactly the fields each
one was built with, never a field leaked in from a sibling.

## Resources

- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — how `Log` and `With` share one captured `fields` map.
- [pkg.go.dev: maps package overview](https://pkg.go.dev/maps) — idioms for copying a map defensively (the standard library's `maps.Clone` does what `copyFields` does by hand here).

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-keyset-pagination-cursor-codec.md](12-keyset-pagination-cursor-codec.md) | Next: [14-key-scoped-debounce-detector.md](14-key-scoped-debounce-detector.md)
