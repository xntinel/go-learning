# Exercise 6: Per-Key Parse-Once Cache: sync.Map of OnceValues Closures

One `OnceValue` caches one value; an HTTP service rendering many templates
needs parse-exactly-once *per name*, under concurrent request load — this
exercise builds the stdlib-only idiom for that: a `sync.Map` whose values are
`sync.OnceValues` closures installed with `LoadOrStore`.

## What you'll build

```text
tmplregistry/              independent module: example.com/tmplregistry
  go.mod                   go mod init example.com/tmplregistry
  tmplregistry.go          type Registry; New(fs.FS), Get(name) (*template.Template, error)
  tmplregistry_test.go     per-key parse counter, 100-goroutine mixed load, cached parse error, Example
  cmd/
    demo/
      main.go              runnable demo over fstest.MapFS: render, pointer identity, cached error
```

- Files: `tmplregistry.go`, `tmplregistry_test.go`, `cmd/demo/main.go`.
- Implement: `Registry` over an `fs.FS` whose `Get(name)` parses each template exactly once via `sync.Map.LoadOrStore` of `sync.OnceValues` closures; an unexported, swappable `parse` field for test instrumentation.
- Test: 100 goroutines requesting a mix of 5 names — each name parsed exactly once, identical `*template.Template` pointer per name, and a syntax-broken template returning the same cached error to every caller, under `-race`.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

### The idiom: LoadOrStore a closure, let the loser discard

The naive per-key cache — `mu.Lock()`, check a map, parse on miss — holds one
mutex across every parse of every template, so a slow parse of `invoice.tmpl`
blocks an unrelated cache hit on `welcome.tmpl`. The idiom this module builds
removes that coupling with two stdlib pieces whose semantics interlock:

1. `sync.OnceValues` gives us a closure that parses once and caches the
   `(template, error)` pair.
2. `sync.Map.LoadOrStore(key, value)` stores `value` only if the key is
   absent, and returns the value that *won* — the existing one on a race.

Walk the cold-key race, because it is the whole trick. Two goroutines request
`welcome.tmpl` at the same instant. Both miss the initial `Load`. Both
construct a fresh `OnceValues` closure — cheap: no parsing has happened, a
closure is a pointer pair. Both call `LoadOrStore`. The map admits exactly
one; both goroutines receive the *winner's* closure and call it. The loser's
closure is discarded **unexecuted** — its init never ran, so no work is
wasted beyond the allocation — and the winner's internal once collapses the
two calls into one parse. Every later request hits the fast `Load` path,
which for `sync.Map` is a lock-free read. This is a miniature `singleflight`
for immutable results: dedupe concurrent work per key, then cache forever.

The same shape serves compiled regexps, prepared statements, and per-tenant
validators — anything keyed, expensive, and immutable once built. Two
consequences follow from "immutable, forever". First, errors are cached
per-key permanently, exactly like exercise 4: right for a template with a
syntax error (deterministic — redeploy to fix), and a reason not to use this
idiom for keys whose init can fail transiently. Second, the map only grows.
Fine when keys are a fixed set of template files; unbounded user-derived keys
need an evicting cache instead, and note that `sync.Map` itself is the right
map here precisely because the workload is its documented sweet spot —
write-once keys, read-many, disjoint key sets.

One instrumentation choice: the registry parses through an unexported `parse`
field defaulting to `template.ParseFS`. Counting `fs.FS` opens would be
fragile (a single `ParseFS` may `Stat` and `ReadFile`, opening more than
once), so the tests swap `parse` for a counting wrapper — same-package access
to unexported fields, no API surface added.

Create `tmplregistry.go`:

```go
// Package tmplregistry parses each named template from an fs.FS exactly
// once, on first use, under concurrent request load: a sync.Map of
// sync.OnceValues closures installed with LoadOrStore.
package tmplregistry

import (
	"io/fs"
	"sync"
	"text/template"
)

// Registry is a parse-once template cache keyed by file name. The zero
// value is not usable; construct with New.
type Registry struct {
	fsys  fs.FS
	parse func(fsys fs.FS, name string) (*template.Template, error)
	m     sync.Map // name -> func() (*template.Template, error)
}

// New returns a Registry reading templates from fsys.
func New(fsys fs.FS) *Registry {
	return &Registry{
		fsys: fsys,
		parse: func(fsys fs.FS, name string) (*template.Template, error) {
			return template.ParseFS(fsys, name)
		},
	}
}

// Get returns the parsed template for name, parsing it on the first call
// for that name. Concurrent first callers race to install a OnceValues
// closure with LoadOrStore; the loser's closure is discarded unexecuted, so
// exactly one parse runs per name. Both the template and any parse error
// are cached for the life of the Registry.
func (r *Registry) Get(name string) (*template.Template, error) {
	if f, ok := r.m.Load(name); ok {
		return f.(func() (*template.Template, error))()
	}
	once := sync.OnceValues(func() (*template.Template, error) {
		return r.parse(r.fsys, name)
	})
	f, _ := r.m.LoadOrStore(name, once)
	return f.(func() (*template.Template, error))()
}
```

Note the initial `Load` before `LoadOrStore` is purely an optimization: on
the hot path (key already present) it avoids allocating a throwaway closure.
Correctness comes from `LoadOrStore` alone.

### The demo

The demo runs the registry over an in-memory `fstest.MapFS` — the same
technique the tests use, and a standing reminder that `fs.FS` consumers are
trivially testable. It renders a greeting twice (same template pointer both
times), then requests a syntactically broken template twice and shows the
identical cached error object coming back.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"testing/fstest"

	"example.com/tmplregistry"
)

func main() {
	fsys := fstest.MapFS{
		"welcome.tmpl": &fstest.MapFile{Data: []byte("Hello, {{.Name}}!\n")},
		"broken.tmpl":  &fstest.MapFile{Data: []byte("{{.Name")},
	}
	reg := tmplregistry.New(fsys)

	t1, err := reg.Get("welcome.tmpl")
	if err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	if err := t1.Execute(os.Stdout, struct{ Name string }{Name: "Ada"}); err != nil {
		fmt.Println("execute:", err)
		return
	}

	t2, _ := reg.Get("welcome.tmpl")
	fmt.Println("same template object:", t1 == t2)

	_, err1 := reg.Get("broken.tmpl")
	_, err2 := reg.Get("broken.tmpl")
	fmt.Println("parse error:", err1)
	fmt.Println("same error object:", err1 == err2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Hello, Ada!
same template object: true
parse error: template: broken.tmpl:1: unclosed action
same error object: true
```

### Tests

`TestParsesEachNameOnce` is the load test the idiom exists for: 100
goroutines request a rotating mix of five names (four valid, one broken)
through a counting `parse`; afterwards each name must have been parsed
exactly once, every caller of a given name must hold the identical
`*template.Template`, and every caller of the broken name must hold the
identical error. `TestColdKeyRace` narrows to the two-goroutine race on one
cold key. The `Example` renders through the public API.

Create `tmplregistry_test.go`:

```go
package tmplregistry

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"text/template"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"welcome.tmpl": &fstest.MapFile{Data: []byte("Hello, {{.Name}}!")},
		"goodbye.tmpl": &fstest.MapFile{Data: []byte("Bye, {{.Name}}.")},
		"invoice.tmpl": &fstest.MapFile{Data: []byte("Total: {{.Name}}")},
		"header.tmpl":  &fstest.MapFile{Data: []byte("== {{.Name}} ==")},
		"broken.tmpl":  &fstest.MapFile{Data: []byte("{{.Name")},
	}
}

// parseCounter counts parse calls per template name.
type parseCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

// instrument swaps the registry's parse func for a counting wrapper.
func instrument(r *Registry) *parseCounter {
	pc := &parseCounter{counts: make(map[string]int)}
	inner := r.parse
	r.parse = func(fsys fs.FS, name string) (*template.Template, error) {
		pc.mu.Lock()
		pc.counts[name]++
		pc.mu.Unlock()
		return inner(fsys, name)
	}
	return pc
}

func (p *parseCounter) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[name]
}

func TestParsesEachNameOnce(t *testing.T) {
	t.Parallel()

	reg := New(testFS())
	pc := instrument(reg)
	names := []string{"welcome.tmpl", "goodbye.tmpl", "invoice.tmpl", "header.tmpl", "broken.tmpl"}

	type result struct {
		tmpl *template.Template
		err  error
	}
	const goroutines = 100
	results := make([]result, goroutines)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			tmpl, err := reg.Get(names[i%len(names)])
			results[i] = result{tmpl: tmpl, err: err}
		}()
	}
	wg.Wait()

	// Exactly one parse per name.
	for _, name := range names {
		if n := pc.count(name); n != 1 {
			t.Errorf("parse count for %s = %d, want 1", name, n)
		}
	}

	// Identical template pointer (or identical error) per name.
	first := make(map[string]result)
	for i, res := range results {
		name := names[i%len(names)]
		if name == "broken.tmpl" {
			if res.err == nil || !strings.Contains(res.err.Error(), "unclosed action") {
				t.Fatalf("broken.tmpl: err = %v, want unclosed action", res.err)
			}
		} else if res.err != nil {
			t.Fatalf("%s: unexpected err %v", name, res.err)
		}
		if f, ok := first[name]; ok {
			if res.tmpl != f.tmpl {
				t.Errorf("%s: different *template.Template across callers", name)
			}
			if res.err != f.err {
				t.Errorf("%s: different error object across callers", name)
			}
		} else {
			first[name] = res
		}
	}
}

func TestColdKeyRace(t *testing.T) {
	t.Parallel()

	reg := New(testFS())
	pc := instrument(reg)

	var wg sync.WaitGroup
	tmpls := make([]*template.Template, 2)
	wg.Add(2)
	for i := range 2 {
		go func() {
			defer wg.Done()
			tmpl, err := reg.Get("welcome.tmpl")
			if err != nil {
				t.Errorf("Get: %v", err)
			}
			tmpls[i] = tmpl
		}()
	}
	wg.Wait()

	if n := pc.count("welcome.tmpl"); n != 1 {
		t.Fatalf("cold-key race parsed %d times, want 1 (loser closure must be discarded unexecuted)", n)
	}
	if tmpls[0] != tmpls[1] {
		t.Fatal("racing callers received different templates")
	}
}

func ExampleRegistry_Get() {
	fsys := fstest.MapFS{
		"welcome.tmpl": &fstest.MapFile{Data: []byte("Hello, {{.Name}}!")},
	}
	reg := New(fsys)
	tmpl, err := reg.Get("welcome.tmpl")
	if err != nil {
		fmt.Println(err)
		return
	}
	_ = tmpl.Execute(os.Stdout, struct{ Name string }{Name: "Ada"})
	// Output: Hello, Ada!
}
```

Run the suite:

```bash
go test -count=1 -race ./...
```

## Review

The registry is correct when the counting tests hold under `-race`: one parse
per name across 100 mixed-goroutine requests, pointer-identical templates,
and object-identical cached errors — including on the deliberate two-goroutine
cold-key race, where the losing closure must die unexecuted. The subtle
mistakes in this idiom are all in the ordering: calling the closure *before*
`LoadOrStore` (both racers parse — the once you execute must be the one the
map admitted), storing the parsed template in the map instead of the closure
(you have to parse to have something to store, so racers duplicate work and
may publish different pointers), and using a plain map with a mutex held
across the parse (correct, but serializes unrelated keys). Also keep the
lifecycle rule in view: entries never expire and errors are permanent, which
fits deploy-time template files and does not fit user-derived or transiently
failing keys. If you need html-escaped output for an HTTP handler, swap
`text/template` for `html/template` — `ParseFS` has the same shape there.

## Resources

- [sync.Map](https://pkg.go.dev/sync#Map) — LoadOrStore semantics and the append-only/disjoint-keys sweet spot.
- [text/template ParseFS](https://pkg.go.dev/text/template#ParseFS) — the parse function behind the closure.
- [testing/fstest](https://pkg.go.dev/testing/fstest) — MapFS, the in-memory fs.FS the demo and tests run on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-retryable-lazy-init.md](05-retryable-lazy-init.md) | Next: [07-idempotent-close.md](07-idempotent-close.md)
