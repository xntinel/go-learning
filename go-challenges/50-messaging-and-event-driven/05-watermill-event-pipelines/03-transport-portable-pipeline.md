# Exercise 3: Transport-Portable Handlers — GoChannel in Tests, Redis Streams in Prod

The payoff of writing against `message.Publisher` and `message.Subscriber` is that
the same router runs against an in-memory transport in tests and a real broker in
production, changing only the constructor. Here the business handler and the
`BuildRouter` wiring depend solely on the two interfaces; the default build wires
GoChannel and is fully testable offline, and a build-tagged file wires Redis
Streams for production without touching a line of handler code.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
portable/                    independent module: example.com/portable
  go.mod                     go 1.26; watermill + (tagged) redisstream, go-redis
  pipeline.go                OrderEvent, ProcessedOrder, EnrichHandler, BuildRouter (interfaces only)
  transport_gochannel.go     //go:build !redis  -- NewTransport returns gochannel
  transport_redis.go         //go:build  redis  -- NewTransport returns Redis Streams
  cmd/
    demo/
      main.go                graceful shutdown via signal.NotifyContext
  pipeline_test.go           default build: publish, assert the handler processed it
```

- Files: `pipeline.go`, `transport_gochannel.go`, `transport_redis.go`, `cmd/demo/main.go`, `pipeline_test.go`.
- Implement: `BuildRouter(pub message.Publisher, sub message.Subscriber, logger)` (transport-agnostic) and two build-tagged `NewTransport` constructors with an identical signature.
- Test: the default (no-tag) build wires GoChannel through `NewTransport`, publishes an order, and asserts the enriched result reaches the output topic — proving the handler is transport-agnostic.
- Verify: `go test -count=1 -race ./...` for the default build; `go build -tags redis ./...` compiles the Redis wiring.

Set up the module:

```bash
mkdir -p ~/go-exercises/portable/cmd/demo
cd ~/go-exercises/portable
go mod init example.com/portable
go mod edit -go=1.26
go get github.com/ThreeDotsLabs/watermill@v1.5.2
go get github.com/ThreeDotsLabs/watermill-redisstream@v1.4.5
go get github.com/redis/go-redis/v9@v9.21.0
```

### The wiring depends only on interfaces

`BuildRouter` takes a `message.Publisher` and a `message.Subscriber` — never a
concrete `*gochannel.GoChannel` or `*redisstream.Publisher`. It subscribes the
handler to `InputTopic` via the subscriber and publishes to `OutputTopic` via the
publisher. Because the parameters are interfaces, the same function compiles and
runs against any transport that satisfies them, which is the entire reason the
handler is testable without a broker.

The only transport-specific code is `NewTransport`, and there are two of them
guarded by build tags. `transport_gochannel.go` carries `//go:build !redis` and
returns a single `*gochannel.GoChannel` used as both publisher and subscriber.
`transport_redis.go` carries `//go:build redis` and builds a `redis.Client`, a
`redisstream.NewPublisher`, and a `redisstream.NewSubscriber` sharing that client.
Both expose the identical signature `func NewTransport(logger
watermill.LoggerAdapter) (message.Publisher, message.Subscriber, func(), error)`,
so the caller — demo or test — is written once against that signature. The default
`go build`/`go test` compiles only the `!redis` file; `go build -tags redis`
swaps in the Redis file. Nothing else changes.

Create `pipeline.go`:

```go
package portable

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
)

const (
	InputTopic  = "orders.incoming"
	OutputTopic = "orders.processed"
)

// ErrInvalidOrder is the sentinel wrapped by validation failures.
var ErrInvalidOrder = errors.New("invalid order")

// OrderEvent is the inbound event shape.
type OrderEvent struct {
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
}

// ProcessedOrder is the enriched event the pipeline produces.
type ProcessedOrder struct {
	OrderID     string    `json:"order_id"`
	AmountCents int64     `json:"amount_cents"`
	Status      string    `json:"status"`
	ProcessedAt time.Time `json:"processed_at"`
}

// EnrichHandler validates and enriches an order. It depends on nothing but the
// message it is given, so it runs identically under any transport.
func EnrichHandler(msg *message.Message) ([]*message.Message, error) {
	var in OrderEvent
	if err := json.Unmarshal(msg.Payload, &in); err != nil {
		return nil, fmt.Errorf("%w: bad json: %v", ErrInvalidOrder, err)
	}
	if in.OrderID == "" || in.AmountCents <= 0 {
		return nil, fmt.Errorf("%w: order_id=%q amount_cents=%d", ErrInvalidOrder, in.OrderID, in.AmountCents)
	}

	out := ProcessedOrder{
		OrderID:     in.OrderID,
		AmountCents: in.AmountCents,
		Status:      "processed",
		ProcessedAt: time.Now().UTC(),
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal processed order: %w", err)
	}
	return []*message.Message{message.NewMessage(watermill.NewUUID(), payload)}, nil
}

// BuildRouter wires EnrichHandler with the standard middleware stack. It accepts
// the transport as interfaces, so the concrete pub/sub is chosen by the caller.
func BuildRouter(
	pub message.Publisher,
	sub message.Subscriber,
	logger watermill.LoggerAdapter,
) (*message.Router, error) {
	router, err := message.NewRouter(message.RouterConfig{CloseTimeout: 5 * time.Second}, logger)
	if err != nil {
		return nil, fmt.Errorf("new router: %w", err)
	}
	router.AddMiddleware(
		middleware.CorrelationID,
		middleware.Retry{MaxRetries: 3, InitialInterval: 10 * time.Millisecond, Logger: logger}.Middleware,
		middleware.Recoverer,
	)
	router.AddHandler("enrich-orders", InputTopic, sub, OutputTopic, pub, EnrichHandler)
	return router, nil
}
```

Create `transport_gochannel.go` — this is the default build (`!redis`):

```go
//go:build !redis

package portable

import (
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
)

// NewTransport returns an in-process GoChannel used as both publisher and
// subscriber. Hermetic and deterministic: no broker, no network.
func NewTransport(logger watermill.LoggerAdapter) (message.Publisher, message.Subscriber, func(), error) {
	pubSub := gochannel.NewGoChannel(gochannel.Config{}, logger)
	closeFn := func() { _ = pubSub.Close() }
	return pubSub, pubSub, closeFn, nil
}
```

### The production wiring behind a build tag

`transport_redis.go` is compiled only with `-tags redis`. It builds a
`redis.Client` (which satisfies `redis.UniversalClient`), a
`redisstream.NewPublisher`, and a `redisstream.NewSubscriber` that shares the same
client and joins a consumer group. The subscriber's `Consumer` must be unique per
process instance, so it is derived from `watermill.NewUUID`; the consumer group is
what gives Redis Streams its at-least-once, load-balanced delivery across
replicas. Because this file is excluded from the default build, the offline test
path never needs Redis, yet the wiring is real and compiles under its tag.

Create `transport_redis.go`:

```go
//go:build redis

package portable

import (
	"fmt"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill-redisstream/pkg/redisstream"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/redis/go-redis/v9"
)

// NewTransport returns Redis Streams publisher/subscriber sharing one client.
// Compiled only with `go build -tags redis`.
func NewTransport(logger watermill.LoggerAdapter) (message.Publisher, message.Subscriber, func(), error) {
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379"})

	pub, err := redisstream.NewPublisher(redisstream.PublisherConfig{Client: client}, logger)
	if err != nil {
		_ = client.Close()
		return nil, nil, nil, fmt.Errorf("redis publisher: %w", err)
	}

	sub, err := redisstream.NewSubscriber(redisstream.SubscriberConfig{
		Client:        client,
		ConsumerGroup: "orders-workers",
		Consumer:      watermill.NewUUID(),
	}, logger)
	if err != nil {
		_ = pub.Close()
		_ = client.Close()
		return nil, nil, nil, fmt.Errorf("redis subscriber: %w", err)
	}

	closeFn := func() {
		_ = pub.Close()
		_ = sub.Close()
		_ = client.Close()
	}
	return pub, sub, closeFn, nil
}
```

### Graceful shutdown in the demo

The demo shows the real shutdown mechanism: the context comes from
`signal.NotifyContext`, so a `SIGINT` or `SIGTERM` cancels it and `Run` drains
in-flight handlers up to `CloseTimeout` before returning. So that the demo
terminates on its own after doing its work, it also calls `stop()` once the two
orders are processed — the same code path a real signal would trigger.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"

	"example.com/portable"
)

func main() {
	logger := watermill.NopLogger{}
	pub, sub, closeTransport, err := portable.NewTransport(logger)
	if err != nil {
		log.Fatal(err)
	}
	defer closeTransport()

	router, err := portable.BuildRouter(pub, sub, logger)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runErr := make(chan error, 1)
	go func() { runErr <- router.Run(ctx) }()
	<-router.Running()

	out, err := sub.Subscribe(ctx, portable.OutputTopic)
	if err != nil {
		log.Fatal(err)
	}

	for _, id := range []string{"ord-1", "ord-2"} {
		payload, _ := json.Marshal(portable.OrderEvent{OrderID: id, AmountCents: 1000})
		if err := pub.Publish(portable.InputTopic, message.NewMessage(watermill.NewUUID(), payload)); err != nil {
			log.Fatal(err)
		}
	}

	for range 2 {
		got := <-out
		got.Ack()
		var p portable.ProcessedOrder
		if err := json.Unmarshal(got.Payload, &p); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("processed order %s status=%s\n", p.OrderID, p.Status)
	}

	stop()
	if err := <-runErr; err != nil {
		log.Fatal(err)
	}
	fmt.Println("router stopped cleanly")
}
```

`NewTransport` and `BuildRouter` both live in `package portable`, so the demo's
`package main` calls them as `portable.NewTransport` and `portable.BuildRouter`.
The build tag lives on the two `transport_*.go` files inside `portable`, so it is
that package that swaps implementations: `go run ./cmd/demo` builds `portable`
with GoChannel, and `go run -tags redis ./cmd/demo` rebuilds it with Redis
Streams. The demo source itself never changes.

Expected output (default GoChannel build):

```
processed order ord-1 status=processed
processed order ord-2 status=processed
router stopped cleanly
```

### Tests

The default test path is fully hermetic: it calls `NewTransport` (GoChannel under
`!redis`), builds the router through the shared `BuildRouter`, publishes an order,
and asserts the enriched result reaches the output topic. Because the test uses
the same `BuildRouter` the production path uses, a passing test proves the handler
and wiring are transport-agnostic — the only thing the Redis build changes is the
constructor.

Create `pipeline_test.go`:

```go
package portable

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
)

func TestPipelineProcessesEventsOverTransport(t *testing.T) {
	t.Parallel()
	logger := watermill.NopLogger{}
	pub, sub, closeTransport, err := NewTransport(logger)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	defer closeTransport()

	router, err := BuildRouter(pub, sub, logger)
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- router.Run(ctx) }()
	<-router.Running()

	out, err := sub.Subscribe(ctx, OutputTopic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	payload, _ := json.Marshal(OrderEvent{OrderID: "ord-77", AmountCents: 3300})
	if err := pub.Publish(InputTopic, message.NewMessage(watermill.NewUUID(), payload)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case got := <-out:
		got.Ack()
		var p ProcessedOrder
		if err := json.Unmarshal(got.Payload, &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.OrderID != "ord-77" || p.Status != "processed" {
			t.Fatalf("processed = %+v; want ord-77/processed", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for processed order")
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func ExampleEnrichHandler() {
	payload := []byte(`{"order_id":"ord-7","amount_cents":1999}`)
	produced, err := EnrichHandler(message.NewMessage("u", payload))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	var out ProcessedOrder
	_ = json.Unmarshal(produced[0].Payload, &out)
	fmt.Printf("%s %s %d\n", out.OrderID, out.Status, out.AmountCents)
	// Output: ord-7 processed 1999
}
```

## Review

The design is correct when the handler and `BuildRouter` never mention a concrete
transport type — grep the package for `gochannel` or `redisstream` and you should
find them only in the two `NewTransport` files. If the default test needs a broker
to pass, some transport-specific code leaked into `BuildRouter`. The build tags
are the load-bearing detail: `transport_gochannel.go` must carry `//go:build
!redis` and `transport_redis.go` must carry `//go:build redis`, each on its own
line followed by a blank line before `package portable`, or both files compile at
once and you get a duplicate `NewTransport`. The Redis file is documented and
compiles under `-tags redis` but is not exercised in default CI because it needs a
running broker; that is the chapter's rule that external and network code sits
behind a build tag. Confirm graceful shutdown by cancelling the context and
checking `Run` returns `nil` — a non-nil return means a handler did not drain
within `CloseTimeout`.

## Resources

- [Watermill — Pub/Sub implementations](https://watermill.io/pubsubs/) — the catalog of transports behind the same `Publisher`/`Subscriber` interfaces.
- [pkg.go.dev — watermill-redisstream](https://pkg.go.dev/github.com/ThreeDotsLabs/watermill-redisstream/pkg/redisstream) — verified `NewPublisher`, `NewSubscriber`, and their configs.
- [pkg.go.dev — go-redis](https://pkg.go.dev/github.com/redis/go-redis/v9) — `redis.NewClient`, `redis.Options`, `redis.UniversalClient`.
- [Go command — build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — the `//go:build` tag syntax used to select the transport.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-poison-queue-and-retry-topology.md](02-poison-queue-and-retry-topology.md) | Next: [../06-transactional-outbox-pattern/00-concepts.md](../06-transactional-outbox-pattern/00-concepts.md)
