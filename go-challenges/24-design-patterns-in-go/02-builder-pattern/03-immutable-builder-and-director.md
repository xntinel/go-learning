# Exercise 3: An Immutable Value Builder And A Director

The fluent pointer-builder of Exercise 1 is mutable and unsafe to share across goroutines. This exercise builds the opposite: a `ServerConfig` builder that is a *value* type whose setters copy-on-write, so a base builder can be shared as a starting point and forked freely with no mutex and no race. On top of it sits a *Director* — the classic Builder-pattern role — captured in Go as a plain function that names a reusable construction recipe.

This module is fully self-contained. It starts with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
config.go            ServerConfig, the value Builder, value-receiver setters, Director, Production
cmd/
  demo/
    main.go          fork a shared base into a dev and a prod config
config_test.go       immutability, fork independence, concurrent forks under -race, the Director
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `ServerConfig`; a value `Builder` with value-receiver setters (`Host`, `Port`, `TLS`, `MaxConns`, `ReadTimeout`) and `Build`; the `Director` function type with an `Apply` method; and the `Production` recipe.
- Test: `config_test.go` proves forking does not mutate the base, that two forks are independent, that concurrent forks of one base do not race, and that `Production` yields the expected config without mutating its base.
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/02-builder-pattern/03-immutable-builder-and-director/cmd/demo && cd go-solutions/24-design-patterns-in-go/02-builder-pattern/03-immutable-builder-and-director
```

### Why a value receiver makes the builder safe to share

The fluent builder of Exercise 1 used a pointer receiver: every setter mutated the one struct the pointer pointed at, so two goroutines chaining on the same `*RequestBuilder` wrote to the same maps and raced. Switch the receiver to a *value* and the whole hazard disappears. When a method declares `func (b Builder) Host(h string) Builder`, Go passes the receiver *by copy*: `b` inside the method is a private copy of the caller's builder. Mutating `b.cfg.Host` mutates only that copy, and returning `b` hands the caller the modified copy. The builder the caller started from is never touched.

That single change turns the builder immutable in the sense that matters: no operation ever mutates a builder another goroutine can see. A base builder becomes a value you can share by copying — `base := New().MaxConns(500)` — and then *fork* in as many directions as you like, concurrently, with no synchronisation, because each fork operates on its own copy. Copying is the synchronisation. `TestBuilder_ConcurrentForksDoNotRace` launches sixty-four goroutines that each fork one shared base and build, and it passes clean under `-race` precisely because no two of them touch the same memory.

There is one boundary condition the concepts file flagged and this exercise respects: value-copying a struct copies its fields, but a slice or map field is a header whose backing array is *shared* by the copy. A value builder whose product held a `[]string` would let an `append` in one fork become visible in another, breaking the independence the pattern promises. So `ServerConfig` is deliberately all scalars (`string`, `int`, `bool`, `time.Duration`), for which a struct copy is a complete, independent copy. If you ever add a slice field, copy it inside the setter before mutating.

### Why a Director is just a function

Once a particular sequence of builder calls recurs — the way every production server is configured, say — it deserves a name, and naming construction recipes is exactly the job the Builder pattern assigns to a *Director*. The Director knows *which* steps to run, in *what* order, with *what* arguments; the builder knows only *how* each individual step mutates state. Keeping those two concerns apart means the recipe lives in one place and every caller that wants "the production config" gets the same one.

In Go the Director needs no class. A recipe is a transformation from a starting builder to a finished one, which is precisely `type Director func(Builder) Builder`. `Production(host)` returns such a function — a closure that hardens a base into a public, TLS-on, high-ceiling configuration — and the `Apply` method runs a recipe against a base and calls `Build` for you. Because the builder is a value, a Director cannot accidentally mutate the base it is handed: it receives a copy, threads it through the setters, and returns a fresh result, so the same `base` can be fed to several Directors to produce several independent configs. `TestDirector_DoesNotMutateBase` pins that.

Create `config.go`:

```go
package server

import "time"

// ServerConfig is the immutable product. Every field is a scalar, so copying a
// Builder by value copies the configuration completely: there is no shared
// slice or map backing array that two copies could both mutate.
type ServerConfig struct {
	Host        string
	Port        int
	TLS         bool
	MaxConns    int
	ReadTimeout time.Duration
}

// Builder is a value type, not a pointer. Every setter takes the receiver by
// value, mutates its own copy, and returns that copy. The receiver the caller
// held is never touched, so a Builder can be shared as a base and forked freely
// across goroutines without a mutex.
type Builder struct {
	cfg ServerConfig
}

// New returns a Builder seeded with defaults. The zero ServerConfig would be a
// valid but useless server (port 0, no connection ceiling), so the defaults
// encode a sensible starting point that every recipe refines.
func New() Builder {
	return Builder{cfg: ServerConfig{
		Host:        "localhost",
		Port:        8080,
		MaxConns:    100,
		ReadTimeout: 15 * time.Second,
	}}
}

func (b Builder) Host(h string) Builder {
	b.cfg.Host = h
	return b
}

func (b Builder) Port(p int) Builder {
	b.cfg.Port = p
	return b
}

func (b Builder) TLS(on bool) Builder {
	b.cfg.TLS = on
	return b
}

func (b Builder) MaxConns(n int) Builder {
	b.cfg.MaxConns = n
	return b
}

func (b Builder) ReadTimeout(d time.Duration) Builder {
	b.cfg.ReadTimeout = d
	return b
}

// Build returns the accumulated configuration. Because Builder is a value, the
// returned ServerConfig is independent of the Builder and of every other config
// built from the same base.
func (b Builder) Build() ServerConfig {
	return b.cfg
}

// Director is a reusable construction recipe: a function from a starting Builder
// to a finished one. It names a standard way to assemble a config (which steps,
// in what order) and keeps that knowledge in one place, separate from the
// Builder, which only knows how each individual step mutates state.
type Director func(Builder) Builder

// Production is a Director that hardens a base config for production: public
// host, TLS on the standard port, a high connection ceiling, and a generous
// read timeout.
func Production(host string) Director {
	return func(b Builder) Builder {
		return b.Host(host).
			Port(443).
			TLS(true).
			MaxConns(10000).
			ReadTimeout(30 * time.Second)
	}
}

// Apply runs the recipe against a base Builder and returns the finished config.
func (d Director) Apply(base Builder) ServerConfig {
	return d(base).Build()
}
```

Read any setter and the immutability is right there in the signature: `func (b Builder) Host(...) Builder`. The receiver is a value, so `b` is a copy; the method edits the copy and returns it; the caller's builder is untouched. No mutex appears anywhere in the file because none is needed.

### The runnable demo

The demo creates one shared `base`, then forks it two ways — a `dev` config by chaining setters directly, and a `prod` config by applying the `Production` Director — and prints all three. The base printing *after* both forks is the visible proof of immutability: its host, port, and TLS are exactly what they were, unaffected by either fork.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/immutable-builder"
)

func main() {
	// One shared base. Because the Builder is a value type, forking it cannot
	// affect the base or any sibling fork.
	base := server.New().MaxConns(500)

	dev := base.Host("127.0.0.1").Port(3000).Build()
	prod := server.Production("api.example.com").Apply(base)

	fmt.Printf("base:  host=%s port=%d tls=%v maxconns=%d\n",
		base.Build().Host, base.Build().Port, base.Build().TLS, base.Build().MaxConns)
	fmt.Printf("dev:   host=%s port=%d tls=%v maxconns=%d\n",
		dev.Host, dev.Port, dev.TLS, dev.MaxConns)
	fmt.Printf("prod:  host=%s port=%d tls=%v maxconns=%d read=%s\n",
		prod.Host, prod.Port, prod.TLS, prod.MaxConns, prod.ReadTimeout)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
base:  host=localhost port=8080 tls=false maxconns=500
dev:   host=127.0.0.1 port=3000 tls=false maxconns=500
prod:  host=api.example.com port=443 tls=true maxconns=10000 read=30s
```

Both forks inherit the base's `maxconns=500`, but `dev` and `prod` diverge in host, port, and TLS, and the base still reads `localhost:8080` with TLS off — the fork never reached back and changed it.

### Tests

The tests pin the two properties that justify the whole design: immutability and the Director. `TestBuilder_IsImmutable` forks a base and checks the base is unchanged. `TestBuilder_ForksAreIndependent` makes two forks from one base and checks they neither share the values they each set nor lose the value the base set. `TestBuilder_ConcurrentForksDoNotRace` is the payoff: sixty-four goroutines fork one shared base concurrently, and the test passing under `-race` is what "safe to share" means operationally. `TestDirector_Production` checks the recipe produces the exact expected config, and `TestDirector_DoesNotMutateBase` confirms applying a Director leaves its base untouched.

Create `config_test.go`:

```go
package server

import (
	"testing"
	"time"
)

func TestBuilder_IsImmutable(t *testing.T) {
	t.Parallel()

	base := New().Port(8080)

	// Forking the base must not mutate the base.
	_ = base.Port(9090).Host("other")

	if got := base.Build().Port; got != 8080 {
		t.Errorf("base Port changed to %d; value-receiver builder must be immutable", got)
	}
	if got := base.Build().Host; got != "localhost" {
		t.Errorf("base Host changed to %q; value-receiver builder must be immutable", got)
	}
}

func TestBuilder_ForksAreIndependent(t *testing.T) {
	t.Parallel()

	base := New().MaxConns(500)

	a := base.Host("a.example.com").Port(8081).Build()
	b := base.Host("b.example.com").Port(8082).Build()

	if a.Host == b.Host {
		t.Fatalf("forks share host: %q == %q", a.Host, b.Host)
	}
	if a.Port == b.Port {
		t.Fatalf("forks share port: %d == %d", a.Port, b.Port)
	}
	if a.MaxConns != 500 || b.MaxConns != 500 {
		t.Errorf("forks lost shared base value: a=%d b=%d", a.MaxConns, b.MaxConns)
	}
}

func TestBuilder_ConcurrentForksDoNotRace(t *testing.T) {
	t.Parallel()

	base := New().MaxConns(1000)

	const n = 64
	done := make(chan ServerConfig, n)
	for i := range n {
		go func() {
			done <- base.Port(9000 + i).Build()
		}()
	}
	for range n {
		cfg := <-done
		if cfg.MaxConns != 1000 {
			t.Errorf("MaxConns = %d, want 1000", cfg.MaxConns)
		}
	}
}

func TestDirector_Production(t *testing.T) {
	t.Parallel()

	cfg := Production("api.example.com").Apply(New())

	want := ServerConfig{
		Host:        "api.example.com",
		Port:        443,
		TLS:         true,
		MaxConns:    10000,
		ReadTimeout: 30 * time.Second,
	}
	if cfg != want {
		t.Errorf("Production config = %+v, want %+v", cfg, want)
	}
}

func TestDirector_DoesNotMutateBase(t *testing.T) {
	t.Parallel()

	base := New()
	_ = Production("api.example.com").Apply(base)

	if base.Build().TLS {
		t.Error("Director mutated the shared base builder")
	}
}
```

## Review

The builder is correct when every setter has a *value* receiver and returns the modified copy. Confirm there is no pointer receiver and no mutex in the file: safety comes from copying, not locking. The decisive test is `TestBuilder_ConcurrentForksDoNotRace` under `-race`; if any setter were changed to a pointer receiver, the shared base would be mutated concurrently and the race detector would fire. Confirm too that the product `ServerConfig` is all scalar fields, because the immutability guarantee holds only for fields that a struct copy duplicates completely — a slice or map field would be shared by reference and quietly reintroduce the race.

Common mistakes for this form. The first is reaching for a pointer receiver out of habit; that single character turns copy-on-write back into shared mutation and the whole safety argument collapses. The second is adding a slice or map field to the product and still calling forks independent; copy such a field inside the setter (`append([]T(nil), old...)`) before mutating, or keep the product scalar. The third is writing the Director as a method that mutates a builder it holds, instead of as a pure `func(Builder) Builder`; the value-builder makes the pure form effortless and keeps recipes reusable against any base. Run `go test -race -count=1 ./...` to confirm immutability, fork independence, and the race-free concurrent forks all hold.

## Resources

- [Go Data Structures (Russ Cox)](https://research.swtch.com/godata) — how Go lays out structs, slices, and maps in memory, which is why a value copy duplicates scalar fields but shares a slice's backing array.
- [Functional options for friendly APIs (Dave Cheney)](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — the closure-as-configuration idea the Director here reuses.
- [Builder in Go (Refactoring Guru)](https://refactoring.guru/design-patterns/builder/go/example) — the Builder pattern with an explicit Director role.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-staged-builder.md](02-staged-builder.md) | Next: [04-query-report-builder.md](04-query-report-builder.md)
