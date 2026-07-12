# Exercise 11: Worker Pool Options With an Overflow Policy Enum

**Nivel: Intermedio** — validacion rapida (un test corto).

A bounded worker pool has two sizes — worker count and queue depth — plus a
policy for what happens when the queue fills up. This module configures all
three through options, including one whose valid input is a small closed set
of named values rather than a range, plus a size invariant only visible once
both sizes are known.

## What you'll build

```text
workerpool/               independent module: example.com/workerpool
  go.mod                  go 1.24
  workerpool.go            Pool, OverflowPolicy, Option, New, WithWorkers,
                           WithQueueDepth, WithOverflowPolicy
  workerpool_test.go       table test over sizes and overflow policy values
```

- Implement `New(opts ...Option) (*Pool, error)`: two numeric options, one
  enum-validating option, and a constructor invariant relating the two sizes.
- Test defaults, a valid custom configuration, a queue shallower than the
  worker count, and an out-of-range `OverflowPolicy` value.

Set up the module:

```bash
go mod edit -go=1.24
```

`OverflowPolicy` is a small closed set — `Block`, `DropNewest`, `DropOldest` —
and being an `int` under the hood, nothing stops a caller writing
`OverflowPolicy(99)`; only a `switch` inside the option can reject it. And
neither `WithWorkers` nor `WithQueueDepth` can catch a queue shallower than
the worker count on its own: a pool with 10 workers and a 2-slot queue can
never keep more than 2 workers fed. That relationship only exists once both
fields are set, so `New` checks it after the option loop.

Create `workerpool.go`:

```go
package workerpool

import "fmt"

type OverflowPolicy int

const (
	Block OverflowPolicy = iota
	DropNewest
	DropOldest
)

type Pool struct {
	workers    int
	queueDepth int
	overflow   OverflowPolicy
}

type Option func(*Pool) error

// New seeds defaults, applies opts in order, then checks the invariant no
// single option could see: a queue shorter than the worker count can never
// keep every worker busy.
func New(opts ...Option) (*Pool, error) {
	p := &Pool{workers: 4, queueDepth: 16, overflow: Block}
	for _, opt := range opts {
		if err := opt(p); err != nil {
			return nil, err
		}
	}
	if p.queueDepth < p.workers {
		return nil, fmt.Errorf("queue depth %d is smaller than worker count %d: the queue could never keep every worker busy", p.queueDepth, p.workers)
	}
	return p, nil
}

func WithWorkers(n int) Option {
	return func(p *Pool) error {
		if n < 1 {
			return fmt.Errorf("workers must be >= 1, got %d", n)
		}
		p.workers = n
		return nil
	}
}

func WithQueueDepth(n int) Option {
	return func(p *Pool) error {
		if n < 1 {
			return fmt.Errorf("queue depth must be >= 1, got %d", n)
		}
		p.queueDepth = n
		return nil
	}
}

func WithOverflowPolicy(policy OverflowPolicy) Option {
	return func(p *Pool) error {
		switch policy {
		case Block, DropNewest, DropOldest:
			p.overflow = policy
			return nil
		default:
			return fmt.Errorf("unknown overflow policy: %d", policy)
		}
	}
}

func (p *Pool) Workers() int             { return p.workers }
func (p *Pool) QueueDepth() int          { return p.queueDepth }
func (p *Pool) Overflow() OverflowPolicy { return p.overflow }
```

Create `workerpool_test.go`:

```go
package workerpool

import "testing"

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		opts         []Option
		wantErr      bool
		wantOverflow OverflowPolicy
	}{
		{name: "defaults only", wantOverflow: Block},
		{name: "valid custom sizes and policy", opts: []Option{WithWorkers(8), WithQueueDepth(32), WithOverflowPolicy(DropOldest)}, wantOverflow: DropOldest},
		{name: "queue shorter than workers", opts: []Option{WithWorkers(10), WithQueueDepth(2)}, wantErr: true},
		{name: "invalid worker count", opts: []Option{WithWorkers(0)}, wantErr: true},
		{name: "unknown overflow policy", opts: []Option{WithOverflowPolicy(OverflowPolicy(99))}, wantErr: true},
		{name: "queue depth equal to workers is allowed", opts: []Option{WithWorkers(5), WithQueueDepth(5)}, wantOverflow: Block},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, err := New(tt.opts...)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Overflow() != tt.wantOverflow {
				t.Errorf("Overflow() = %v, want %v", p.Overflow(), tt.wantOverflow)
			}
			if p.QueueDepth() < p.Workers() {
				t.Errorf("QueueDepth() = %d < Workers() = %d, invariant should have blocked this", p.QueueDepth(), p.Workers())
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The pool is correct when its two sizes are individually sane and jointly
workable, and when an `OverflowPolicy` outside the three named constants is
rejected rather than silently accepted. The `switch` in `WithOverflowPolicy`
is the general technique for validating a closed-set option; the
queue-versus-workers check is the general technique for a capacity invariant
that only two options together can violate.

## Resources

- [Effective Go: Constants and iota](https://go.dev/doc/effective_go#constants)
- [Uber Go Style Guide: Functional Options](https://github.com/uber-go/guide/blob/master/style.md#functional-options)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-consumer-dlq-options.md](10-consumer-dlq-options.md) | Next: [12-notification-preferences-field-errors.md](12-notification-preferences-field-errors.md)
