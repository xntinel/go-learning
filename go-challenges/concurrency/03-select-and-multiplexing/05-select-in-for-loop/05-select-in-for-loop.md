# Exercise 5: Select in For Loop

A single `select` handles one event and returns. But a message broker consumer
needs to run continuously: pull messages off a subscription, fire periodic
cleanup on a ticker, and honor a shutdown signal -- all at once, indefinitely.
The `for` + `select` loop is Go's answer to that. It also carries two hazards
worth naming up front: every such loop must have a way to terminate or it leaks
its goroutine, and a channel that has closed will, unless you neutralize it,
make the `select` spin on zero values forever.

## What you'll build

```text
05-select-in-for-loop/
  main.go        a broker consumer loop, multi-source multiplexing, a ticker
                 for cleanup, the nil-channel trick, and a labeled break
```

- Build: a message broker consumer loop that multiplexes a subscription, alerts, a ticker, and a shutdown signal.
- Implement: a `for`+`select` event loop, a quit/shutdown channel, the nil-channel trick for sources that close, and a labeled break to exit the loop.
- Verify: `go run main.go`.

### Why the event loop needs a quit channel and the nil trick

The `for` + `select` combination is the standard Go event loop. It is the idiomatic way to write a goroutine that reacts to multiple channels over its entire lifetime. Nearly every long-running goroutine in production Go code follows this pattern: HTTP servers, queue consumers, connection managers, background workers.

The quit channel is the clean shutdown mechanism. Instead of killing a goroutine externally (which Go intentionally does not support), you signal on a channel that the goroutine checks in its `select`. This gives the goroutine a chance to clean up resources before exiting. This pattern is so common that it was formalized into `context.Context`, which you will learn in a later section.

## Step 1 -- Basic Message Consumer

Build a message broker consumer that receives messages from a subscription channel and shuts down cleanly when signaled.

```go
package main

import (
	"fmt"
	"time"
)

const messageInterval = 50 * time.Millisecond

type MessageBroker struct {
	messages chan string
	shutdown chan struct{}
}

func NewMessageBroker() *MessageBroker {
	return &MessageBroker{
		messages: make(chan string),
		shutdown: make(chan struct{}),
	}
}

func (mb *MessageBroker) ProduceMessages(topics []string) {
	go func() {
		for _, topic := range topics {
			mb.messages <- fmt.Sprintf("[%s] payload={...}", topic)
			time.Sleep(messageInterval)
		}
		close(mb.shutdown)
	}()
}

func (mb *MessageBroker) StartConsumer() {
	go func() {
		for {
			select {
			case msg := <-mb.messages:
				fmt.Println("consumed:", msg)
			case <-mb.shutdown:
				fmt.Println("consumer: shutting down")
				return
			}
		}
	}()
}

func main() {
	broker := NewMessageBroker()

	topics := []string{
		"order.created",
		"user.signup",
		"payment.processed",
		"order.shipped",
		"inventory.updated",
	}

	broker.ProduceMessages(topics)
	broker.StartConsumer()

	// Wait for the producer to finish and the consumer to stop.
	time.Sleep(500 * time.Millisecond)
}
```

The consumer loops forever, processing messages as they arrive. When `shutdown` is closed, the `<-shutdown` case succeeds (closed channels return the zero value immediately), and the consumer returns.

### Verification
```
consumed: [order.created] payload={...}
consumed: [user.signup] payload={...}
consumed: [payment.processed] payload={...}
consumed: [order.shipped] payload={...}
consumed: [inventory.updated] payload={...}
consumer: shutting down
```

## Step 2 -- Consumer with Multiple Event Sources

Extend the consumer to handle messages from a subscription, alerts from a monitoring channel, and a shutdown signal. This is the canonical event loop: one goroutine, multiple concerns.

```go
package main

import (
	"fmt"
	"time"
)

const (
	messageCount    = 5
	alertCount      = 3
	messageInterval = 30 * time.Millisecond
	alertInterval   = 50 * time.Millisecond
	shutdownDelay   = 300 * time.Millisecond
	channelBuffer   = 5
)

func produceMessages(subscription chan<- string, count int, interval time.Duration) {
	go func() {
		for i := 0; i < count; i++ {
			subscription <- fmt.Sprintf("msg-%d", i)
			time.Sleep(interval)
		}
	}()
}

func produceAlerts(alerts chan<- string, count int, interval time.Duration) {
	go func() {
		for i := 0; i < count; i++ {
			alerts <- fmt.Sprintf("alert: consumer-lag-%d", i)
			time.Sleep(interval)
		}
	}()
}

func scheduleShutdown(shutdown chan struct{}, delay time.Duration) {
	go func() {
		time.Sleep(delay)
		close(shutdown)
	}()
}

func runEventLoop(subscription, alerts <-chan string, shutdown <-chan struct{}) {
	for {
		select {
		case msg := <-subscription:
			fmt.Println("[MSG]", msg)
		case alert := <-alerts:
			fmt.Println("[ALERT]", alert)
		case <-shutdown:
			fmt.Println("event loop stopped")
			return
		}
	}
}

func main() {
	subscription := make(chan string, channelBuffer)
	alerts := make(chan string, channelBuffer)
	shutdown := make(chan struct{})

	produceMessages(subscription, messageCount, messageInterval)
	produceAlerts(alerts, alertCount, alertInterval)
	scheduleShutdown(shutdown, shutdownDelay)

	runEventLoop(subscription, alerts, shutdown)
}
```

A single `select` cleanly multiplexes two event streams plus a shutdown signal. Adding a new event source is as simple as adding a new case.

### Verification
```
[MSG] msg-0
[ALERT] alert: consumer-lag-0
[MSG] msg-1
[MSG] msg-2
[ALERT] alert: consumer-lag-1
[MSG] msg-3
[ALERT] alert: consumer-lag-2
[MSG] msg-4
event loop stopped
```

## Step 3 -- Periodic Cleanup with a Ticker

Add a `time.Ticker` for periodic maintenance tasks: flushing commit offsets, cleaning up expired sessions, or logging consumer lag. This is the canonical Go service event loop.

```go
package main

import (
	"fmt"
	"time"
)

const (
	eventCount      = 8
	eventInterval   = 40 * time.Millisecond
	cleanupInterval = 100 * time.Millisecond
	channelBuffer   = 10
)

func produceEvents(messages chan<- string, shutdown chan struct{}, count int) {
	go func() {
		for i := 0; i < count; i++ {
			messages <- fmt.Sprintf("event-%d", i)
			time.Sleep(eventInterval)
		}
		close(shutdown)
	}()
}

type EventConsumer struct {
	messages chan string
	shutdown chan struct{}
	consumed int
}

func NewEventConsumer(bufferSize int) *EventConsumer {
	return &EventConsumer{
		messages: make(chan string, bufferSize),
		shutdown: make(chan struct{}),
	}
}

func (ec *EventConsumer) Run() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

loop:
	for {
		select {
		case msg := <-ec.messages:
			fmt.Println("[consume]", msg)
			ec.consumed++
		case <-ticker.C:
			fmt.Printf("[cleanup] offset commit, %d messages processed\n", ec.consumed)
		case <-ec.shutdown:
			fmt.Printf("[shutdown] total: %d messages consumed\n", ec.consumed)
			break loop
		}
	}
}

func main() {
	consumer := NewEventConsumer(channelBuffer)
	produceEvents(consumer.messages, consumer.shutdown, eventCount)
	consumer.Run()
}
```

### Verification
```
[consume] event-0
[consume] event-1
[cleanup] offset commit, 2 messages processed
[consume] event-2
[consume] event-3
[cleanup] offset commit, 4 messages processed
[consume] event-4
[consume] event-5
[cleanup] offset commit, 6 messages processed
[consume] event-6
[consume] event-7
[shutdown] total: 8 messages consumed
```

## Step 4 -- Nil Channel Trick for Sources That Close

When a producer closes its channel, a `select` case on that channel returns the zero value instantly, forever. Set closed channels to `nil` to exclude them from the `select`. This is essential when consuming from multiple sources that finish at different times.

```go
package main

import (
	"fmt"
	"time"
)

func produceTopicEvents(name string, count int, interval time.Duration) <-chan string {
	topic := make(chan string)
	go func() {
		for i := 0; i < count; i++ {
			topic <- fmt.Sprintf("%s: event-%d", name, i)
			time.Sleep(interval)
		}
		close(topic)
	}()
	return topic
}

func consumeUntilAllClosed(topicA, topicB <-chan string) {
	topicADone, topicBDone := false, false

	for {
		select {
		case msg, ok := <-topicA:
			if !ok {
				topicA = nil // Nil channel is never selected.
				topicADone = true
				fmt.Println("topic-a: subscription ended")
			} else {
				fmt.Println("received:", msg)
			}
		case msg, ok := <-topicB:
			if !ok {
				topicB = nil
				topicBDone = true
				fmt.Println("topic-b: subscription ended")
			} else {
				fmt.Println("received:", msg)
			}
		}

		if topicADone && topicBDone {
			fmt.Println("all subscriptions ended, consumer exiting")
			return
		}
	}
}

func main() {
	topicA := produceTopicEvents("topic-a", 3, 50*time.Millisecond)
	topicB := produceTopicEvents("topic-b", 5, 30*time.Millisecond)

	consumeUntilAllClosed(topicA, topicB)
}
```

Key technique: setting a channel to `nil` after it closes. A `nil` channel in a `select` case is never ready, so the runtime skips it. Without this, the closed channel returns zero values in a tight loop forever.

### Verification
```
received: topic-b: event-0
received: topic-a: event-0
received: topic-b: event-1
received: topic-a: event-1
received: topic-b: event-2
received: topic-b: event-3
received: topic-a: event-2
topic-a: subscription ended
received: topic-b: event-4
topic-b: subscription ended
all subscriptions ended, consumer exiting
```

## Step 5 -- Labeled Break to Exit For-Select

A bare `break` inside `select` breaks the select, NOT the for loop. Use a labeled break or `return` to exit the loop. This is a frequent source of bugs.

```go
package main

import (
	"fmt"
	"time"
)

const messageBuffer = 5

func produceOrderEvents(messages chan<- string, done chan struct{}) {
	go func() {
		events := []string{"order.created", "order.paid", "order.shipped"}
		for _, event := range events {
			messages <- event
			time.Sleep(30 * time.Millisecond)
		}
		close(done)
	}()
}

func consumeUntilDone(messages <-chan string, done <-chan struct{}) {
loop:
	for {
		select {
		case msg := <-messages:
			fmt.Println("processed:", msg)
		case <-done:
			fmt.Println("done signal received")
			break loop // Exits the for loop, not just the select.
		}
	}
	fmt.Println("consumer cleanup complete")
}

func main() {
	messages := make(chan string, messageBuffer)
	done := make(chan struct{})

	produceOrderEvents(messages, done)
	consumeUntilDone(messages, done)
}
```

### Verification
```
processed: order.created
processed: order.paid
processed: order.shipped
done signal received
consumer cleanup complete
```

## Common Mistakes

### 1. Not Setting Closed Channels to Nil
A closed channel returns the zero value immediately, forever. Without setting it to `nil`, the `select` spins on the closed channel case:

```go
// BAD: after ch closes, this prints "" forever.
for {
    select {
    case msg := <-ch: // ch is closed — returns "" every iteration
        fmt.Println(msg)
    }
}
```

### 2. Breaking Out of Select vs. For Loop
A `break` inside a `select` breaks out of the `select`, not the enclosing `for` loop. Use `return`, a labeled break (`break loop`), or a flag variable to exit the loop.

### 3. Goroutine Leak: Forgetting the Shutdown Channel
If the for-select loop has no exit condition, the goroutine runs forever. Every for-select must have a way to terminate: a shutdown channel, context cancellation, or detection of all sources closing.

### 4. Sending on a Closed Channel
Closing a channel signals all receivers, but sending on a closed channel panics. The producer closes, the consumer detects.

## Review

The `for` + `select` combination is Go's event loop idiom: one goroutine that
loops forever and, on each pass, blocks in a `select` that multiplexes every
channel it cares about -- a subscription, an alert stream, a ticker for periodic
maintenance, and a shutdown signal. Adding a concern is just adding a case. The
asymmetry to internalize is that `break` inside a `select` breaks the `select`,
not the loop, so exiting takes a `return`, a labeled `break loop`, or a flag.
And a closed channel is not inert: its `select` case fires immediately and
forever, returning the zero value, so the moment a source closes you set its
variable to `nil` -- a nil channel is never ready, and the runtime skips it --
which is what lets a single loop drain several sources that finish at different
times.

You should be able to answer the loop's four standing questions without
rereading: why a bare `break` leaves you stuck in the loop, why the nil-channel
trick is necessary and what it costs (nothing -- a nil case is simply never
selected), the three ways a for-select can terminate (a shutdown or done
channel, context cancellation, or detecting that all sources have closed), and
how a `time.Ticker` case slots in beside the message cases to run cleanup on a
fixed interval. Above all, remember that a for-select with no exit path is a
goroutine leak with a heartbeat -- every one of these loops needs a way out.

## Resources
- [Go Spec: Select statements](https://go.dev/ref/spec#Select_statements) -- how select chooses among ready cases and why a nil channel is never one.
- [Go Spec: Receive operator](https://go.dev/ref/spec#Receive_operator) -- the two-value receive `v, ok := <-ch` that detects a closed channel.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- channel idioms including closing and the producer/consumer split.

---

Back to [Concurrency](../../concurrency.md) | Next: [06-done-channel-pattern](../06-done-channel-pattern/06-done-channel-pattern.md)
