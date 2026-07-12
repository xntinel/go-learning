# Exercise 9: Composable Preset Bundles and Option Ordering Contracts

An option is just a function, so an option can *return* another option and one
option can bundle several. This module uses that to build preset bundles —
`WithProductionDefaults()` and `WithTestDefaults()` fan out into several primitive
options — and to make option ordering an explicit, tested part of the contract:
later options override earlier ones, and a preset's fields survive unless a
later explicit option replaces them.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
publisher/                       independent module: example.com/publisher
  go.mod                         go 1.26
  publisher.go                   Publisher, Option, NewPublisher, WithBatchSize,
                                 WithFlushInterval, WithAcks, WithOptions,
                                 WithProductionDefaults, WithTestDefaults
  cmd/
    demo/
      main.go                    applies a preset then overrides one field
  publisher_test.go              table-driven override tests, last-wins, invalid sub-option
```

- Files: `publisher.go`, `cmd/demo/main.go`, `publisher_test.go`.
- Implement: `NewPublisher(opts...) (*Publisher, error)` with primitive options, a `WithOptions` combiner that applies a slice of options, and two presets built on it.
- Test: apply a preset then an explicit override and assert the override won while other preset fields survived; apply the same primitive twice and assert last-wins; assert a preset containing an invalid sub-option surfaces the error.
- Verify: `go test -count=1 ./...`

### An option that applies options

The key building block is `WithOptions`, an option that loops over a slice of
other options and applies them in order:

```go
func WithOptions(opts ...Option) Option {
	return func(p *Publisher) error {
		for _, opt := range opts {
			if err := opt(p); err != nil {
				return err
			}
		}
		return nil
	}
}
```

Because it is itself an `Option`, it slots into the same `NewPublisher(opts...)`
loop as any primitive, and it short-circuits on the first sub-option error — so an
invalid sub-option inside a preset surfaces through the normal constructor
boundary, exactly like a top-level option would. Presets are then just named
`WithOptions` bundles: `WithProductionDefaults()` returns
`WithOptions(WithBatchSize(500), WithFlushInterval(200*time.Millisecond), WithAcks(-1))`.
A team encodes "how we run this in production" once and reuses it everywhere.

### Ordering is a tested contract

Options run sequentially and these options *replace* their field, so ordering is
observable: `NewPublisher(WithProductionDefaults(), WithBatchSize(1))` yields a
batch size of 1 because the explicit option ran after the preset, while the
preset's flush interval and acks survive untouched. This last-writer-wins behavior
is not left implicit here — it is pinned by tests, so callers can rely on "put your
override after the preset" as a documented guarantee. That is the honest way to
handle ordering: make it part of the contract and test it, rather than leaving it
undefined.

The `acks` field mirrors a real message-broker setting: `-1` means "wait for all
replicas", `0` means "fire and forget", `1` means "wait for the leader". Only
those three values are valid, which gives `WithAcks` a real validation rule.

Create `publisher.go`:

```go
package publisher

import (
	"fmt"
	"time"
)

// Publisher batches messages before flushing them to a broker.
type Publisher struct {
	batchSize     int
	flushInterval time.Duration
	acks          int
}

// Option configures a Publisher and may reject invalid input.
type Option func(*Publisher) error

// NewPublisher builds a Publisher, seeding defaults and applying opts in order.
func NewPublisher(opts ...Option) (*Publisher, error) {
	p := &Publisher{
		batchSize:     100,
		flushInterval: time.Second,
		acks:          1,
	}
	for _, opt := range opts {
		if err := opt(p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// WithBatchSize sets the number of messages per batch (> 0).
func WithBatchSize(n int) Option {
	return func(p *Publisher) error {
		if n < 1 {
			return fmt.Errorf("batch size must be >= 1, got %d", n)
		}
		p.batchSize = n
		return nil
	}
}

// WithFlushInterval sets the maximum time before a partial batch is flushed.
func WithFlushInterval(d time.Duration) Option {
	return func(p *Publisher) error {
		if d <= 0 {
			return fmt.Errorf("flush interval must be positive, got %s", d)
		}
		p.flushInterval = d
		return nil
	}
}

// WithAcks sets the acknowledgement level: -1 (all), 0 (none), or 1 (leader).
func WithAcks(acks int) Option {
	return func(p *Publisher) error {
		switch acks {
		case -1, 0, 1:
			p.acks = acks
			return nil
		default:
			return fmt.Errorf("acks must be -1, 0, or 1, got %d", acks)
		}
	}
}

// WithOptions applies a slice of options as a single option, short-circuiting on
// the first error. It is the building block for presets.
func WithOptions(opts ...Option) Option {
	return func(p *Publisher) error {
		for _, opt := range opts {
			if err := opt(p); err != nil {
				return err
			}
		}
		return nil
	}
}

// WithProductionDefaults bundles the production-tuned settings.
func WithProductionDefaults() Option {
	return WithOptions(
		WithBatchSize(500),
		WithFlushInterval(200*time.Millisecond),
		WithAcks(-1),
	)
}

// WithTestDefaults bundles fast, low-durability settings for tests.
func WithTestDefaults() Option {
	return WithOptions(
		WithBatchSize(1),
		WithFlushInterval(time.Millisecond),
		WithAcks(0),
	)
}

// BatchSize returns the configured batch size.
func (p *Publisher) BatchSize() int { return p.batchSize }

// FlushInterval returns the configured flush interval.
func (p *Publisher) FlushInterval() time.Duration { return p.flushInterval }

// Acks returns the configured acknowledgement level.
func (p *Publisher) Acks() int { return p.acks }
```

### The runnable demo

The demo applies the production preset and then overrides only the batch size,
printing all three fields to show the override won while the rest of the preset
survived.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/publisher"
)

func main() {
	p, err := publisher.NewPublisher(
		publisher.WithProductionDefaults(),
		publisher.WithBatchSize(250), // overrides the preset's 500
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf("batch size: %d\n", p.BatchSize())
	fmt.Printf("flush interval: %s\n", p.FlushInterval())
	fmt.Printf("acks: %d\n", p.Acks())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batch size: 250
flush interval: 200ms
acks: -1
```

### Tests

`TestPresetOverride` is table-driven over override scenarios, asserting the
explicit option beat the preset while other preset fields survived.
`TestLastWins` applies the same primitive twice and asserts the later value.
`TestInvalidSubOptionSurfaces` composes a preset with an invalid sub-option and
asserts the constructor returns the error.

Create `publisher_test.go`:

```go
package publisher

import (
	"fmt"
	"testing"
	"time"
)

func TestPresetOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		opts      []Option
		wantBatch int
		wantFlush time.Duration
		wantAcks  int
	}{
		{
			name:      "production preset alone",
			opts:      []Option{WithProductionDefaults()},
			wantBatch: 500,
			wantFlush: 200 * time.Millisecond,
			wantAcks:  -1,
		},
		{
			name:      "override batch after preset",
			opts:      []Option{WithProductionDefaults(), WithBatchSize(1)},
			wantBatch: 1,
			wantFlush: 200 * time.Millisecond, // preset survives
			wantAcks:  -1,                     // preset survives
		},
		{
			name:      "test preset alone",
			opts:      []Option{WithTestDefaults()},
			wantBatch: 1,
			wantFlush: time.Millisecond,
			wantAcks:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, err := NewPublisher(tt.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if p.BatchSize() != tt.wantBatch {
				t.Errorf("BatchSize = %d, want %d", p.BatchSize(), tt.wantBatch)
			}
			if p.FlushInterval() != tt.wantFlush {
				t.Errorf("FlushInterval = %s, want %s", p.FlushInterval(), tt.wantFlush)
			}
			if p.Acks() != tt.wantAcks {
				t.Errorf("Acks = %d, want %d", p.Acks(), tt.wantAcks)
			}
		})
	}
}

func TestLastWins(t *testing.T) {
	t.Parallel()

	p, err := NewPublisher(WithBatchSize(10), WithBatchSize(20))
	if err != nil {
		t.Fatal(err)
	}
	if p.BatchSize() != 20 {
		t.Fatalf("BatchSize = %d, want 20 (last wins)", p.BatchSize())
	}
}

func TestInvalidSubOptionSurfaces(t *testing.T) {
	t.Parallel()

	_, err := NewPublisher(WithOptions(
		WithFlushInterval(time.Second),
		WithBatchSize(0), // invalid, must surface
	))
	if err == nil {
		t.Fatal("expected error from invalid sub-option, got nil")
	}
}

func ExampleNewPublisher() {
	p, _ := NewPublisher(WithTestDefaults())
	fmt.Printf("%d %d\n", p.BatchSize(), p.Acks())
	// Output: 1 0
}
```

## Review

The publisher is correct when a preset is indistinguishable from applying its
primitives by hand and when ordering is a guarantee rather than an accident.
`TestPresetOverride` pins both: the preset alone produces its full settings, and an
explicit option after it overrides exactly one field while the rest survive.
`TestLastWins` makes the last-writer-wins rule an assertion, and
`TestInvalidSubOptionSurfaces` proves `WithOptions` propagates a sub-option's error
through the constructor boundary instead of swallowing it. The design lesson is
that composition falls out for free once an option is just a function — no special
machinery is needed to bundle or nest them.

## Resources

- [Rob Pike: Self-referential functions and the design of options](https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design-of.html)
- [Uber Go Style Guide: Functional Options](https://github.com/uber-go/guide/blob/master/style.md#functional-options)
- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [Kafka producer acks configuration](https://kafka.apache.org/documentation/#producerconfigs_acks)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-config-precedence-options.md](08-config-precedence-options.md) | Next: [10-consumer-dlq-options.md](10-consumer-dlq-options.md)
