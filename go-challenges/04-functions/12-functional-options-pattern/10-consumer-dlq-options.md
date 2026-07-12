# Exercise 10: Message Consumer Options With a Conditional DLQ Toggle

**Nivel: Intermedio** — validacion rapida (un test corto).

A message consumer has knobs a team tunes per environment: batch size, how
many messages may be unacknowledged at once, and whether failed messages get
routed to a dead-letter queue. That last one is a toggle plus a destination,
and the two only make sense together — this module enforces the rule that a
single option cannot see on its own: DLQ on with no topic is invalid.

## What you'll build

```text
consumer/                 independent module: example.com/consumer
  go.mod                  go 1.24
  consumer.go             Consumer, Option, New, WithBatchSize, WithMaxInFlight,
                           WithDLQEnabled, WithDLQTopic
  consumer_test.go         table test over valid/invalid combinations
```

- Implement `New(opts ...Option) (*Consumer, error)`: two numeric options, one
  total boolean toggle (`WithDLQEnabled`), one string option (`WithDLQTopic`).
- Test the cross-field rule: DLQ enabled without a topic fails, DLQ enabled
  with a topic succeeds, a topic set without ever enabling the DLQ stays off.

Set up the module:

```bash
go mod edit -go=1.24
```

`WithDLQEnabled(bool)` is *total* — whatever `bool` you pass, it always
succeeds — while the other three are partial. Both shapes return the same
`func(*Consumer) error`. Neither `WithDLQEnabled` nor `WithDLQTopic` can catch
"enabled but no topic" alone, since each only sees its own argument; that
invariant only becomes visible once every option has run, which is why `New`
checks it, not either option.

Create `consumer.go`:

```go
package consumer

import "fmt"

type Consumer struct {
	batchSize   int
	maxInFlight int
	dlqEnabled  bool
	dlqTopic    string
}

type Option func(*Consumer) error

// New seeds defaults, applies opts in order, then checks the one invariant
// no single option could see: DLQ enabled implies a non-empty topic.
func New(opts ...Option) (*Consumer, error) {
	c := &Consumer{batchSize: 10, maxInFlight: 100}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	if c.dlqEnabled && c.dlqTopic == "" {
		return nil, fmt.Errorf("DLQ enabled but no DLQ topic configured")
	}
	return c, nil
}

func WithBatchSize(n int) Option {
	return func(c *Consumer) error {
		if n < 1 {
			return fmt.Errorf("batch size must be >= 1, got %d", n)
		}
		c.batchSize = n
		return nil
	}
}

func WithMaxInFlight(n int) Option {
	return func(c *Consumer) error {
		if n < 1 {
			return fmt.Errorf("max in-flight must be >= 1, got %d", n)
		}
		c.maxInFlight = n
		return nil
	}
}

// WithDLQEnabled is total: it can never fail.
func WithDLQEnabled(enabled bool) Option {
	return func(c *Consumer) error {
		c.dlqEnabled = enabled
		return nil
	}
}

func WithDLQTopic(topic string) Option {
	return func(c *Consumer) error {
		if topic == "" {
			return fmt.Errorf("DLQ topic must not be empty")
		}
		c.dlqTopic = topic
		return nil
	}
}

func (c *Consumer) BatchSize() int   { return c.batchSize }
func (c *Consumer) MaxInFlight() int { return c.maxInFlight }
func (c *Consumer) DLQEnabled() bool { return c.dlqEnabled }
func (c *Consumer) DLQTopic() string { return c.dlqTopic }
```

Create `consumer_test.go`:

```go
package consumer

import "testing"

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
		wantDLQ bool
	}{
		{name: "defaults only"},
		{name: "valid custom values", opts: []Option{WithBatchSize(25), WithMaxInFlight(500)}},
		{name: "invalid batch size", opts: []Option{WithBatchSize(0)}, wantErr: true},
		{name: "DLQ enabled without topic", opts: []Option{WithDLQEnabled(true)}, wantErr: true},
		{name: "DLQ enabled with topic", opts: []Option{WithDLQEnabled(true), WithDLQTopic("orders-dlq")}, wantDLQ: true},
		{name: "empty DLQ topic rejected", opts: []Option{WithDLQTopic("")}, wantErr: true},
		{name: "topic alone does not enable DLQ", opts: []Option{WithDLQTopic("orders-dlq")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c, err := New(tt.opts...)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.BatchSize() < 1 || c.MaxInFlight() < 1 {
				t.Fatalf("invalid built consumer: %+v", c)
			}
			if c.DLQEnabled() != tt.wantDLQ {
				t.Errorf("DLQEnabled() = %v, want %v", c.DLQEnabled(), tt.wantDLQ)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The consumer is correct when the DLQ can only be in two coherent states — off,
or on with a real destination — never a third where it is "on" but silently
drops messages nowhere. Neither option can enforce that alone; `New` is the
one place both fields are visible together, so it is the one place the
combination can be checked. The lesson generalizes to any toggle paired with
configuration that only matters once the toggle is on.

## Resources

- [Uber Go Style Guide: Functional Options](https://github.com/uber-go/guide/blob/master/style.md#functional-options)
- [AWS: Amazon SQS dead-letter queues](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-dead-letter-queues.html)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-preset-and-ordering-options.md](09-preset-and-ordering-options.md) | Next: [11-worker-pool-overflow-options.md](11-worker-pool-overflow-options.md)
