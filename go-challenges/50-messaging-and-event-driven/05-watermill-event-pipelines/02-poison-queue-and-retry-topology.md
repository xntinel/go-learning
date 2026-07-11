# Exercise 2: Dead-Letter Routing with the Poison Queue Middleware

A bounded `Retry` followed by a `PoisonQueue` builds a retry-then-dead-letter
topology: a message that exhausts its retries is published to a dedicated
dead-letter topic with diagnostic metadata instead of being redelivered forever.
This exercise wires that topology, adds a consumer on the poison topic that
records the diagnostics, and then uses `PoisonQueueWithFilter` to send permanent
errors straight to the DLQ while transient ones are still retried.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
dlq/                         independent module: example.com/dlq
  go.mod                     go 1.26; requires watermill v1.5.2
  dlq.go                     BuildDLQRouter, BuildFilteredRouter, sentinels, metadata helpers
  cmd/
    demo/
      main.go                good message processed; poison message dead-lettered
  dlq_test.go                exhausted-retries -> DLQ metadata; permanent bypasses retry
```

- Files: `dlq.go`, `cmd/demo/main.go`, `dlq_test.go`.
- Implement: `BuildDLQRouter` (per-handler stack `PoisonQueue` over `Retry`) and `BuildFilteredRouter` (`Retry` over `PoisonQueueWithFilter`) plus the `ErrPermanent`/`ErrTransient` sentinels.
- Test: an always-failing handler lands its message in the DLQ with `reason_poisoned`/`handler_poisoned` populated; a good message never reaches the DLQ; a permanent error is dead-lettered on the first attempt while a transient one is retried until it succeeds.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dlq/cmd/demo
cd ~/go-exercises/dlq
go mod init example.com/dlq
go mod edit -go=1.26
go get github.com/ThreeDotsLabs/watermill@v1.5.2
```

### Why PoisonQueue must sit outside Retry

`middleware.PoisonQueue(pub, topic)` returns `(message.HandlerMiddleware,
error)` — note the error, which is `ErrInvalidPoisonQueueTopic` if the topic is
empty, and which you must not drop. When its wrapped handler returns an error, the
poison middleware publishes the message to `topic` with four metadata keys —
`reason_poisoned`, `topic_poisoned`, `handler_poisoned`, `subscriber_poisoned` —
and then returns `nil`, acking the input so it is not redelivered.

That swallow-and-ack behavior is exactly why the ordering matters. This exercise
attaches the stack per handler with `Handler.AddMiddleware`, applied
outermost-first, so `[poison, retry]` becomes `poison(retry(handler))`. `Retry`
runs the handler and its backoff loop to exhaustion; only the final error escapes
to `poison`, which dead-letters it. If you reversed the two — `retry(poison(...))`
— `poison` would swallow the first failure and return `nil`, `Retry` would see
success, and every failure would hit the DLQ on attempt one with no retries at
all.

### Classifying permanent versus transient failures

The default `PoisonQueue` dead-letters any error once retries are exhausted.
`middleware.PoisonQueueWithFilter(pub, topic, shouldGoToPoisonQueue)` adds a
predicate over the error: return true to dead-letter it, false to propagate it so
the surrounding `Retry` (or the transport) can retry. `BuildFilteredRouter` puts
the filter *inside* `Retry` — the stack `[retry, filter]` becomes
`retry(filter(handler))` — with the predicate returning true for permanent errors.
Now a permanent error is dead-lettered by the filter on the first call and returns
`nil`, so `Retry` never loops; a transient error is propagated, so `Retry` retries
it until the handler succeeds. The tests prove the distinction by counting how
many times the handler ran for each kind.

Create `dlq.go`:

```go
package dlq

import (
	"errors"
	"fmt"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
)

// Topics for the incoming work and the dead-letter destination.
const (
	InputTopic = "tasks.incoming"
	DLQTopic   = "tasks.dead"
)

// Sentinels used to classify handler failures.
var (
	// ErrPermanent marks a failure that will never succeed on retry.
	ErrPermanent = errors.New("permanent failure")
	// ErrTransient marks a failure that may succeed if retried.
	ErrTransient = errors.New("transient failure")
)

// BuildDLQRouter wires worker onto a router with a per-handler stack of
// PoisonQueue outside Retry: the message is retried up to MaxRetries and, once
// exhausted, dead-lettered to DLQTopic with diagnostic metadata.
func BuildDLQRouter(
	pub message.Publisher,
	sub message.Subscriber,
	logger watermill.LoggerAdapter,
	worker message.NoPublishHandlerFunc,
) (*message.Router, error) {
	router, err := message.NewRouter(message.RouterConfig{CloseTimeout: 5 * time.Second}, logger)
	if err != nil {
		return nil, fmt.Errorf("new router: %w", err)
	}

	poison, err := middleware.PoisonQueue(pub, DLQTopic)
	if err != nil {
		return nil, fmt.Errorf("poison queue: %w", err)
	}

	h := router.AddNoPublisherHandler("worker", InputTopic, sub, worker)
	h.AddMiddleware(
		// Outermost: only sees the error after retries are exhausted.
		poison,
		// Innermost: retries the handler with backoff.
		middleware.Retry{
			MaxRetries:      2,
			InitialInterval: time.Millisecond,
			Logger:          logger,
		}.Middleware,
	)
	return router, nil
}

// BuildFilteredRouter wires worker with Retry outside a PoisonQueueWithFilter
// whose predicate dead-letters permanent errors immediately and lets transient
// errors fall through to be retried.
func BuildFilteredRouter(
	pub message.Publisher,
	sub message.Subscriber,
	logger watermill.LoggerAdapter,
	worker message.NoPublishHandlerFunc,
) (*message.Router, error) {
	router, err := message.NewRouter(message.RouterConfig{CloseTimeout: 5 * time.Second}, logger)
	if err != nil {
		return nil, fmt.Errorf("new router: %w", err)
	}

	poison, err := middleware.PoisonQueueWithFilter(pub, DLQTopic, func(err error) bool {
		return errors.Is(err, ErrPermanent)
	})
	if err != nil {
		return nil, fmt.Errorf("poison queue with filter: %w", err)
	}

	h := router.AddNoPublisherHandler("worker", InputTopic, sub, worker)
	h.AddMiddleware(
		// Outermost: retries whatever the filter propagates.
		middleware.Retry{
			MaxRetries:      3,
			InitialInterval: time.Millisecond,
			Logger:          logger,
		}.Middleware,
		// Innermost: dead-letters permanent errors, propagates transient ones.
		poison,
	)
	return router, nil
}

// PoisonReason reads the failure reason that a PoisonQueue set on a dead-lettered
// message.
func PoisonReason(msg *message.Message) string {
	return msg.Metadata.Get(middleware.ReasonForPoisonedKey)
}

// PoisonHandler reads the name of the handler that produced a dead-lettered
// message.
func PoisonHandler(msg *message.Message) string {
	return msg.Metadata.Get(middleware.PoisonedHandlerKey)
}
```

### The runnable demo

The demo registers the retry-then-DLQ topology plus a second consumer on the
poison topic that prints the diagnostics. It publishes one good task and one that
always fails; the good one is processed, and the failing one is retried, exhausts,
and is dead-lettered, at which point the poison consumer reports why.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"

	"example.com/dlq"
)

type task struct {
	ID   string `json:"id"`
	Fail bool   `json:"fail"`
}

func main() {
	logger := watermill.NopLogger{}
	pubSub := gochannel.NewGoChannel(gochannel.Config{}, logger)

	processed := make(chan string, 1)
	dead := make(chan string, 1)

	worker := func(msg *message.Message) error {
		var t task
		if err := json.Unmarshal(msg.Payload, &t); err != nil {
			return err
		}
		if t.Fail {
			return fmt.Errorf("cannot process order %s", t.ID)
		}
		processed <- t.ID
		return nil
	}

	router, err := dlq.BuildDLQRouter(pubSub, pubSub, logger, worker)
	if err != nil {
		log.Fatal(err)
	}

	router.AddNoPublisherHandler("dlq-audit", dlq.DLQTopic, pubSub, func(msg *message.Message) error {
		var t task
		_ = json.Unmarshal(msg.Payload, &t)
		dead <- fmt.Sprintf("dead-lettered: %s reason=%q handler=%s",
			t.ID, dlq.PoisonReason(msg), dlq.PoisonHandler(msg))
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- router.Run(ctx) }()
	<-router.Running()

	good, _ := json.Marshal(task{ID: "good-1", Fail: false})
	bad, _ := json.Marshal(task{ID: "bad-1", Fail: true})
	if err := pubSub.Publish(dlq.InputTopic, message.NewMessage(watermill.NewUUID(), good)); err != nil {
		log.Fatal(err)
	}
	if err := pubSub.Publish(dlq.InputTopic, message.NewMessage(watermill.NewUUID(), bad)); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("processed: %s\n", <-processed)
	fmt.Println(<-dead)

	cancel()
	if err := <-runErr; err != nil {
		log.Fatal(err)
	}
	_ = pubSub.Close()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed: good-1
dead-lettered: bad-1 reason="cannot process order bad-1" handler=worker
```

### Tests

The first test wires the retry-then-DLQ router with an always-failing handler,
subscribes to the DLQ topic, and asserts the message arrives there with
`reason_poisoned` and `handler_poisoned` set — and that a good message, sent
through a separate router with a succeeding handler, never reaches the DLQ. The
second test uses the filtered router: a permanent-error message must be
dead-lettered after exactly one handler call, while a transient-error message is
retried and eventually succeeds, so it is called three times and never
dead-lettered.

Create `dlq_test.go`:

```go
package dlq

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
)

func startRouter(t *testing.T, router *message.Router) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- router.Run(ctx) }()
	<-router.Running()
	t.Cleanup(func() {
		cancel()
		if err := <-runErr; err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	})
}

func TestExhaustedRetriesGoToDLQ(t *testing.T) {
	t.Parallel()
	logger := watermill.NopLogger{}
	pubSub := gochannel.NewGoChannel(gochannel.Config{}, logger)

	worker := func(msg *message.Message) error {
		return errors.New("boom")
	}
	router, err := BuildDLQRouter(pubSub, pubSub, logger, worker)
	if err != nil {
		t.Fatalf("BuildDLQRouter: %v", err)
	}
	startRouter(t, router)

	dlqCh, err := pubSub.Subscribe(context.Background(), DLQTopic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := pubSub.Publish(InputTopic, message.NewMessage("m1", []byte("payload"))); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case dead := <-dlqCh:
		dead.Ack()
		if reason := PoisonReason(dead); reason == "" {
			t.Fatal("reason_poisoned metadata is empty")
		}
		if h := PoisonHandler(dead); h != "worker" {
			t.Fatalf("handler_poisoned = %q; want worker", h)
		}
		if got := dead.Metadata.Get(middleware.PoisonedTopicKey); got != InputTopic {
			t.Fatalf("topic_poisoned = %q; want %q", got, InputTopic)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dead-lettered message")
	}
}

func TestGoodMessageNeverDeadLettered(t *testing.T) {
	t.Parallel()
	logger := watermill.NopLogger{}
	pubSub := gochannel.NewGoChannel(gochannel.Config{}, logger)

	done := make(chan struct{})
	worker := func(msg *message.Message) error {
		close(done)
		return nil
	}
	router, err := BuildDLQRouter(pubSub, pubSub, logger, worker)
	if err != nil {
		t.Fatalf("BuildDLQRouter: %v", err)
	}
	startRouter(t, router)

	dlqCh, err := pubSub.Subscribe(context.Background(), DLQTopic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := pubSub.Publish(InputTopic, message.NewMessage("ok", []byte("payload"))); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never ran")
	}

	select {
	case <-dlqCh:
		t.Fatal("good message reached the DLQ")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestFilterPermanentBypassesRetry(t *testing.T) {
	t.Parallel()
	logger := watermill.NopLogger{}
	pubSub := gochannel.NewGoChannel(gochannel.Config{}, logger)

	var permAttempts, transAttempts atomic.Int64
	transDone := make(chan struct{})
	worker := func(msg *message.Message) error {
		switch msg.Metadata.Get("kind") {
		case "permanent":
			permAttempts.Add(1)
			return fmt.Errorf("bad payload: %w", ErrPermanent)
		case "transient":
			if transAttempts.Add(1) < 3 {
				return fmt.Errorf("blip: %w", ErrTransient)
			}
			close(transDone)
			return nil
		}
		return nil
	}

	router, err := BuildFilteredRouter(pubSub, pubSub, logger, worker)
	if err != nil {
		t.Fatalf("BuildFilteredRouter: %v", err)
	}
	startRouter(t, router)

	dlqCh, err := pubSub.Subscribe(context.Background(), DLQTopic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	permMsg := message.NewMessage("perm", []byte("x"))
	permMsg.Metadata.Set("kind", "permanent")
	transMsg := message.NewMessage("trans", []byte("y"))
	transMsg.Metadata.Set("kind", "transient")
	if err := pubSub.Publish(InputTopic, permMsg); err != nil {
		t.Fatalf("publish perm: %v", err)
	}
	if err := pubSub.Publish(InputTopic, transMsg); err != nil {
		t.Fatalf("publish trans: %v", err)
	}

	select {
	case dead := <-dlqCh:
		dead.Ack()
		if kind := dead.Metadata.Get("kind"); kind != "permanent" {
			t.Fatalf("dead-lettered kind = %q; want permanent", kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("permanent message never dead-lettered")
	}

	select {
	case <-transDone:
	case <-time.After(2 * time.Second):
		t.Fatal("transient message never succeeded")
	}

	if got := permAttempts.Load(); got != 1 {
		t.Fatalf("permanent attempts = %d; want 1 (bypassed retry)", got)
	}
	if got := transAttempts.Load(); got != 3 {
		t.Fatalf("transient attempts = %d; want 3 (retried to success)", got)
	}
}
```

## Review

The topology is correct when an exhausted message carries the four poison metadata
keys and a healthy message never touches the DLQ. If failing messages hit the DLQ
without ever retrying, the middleware order is inverted — `PoisonQueue` must be
outside `Retry` so the retry loop finishes before the poison layer sees the error.
If the filter test dead-letters the transient message, the predicate is matching
the wrong sentinel: it must return true only for `ErrPermanent`, tested with
`errors.Is`, so transient errors are propagated back to `Retry`. Remember that
`PoisonQueue` and `PoisonQueueWithFilter` return an error you must check — an empty
topic yields `middleware.ErrInvalidPoisonQueueTopic`, and dropping it hides the
misconfiguration until messages silently disappear. The attempt counters, read
with atomics, are the deterministic proof that permanent errors bypass the loop
while transient ones traverse it.

## Resources

- [Watermill — Middlewares: Poison Queue](https://watermill.io/docs/middlewares/#poisonqueue) — the poison middleware, its metadata keys, and the filter variant.
- [pkg.go.dev — router/middleware](https://pkg.go.dev/github.com/ThreeDotsLabs/watermill/message/router/middleware) — verified `PoisonQueue`, `PoisonQueueWithFilter`, `ReasonForPoisonedKey` and related constants.
- [Watermill — Message Router](https://watermill.io/docs/messages-router/) — `AddNoPublisherHandler` and per-handler `AddMiddleware`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-router-middleware-pipeline.md](01-router-middleware-pipeline.md) | Next: [03-transport-portable-pipeline.md](03-transport-portable-pipeline.md)
