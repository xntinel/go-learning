# Exercise 35: Environment Configuration Merger with Precedence Levels

**Nivel: Intermedio** — validacion rapida (un test corto).

A service typically loads configuration from several places at once — a
checked-in defaults file, the process environment, a secrets manager —
and the whole point of layering them is that each later source is allowed
to override the ones before it. `Merge(sources ...Source)` takes that
layered stack as a variadic list, in lowest-to-highest precedence order,
and folds it into one final map where the *last* source to mention a key
wins.

## What you'll build

```text
envmerge/                  independent module: example.com/envmerge
  go.mod                   go 1.24
  envmerge.go              package envmerge; type Source struct{Name string; Values map[string]string}; Merge(sources ...Source) map[string]string; MergeWithProvenance(sources ...Source) (map[string]string, map[string]string)
  cmd/
    demo/
      main.go              runnable demo: file, env, and secrets layers merged with provenance shown per key
  envmerge_test.go          table tests: later source overrides, precedence is purely positional, empty input, provenance tracking
```

- Files: `envmerge.go`, `cmd/demo/main.go`, `envmerge_test.go`.
- Implement: `type Source struct{ Name string; Values map[string]string }`, `Merge(sources ...Source) map[string]string`, and `MergeWithProvenance(sources ...Source) (values map[string]string, from map[string]string)`.
- Test: `Merge(file, env)` takes `env`'s value for a key both define; reversing the argument order flips which one wins, proving precedence is positional, not tied to a source's `Name`; zero sources merges to an empty map; `MergeWithProvenance` reports which source's `Name` contributed each key's final value.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why precedence lives in argument order, not in the source's identity

`Merge` deliberately has no special-cased knowledge of "environment
variables beat files" or "secrets beat everything" — it just iterates
`sources` in the order given and lets each later source's values
overwrite the map entry for the same key. The precedence policy — which
kind of source should win — lives entirely at the call site:
`Merge(fileSource, envSource, secretsSource)` encodes "secrets are most
authoritative" purely by putting `secretsSource` last. This is the same
principle the header merger and telemetry tag encoder earlier in this
chapter use (last write wins, by position), applied here to whole
key/value maps instead of individual pairs. The advantage of keeping the
policy at the call site rather than inside `Merge` is that a caller
serving a different environment — say, a local dev setup where the `.env`
file should be allowed to override the shell's real environment variables
for convenience — just changes the argument order, with no change to
`Merge` itself. `TestMergePrecedenceIsPositionalNotSemantic` pins this
directly: the same two sources, given in reversed order, produce the
opposite winner.

`MergeWithProvenance` exists because "which source actually won for this
key" is a question every real deployment eventually needs answered during
an incident — a service behaving unexpectedly because a stale secret
silently overrode a freshly deployed config file is a common enough class
of bug that logging the provenance of every merged setting at startup is
worth the extra return value. It reuses exactly the same merge loop as
`Merge`, just also recording `src.Name` alongside each value as it writes
it, so the two functions can never disagree about which value wins.

Create `envmerge.go`:

```go
// envmerge.go
package envmerge

// Source is one origin of configuration values, such as the process
// environment, a .env file, or a secrets manager.
type Source struct {
	Name   string
	Values map[string]string
}

// Merge combines any number of configuration Sources into one map. A
// source's position in the variadic list is its precedence: later
// sources override earlier ones key by key, so
// Merge(fileSource, envSource, secretsSource) means "secrets override
// env vars, which override the file," entirely because of argument
// order — Merge itself has no notion of which kind of source is more
// important.
func Merge(sources ...Source) map[string]string {
	merged := make(map[string]string)
	for _, src := range sources {
		for k, v := range src.Values {
			merged[k] = v
		}
	}
	return merged
}

// MergeWithProvenance behaves like Merge but additionally reports, for
// every key in the result, the Name of the source that contributed its
// final value — useful for a config-loaded log line or a debug endpoint
// that explains where each setting actually came from.
func MergeWithProvenance(sources ...Source) (values map[string]string, from map[string]string) {
	values = make(map[string]string)
	from = make(map[string]string)
	for _, src := range sources {
		for k, v := range src.Values {
			values[k] = v
			from[k] = src.Name
		}
	}
	return values, from
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"sort"

	"example.com/envmerge"
)

func main() {
	fileSource := envmerge.Source{Name: "file", Values: map[string]string{
		"LOG_LEVEL": "info",
		"PORT":      "8080",
	}}
	envSource := envmerge.Source{Name: "env", Values: map[string]string{
		"PORT": "9090",
	}}
	secretsSource := envmerge.Source{Name: "secrets", Values: map[string]string{
		"DB_PASSWORD": "s3cr3t",
	}}

	values, from := envmerge.MergeWithProvenance(fileSource, envSource, secretsSource)

	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		fmt.Printf("%s=%s (from %s)\n", k, values[k], from[k])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
DB_PASSWORD=s3cr3t (from secrets)
LOG_LEVEL=info (from file)
PORT=9090 (from env)
```

(The demo sorts keys before printing since map iteration order is
randomized in Go — the merge result itself does not depend on that
order, only the printed listing does.)

### Tests

`TestMergePrecedenceIsPositionalNotSemantic` is the one that pins the
core design decision: it merges the exact same two sources in the
opposite order and asserts the winner flips, proving precedence comes
from argument position and nothing else.

Create `envmerge_test.go`:

```go
// envmerge_test.go
package envmerge

import "testing"

func TestMergeLaterSourceOverrides(t *testing.T) {
	t.Parallel()

	file := Source{Name: "file", Values: map[string]string{"LOG_LEVEL": "info", "PORT": "8080"}}
	env := Source{Name: "env", Values: map[string]string{"PORT": "9090"}}

	merged := Merge(file, env)
	if merged["PORT"] != "9090" {
		t.Errorf("PORT = %q, want %q (later source wins)", merged["PORT"], "9090")
	}
	if merged["LOG_LEVEL"] != "info" {
		t.Errorf("LOG_LEVEL = %q, want %q (untouched key preserved)", merged["LOG_LEVEL"], "info")
	}
}

func TestMergePrecedenceIsPositionalNotSemantic(t *testing.T) {
	t.Parallel()

	// Same two sources, reversed order: env now comes first, so file
	// wins for PORT. Merge has no built-in notion of which source "should"
	// win; only argument order decides.
	env := Source{Name: "env", Values: map[string]string{"PORT": "9090"}}
	file := Source{Name: "file", Values: map[string]string{"PORT": "8080"}}

	merged := Merge(env, file)
	if merged["PORT"] != "8080" {
		t.Errorf("PORT = %q, want %q (last argument wins)", merged["PORT"], "8080")
	}
}

func TestMergeNoSourcesIsEmpty(t *testing.T) {
	t.Parallel()

	merged := Merge()
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want empty", merged)
	}
}

func TestMergeWithProvenanceTracksWinningSource(t *testing.T) {
	t.Parallel()

	file := Source{Name: "file", Values: map[string]string{"PORT": "8080"}}
	env := Source{Name: "env", Values: map[string]string{"PORT": "9090"}}
	secrets := Source{Name: "secrets", Values: map[string]string{"DB_PASSWORD": "s3cr3t"}}

	values, from := MergeWithProvenance(file, env, secrets)

	if values["PORT"] != "9090" || from["PORT"] != "env" {
		t.Errorf("PORT = %q from %q, want 9090 from env", values["PORT"], from["PORT"])
	}
	if values["DB_PASSWORD"] != "s3cr3t" || from["DB_PASSWORD"] != "secrets" {
		t.Errorf("DB_PASSWORD = %q from %q, want s3cr3t from secrets", values["DB_PASSWORD"], from["DB_PASSWORD"])
	}
}
```

## Review

`Merge` is correct when, for every key, the value in the result is
whichever source mentioning that key came last in the argument list — no
more, no less — and zero sources merge to an empty, non-nil map. The
senior point is keeping the precedence *policy* (which kind of source
should be authoritative) entirely out of the merge function and encoded
instead in how the caller orders its arguments, which is what lets the
same `Merge` serve a production layering (file, env, secrets) and a local
development layering (env, file — deliberately reversed for convenience)
without any change to the function itself. `MergeWithProvenance` is the
one addition worth having in a real system: the moment a merged
configuration behaves unexpectedly, "which source actually set this
value" is the first question anyone asks, and it is far cheaper to have
recorded that at merge time than to reconstruct it after the fact from
separate logs of each source.

## Resources

- [The Twelve-Factor App: Config](https://12factor.net/config)
- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)
- [`os.LookupEnv`](https://pkg.go.dev/os#LookupEnv)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [34-json-schema-validator-rules-aggregate.md](34-json-schema-validator-rules-aggregate.md) | Next: [../04-first-class-functions-and-closures/00-concepts.md](../04-first-class-functions-and-closures/00-concepts.md)
