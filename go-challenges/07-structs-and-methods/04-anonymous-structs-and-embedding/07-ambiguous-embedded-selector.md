# Exercise 7: Resolving Ambiguous Selectors When Merging Config Sources

Configuration comes from several sources — a file and the environment — and both
define the same keys. If you model that by embedding both into one merged type,
the compiler refuses to guess which `Timeout` you mean: a bare `cfg.Timeout` is an
*ambiguous-selector compile error*. This exercise builds the merged config the
right way, resolving the ambiguity with explicit qualification and an outer method
that imposes a precedence order.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
config/                     independent module: example.com/config
  go.mod                    module example.com/config
  config.go                 FileConfig, EnvConfig (both have Timeout + Source); Config resolves
  cmd/
    demo/
      main.go               build a merged config; print resolved timeout and source
  config_test.go            precedence, explicit qualification, both-zero fallback
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `FileConfig` and `EnvConfig`, each with a `Timeout time.Duration` field
and a `Source() string` method, and a `Config` embedding both that defines its own
`Timeout()` method and `Source()` method resolving env-over-file precedence.
Test: the outer `Timeout`/`Source` return the higher-precedence source's value;
both embedded fields remain reachable by explicit qualification; when neither
source sets a timeout, the fallback is the file's zero value.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/config/cmd/demo
cd ~/go-exercises/config
go mod init example.com/config
```

### Why the bare selector will not compile, and how to fix it

`FileConfig` and `EnvConfig` each declare a `Timeout` field and a `Source()`
method. Embed both into `Config` and they sit at the *same depth* (depth 1). The
selector rule is shallowest-depth-wins, and a *tie* at the shallowest depth is not
resolved silently — it is an error. So with only the two embeds, writing
`cfg.Timeout` or `cfg.Source()` fails to compile with "ambiguous selector". This
is a feature: Go refuses to pick a precedence for you, because getting config
precedence wrong is a real production bug.

There are two fixes, and this exercise uses both. First, *explicit qualification*:
`cfg.EnvConfig.Timeout` and `cfg.FileConfig.Timeout` name exactly which embedded
value you mean, and always compile. Second, *shadowing by an outer declaration*:
define `Timeout()` and `Source()` directly on `Config`. A declaration at depth 0
wins over the promoted depth-1 candidates, so `cfg.Timeout()` now unambiguously
calls the outer method — and that method encodes the precedence you want (an env
value overrides a file value; fall back to the file when env is unset). Note that
`Config.Timeout()` is a *method* while the embedded `Timeout` are *fields*; the
depth-0 method shadows the depth-1 fields cleanly, so there is no field/method
collision on `Config` itself.

Create `config.go`:

```go
package config

import "time"

// FileConfig is configuration loaded from a file.
type FileConfig struct {
	Timeout time.Duration
}

// Source identifies where this configuration came from.
func (FileConfig) Source() string { return "file" }

// EnvConfig is configuration loaded from the environment.
type EnvConfig struct {
	Timeout time.Duration
}

// Source identifies where this configuration came from.
func (EnvConfig) Source() string { return "env" }

// Config merges both sources by embedding them. Because both embeds declare
// Timeout and Source at the same depth, a bare cfg.Timeout / cfg.Source() would
// be an AMBIGUOUS-SELECTOR compile error. Config resolves it by defining its own
// Timeout and Source that impose env-over-file precedence.
type Config struct {
	FileConfig
	EnvConfig
}

// Timeout resolves the ambiguous promoted field: the env value wins when set,
// otherwise the file value. This depth-0 method shadows the depth-1 fields.
func (c Config) Timeout() time.Duration {
	if c.EnvConfig.Timeout != 0 {
		return c.EnvConfig.Timeout
	}
	return c.FileConfig.Timeout
}

// Source resolves the ambiguous promoted method with the same precedence.
func (c Config) Source() string {
	if c.EnvConfig.Timeout != 0 {
		return c.EnvConfig.Source()
	}
	return c.FileConfig.Source()
}
```

Note the comment about the bare selector is a *documented* fact, not code we
compile: if `config.go` contained the line `_ = c.EnvConfig.Timeout` that is fine,
but a bare `_ = Config{}.Timeout` used as a field would not compile once `Timeout`
is a method — and with two embeds and no outer method it would not compile as an
ambiguous selector either. The outer method is what makes `cfg.Timeout()` legal.

### The runnable demo

The demo builds a config where the environment overrides the file timeout and
prints the resolved values.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/config"
)

func main() {
	cfg := config.Config{
		FileConfig: config.FileConfig{Timeout: 30 * time.Second},
		EnvConfig:  config.EnvConfig{Timeout: 5 * time.Second},
	}

	fmt.Printf("resolved timeout: %s\n", cfg.Timeout())
	fmt.Printf("winning source: %s\n", cfg.Source())
	fmt.Printf("file timeout (explicit): %s\n", cfg.FileConfig.Timeout)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
resolved timeout: 5s
winning source: env
file timeout (explicit): 30s
```

### Tests

The tests pin the precedence (env wins when set, file otherwise), prove both
embedded fields are reachable by explicit qualification, and cover the both-zero
fallback.

Create `config_test.go`:

```go
package config

import (
	"testing"
	"time"
)

func TestEnvOverridesFile(t *testing.T) {
	t.Parallel()

	cfg := Config{
		FileConfig: FileConfig{Timeout: 30 * time.Second},
		EnvConfig:  EnvConfig{Timeout: 5 * time.Second},
	}
	if got := cfg.Timeout(); got != 5*time.Second {
		t.Fatalf("Timeout() = %s, want 5s (env wins)", got)
	}
	if got := cfg.Source(); got != "env" {
		t.Fatalf("Source() = %q, want env", got)
	}
}

func TestFileUsedWhenEnvUnset(t *testing.T) {
	t.Parallel()

	cfg := Config{FileConfig: FileConfig{Timeout: 30 * time.Second}}
	if got := cfg.Timeout(); got != 30*time.Second {
		t.Fatalf("Timeout() = %s, want 30s (file fallback)", got)
	}
	if got := cfg.Source(); got != "file" {
		t.Fatalf("Source() = %q, want file", got)
	}
}

func TestBothEmbedsReachableByQualification(t *testing.T) {
	t.Parallel()

	cfg := Config{
		FileConfig: FileConfig{Timeout: 30 * time.Second},
		EnvConfig:  EnvConfig{Timeout: 5 * time.Second},
	}
	// A bare cfg.Timeout is an ambiguous-selector compile error; explicit
	// qualification always works.
	if cfg.FileConfig.Timeout != 30*time.Second {
		t.Fatalf("FileConfig.Timeout = %s, want 30s", cfg.FileConfig.Timeout)
	}
	if cfg.EnvConfig.Timeout != 5*time.Second {
		t.Fatalf("EnvConfig.Timeout = %s, want 5s", cfg.EnvConfig.Timeout)
	}
	if cfg.FileConfig.Source() != "file" || cfg.EnvConfig.Source() != "env" {
		t.Fatal("qualified Source() calls returned the wrong values")
	}
}

func TestBothZeroFallsBackToFileZero(t *testing.T) {
	t.Parallel()

	var cfg Config
	if got := cfg.Timeout(); got != 0 {
		t.Fatalf("Timeout() = %s, want 0 (both unset)", got)
	}
	if got := cfg.Source(); got != "file" {
		t.Fatalf("Source() = %q, want file (fallback)", got)
	}
}
```

## Review

The merged config is correct when `cfg.Timeout()` and `cfg.Source()` return the
env value if the env set one and the file value otherwise, and when both embedded
sources stay reachable through explicit qualification. The concept the compiler
enforces for you: two embeds exposing the same name make the bare selector an
error, not a silent pick — you must qualify or shadow. The mistakes to avoid:
assuming Go will "just merge" the two `Timeout` fields (it will not, and that is
protective); trying to declare a `Timeout` *field* on `Config` while the embeds
also carry one (define a method or accessor instead, choosing the precedence);
and forgetting that qualification (`cfg.EnvConfig.Timeout`) is always available as
the escape hatch.

## Resources

- [Go Specification: Selectors](https://go.dev/ref/spec#Selectors) — promotion depth and the ambiguous-selector rule.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — embedded fields and how names promote.
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — resolving name clashes with an outer declaration.

---

Prev: [06-grpc-forward-compat-embedding.md](06-grpc-forward-compat-embedding.md) | Back to [00-concepts.md](00-concepts.md) | Next: [08-embedded-json-flattening.md](08-embedded-json-flattening.md)
