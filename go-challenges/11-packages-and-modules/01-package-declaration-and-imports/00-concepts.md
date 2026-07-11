# Package Declaration and Imports: Wiring a Go Service Together — Concepts

In a Go backend the import graph *is* the architecture. The same edges that the
compiler follows to build the binary are simultaneously the dependency graph, the
compile-time boundary between subsystems, the seam along which you test, and a
first-order driver of build times. The `package` clause and the import rules are
not syntax trivia to skim past on the way to goroutines; they decide whether a
service is testable, whether a change is local or ripples across the tree, and
whether the thing even compiles. Three import mechanics dominate a senior
engineer's day: blank-import registration (how drivers and profilers wire
themselves into global registries), import cycles (a hard compile error whose
canonical fix is dependency inversion), and build constraints plus `go:embed`
(how you gate environment-specific code and bundle assets into the binary). This
file is the conceptual foundation for the nine independent exercises that follow;
read it once and you have the model you need for all of them.

## Concepts

### One package per directory

Every `.go` file in a directory shares one `package` clause. The compiler enforces
this: two different package names in one directory is an error
(`found packages grep (grep.go) and main (main.go)`). The single, deliberate
exception is the *external test package* `foo_test`, which may live in the same
directory as `package foo` so that a black-box test imports the package the way a
real consumer would — and, crucially, so a test may import a third package that
itself imports `foo` without forming a cycle. That external-test escape hatch is
exactly what lets the codec-registry exercise blank-import its provider packages
from `registry`'s own test directory.

### package main builds a command; every other name builds a library

`package main` with a `func main()` compiles to an executable. Any other package
name compiles to a library that cannot be run directly — it is imported. The
package *name* is independent of the directory name and of the import path.
`package grep` can live in a directory called `internal/matcher` imported as
`example.com/svc/internal/matcher`. Nothing forces the three to agree; matching
them is a courtesy to readers and tooling, not a rule. Confusing the three (name
vs directory vs import path) compiles fine and confuses everyone downstream.

### Import paths are module-relative identifiers, not filesystem paths

An import path is resolved through the module graph, not the filesystem. In module
mode, relative imports like `import "./internal/grep"` are simply not supported —
`go build` rejects them. You always import via the full module path
(`example.com/pkgsimports/internal/grep`), which the `go` tool maps to a directory
through the current module's `go.mod` and the module cache. This is why a package
can be moved on disk (within its module) as long as its import path is preserved,
and why the `internal/` and `vendor/` directory names carry special
import-resolution semantics rather than being ordinary folders.

### Import grouping: gofmt leaves it alone, goimports regroups

The convention is three blocks separated by blank lines — standard library, then
third-party, then this module's own internal packages:

```go
import (
	"fmt"
	"os"

	"github.com/some/dep"

	"example.com/svc/internal/grep"
)
```

The trap that bites teams: `gofmt` sorts *within* each existing group but never
reorders or merges groups. `goimports` does regroup (and removes unused / adds
missing imports). A CI that runs `gofmt` on one machine and `goimports` on another
produces perpetual, meaningless diff churn. Pick one formatter for the whole repo
and enforce it in one place.

### Blank imports run init() for side effects — hidden global state

`import _ "path"` imports a package solely to run its `init()` functions and
package-level variable initializers, discarding its exported names. This is the
registration mechanism behind the entire Go ecosystem: `database/sql` drivers
(`_ "github.com/lib/pq"`) call `sql.Register` in `init`; image decoders
(`_ "image/png"`) register format handlers; `_ "net/http/pprof"` mounts profiling
routes onto `http.DefaultServeMux`; `expvar` publishes `/debug/vars`. It is
powerful precisely because it is invisible at the call site — and that is also the
danger. A blank import is *load-bearing*: delete it and the driver silently
disappears at runtime with a "codec not found" error, not a compile error. It
mutates global state at program start, which surprises people in tests (import
order changes what is registered) and, when `net/http/pprof` lands on a public
mux, exposes heap and CPU profiles to the internet.

### Import aliasing resolves name collisions; dot imports are an anti-pattern

Two packages with the same base name cannot both be referred to by that name in
one file. `api/v1/user` and `api/v2/user` are both `package user`; to use both you
alias one or both: `import userv1 "…/v1/user"` and `import userv2 "…/v2/user"`.
This is the everyday reality of proto/DTO versioning and of two libraries that
both named their package `client`. Aliasing changes only the *local* identifier;
it does not change the imported package's own declared name. The dot import
`import . "path"` dumps a package's exported names into the local file scope,
making call sites ambiguous and un-greppable; reserve it for test-only DSLs and
never use it in application code.

### init() ordering: dependency edges, then lexical filename order

Within a package, all package-level variables are initialized first (in
*dependency* order — a `var` that references another is initialized after it, not
in source-line order), then `init()` functions run, in the lexical order of the
filenames and top-to-bottom within a file. Across packages, an imported package is
fully initialized before the importer. You *cannot* control cross-package init
order beyond the dependency edges you actually create, and encoding startup
ordering into filenames (`aaa_first.go`) is fragile. `init` cannot be called or
referenced by name.

### Import cycles are a hard error; break them with dependency inversion

If `store` imports `audit` and `audit` imports `store`, the build fails with
`import cycle not allowed`. The wrong fix is to merge the two into one mud-ball
package. The idiomatic fix is dependency inversion: declare a small interface in
the *consumer* package (`store` declares `type AuditSink interface { Record(...) }`)
and let the provider satisfy it structurally — `audit.Logger` grows a `Record`
method without importing `store`. The consumer now depends on an interface it
owns; the cycle is gone and the seam makes the consumer testable with a fake.

### Build constraints select which files compile

Two forms gate a file. Filename suffixes are automatic: `foo_linux.go` compiles
only on Linux, `foo_amd64.go` only on amd64, `foo_test.go` only under `go test`.
Explicit constraints use a `//go:build` line as the first line of the file,
followed by a blank line before `package`: `//go:build dev` includes the file only
when built with `-tags=dev`; `//go:build !dev` is its complement. (`//go:build`
is the modern form; the old `// +build` comments are deprecated.) This is how you
ship a safe production stub by default and a developer implementation behind
`-tags=dev`, gate platform-specific syscalls, and keep slow integration tests out
of the default `go test` run behind `//go:build integration`. `go vet` validates
the constraint expressions.

### go:embed bundles files into the binary

`//go:embed migrations/*.sql` folds files from disk into an `embed.FS` (or a
`string`/`[]byte`) at compile time, so the assets travel *inside* the binary. This
deletes a whole class of "file not found in production" deploy bugs for
migrations, HTML templates, and OpenAPI specs — there is no runtime directory to
mount or forget. Using the `//go:embed` directive requires importing `embed`; when
you embed into a `string` or `[]byte` and never otherwise reference the package,
you need the blank form `import _ "embed"` or the file fails to compile.

### Example functions are compile-checked, runnable documentation

An `ExampleXxx` function with a trailing `// Output:` comment is run by `go test`,
which compares its captured stdout to the comment; a mismatch fails the test. The
same function is surfaced by `godoc` as documentation. So examples double as docs
that cannot drift from the API — if the signature changes, the example stops
compiling; if the behavior changes, the output comparison fails. For output whose
line order is non-deterministic (ranging a map), use `// Unordered output:`, which
compares the lines as a sorted set.

## Common Mistakes

### Two different package clauses in one directory

Wrong: `package grep` in `grep.go` and `package main` in `main.go` in the same
folder. The compiler rejects it. The only legal second package name in a directory
is the external test package `foo_test`.

### A fake reference to silence an "unused import"

Wrong: keeping `import "strings"` and adding `_ = strings.TrimSpace` so it
compiles. Unused imports (and unused locals) are compile errors, but a *blank
import* `_ "path"` and the blank identifier are always allowed. If you do not use
a package, remove the import — do not add dead references to appease the compiler.

### Relative imports

Wrong: `import "./internal/grep"`. Go module mode does not support relative import
paths. Fix: import via the full module path,
`example.com/pkgsimports/internal/grep`.

### Assuming you can order init() across packages

Only dependency edges are honored; within a package it is lexical filename order.
Encoding startup ordering into filenames is fragile — if two packages must
initialize in a specific order, create an explicit dependency edge, don't rename
files.

### pprof or expvar on a public mux

Wrong: `import _ "net/http/pprof"` (or mounting `expvar`) while
`http.DefaultServeMux` is served on a public listener — this exposes heap/CPU
profiles and internal metrics to the internet. Fix: serve `/debug` on a private
admin mux bound to an internal-only listener.

### Duplicate registration panics at startup

`database/sql.Register`, `expvar.Publish`, and a registry that mirrors them call
`panic`/`log.Panic` on a repeated name. Two packages registering the same key
crash the process at startup, not at first use.

### Forgetting import _ "embed"

When `//go:embed` targets a `string` or `[]byte` and the `embed` package is not
otherwise referenced, the file fails to compile without `import _ "embed"`. (The
`embed.FS` form references the package by type, so a normal `import "embed"`
suffices.)

### Relying on gofmt to fix import grouping

`gofmt` sorts within groups but never reorders or merges them; only `goimports`
regroups. CI that runs one on some paths and the other elsewhere yields perpetual
diffs. Standardize on one formatter.

### Dot-importing into application code

`import . "path"` pollutes the file scope and makes call sites ambiguous and
un-greppable. Reserve it for test DSLs.

### bufio.Scanner silently failing on long lines

`bufio.Scanner` errors (`bufio.ErrTooLong`) on any line longer than
`bufio.MaxScanTokenSize` (64 KiB). A log/grep processor drops the rest of the
input the moment it meets one long line, until you raise the limit with
`Scanner.Buffer` or switch to `bufio.Reader`.

### Merging packages to fix a cycle

An import cycle is fixed by inverting the dependency with a consumer-side
interface, not by collapsing the two packages into one blob that loses the
boundary you wanted.

### Confusing package name, directory name, and import path

They are three separate things. A mismatch compiles but misleads readers and
tooling; keep them aligned unless you have a specific reason not to.

Next: [01-grep-matcher-library.md](01-grep-matcher-library.md)
