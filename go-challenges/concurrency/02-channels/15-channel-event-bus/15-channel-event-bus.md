# Exercise 15: Channel Event Bus

In an e-commerce platform, placing an order triggers multiple side effects:
inventory decrements stock, notifications send a confirmation email, and
analytics logs the sale. These services should not know about each other -- they
just react to events. Hardcode calls from the order handler to each one and
adding a fourth service means editing order code; a publish-subscribe bus
decouples them instead. The bus is the channel-based equivalent of the
observer pattern, but with built-in concurrency and no callbacks -- and its one
sharp edge is lifecycle: publishing to a bus that has already shut down panics.

## What you'll build

```text
15-channel-event-bus/
  main.go        an EventBus of per-subscriber channels: fan-out on Publish,
                 subscriber-side filtering, and clean shutdown via Close
```

- Build: a publish-subscribe event bus where each subscriber owns its own dedicated buffered channel.
- Implement: `EventBus` with `Subscribe`, `Publish`, and `Close`; independent subscriber goroutines (logger, inventory, notifier, analytics); and subscriber-side filtering by event type.
- Verify: `go run main.go`, and `go run -race main.go` on the clean-shutdown step.

### Why one channel per subscriber

Channels make a natural transport for this pattern. The bus holds a list of
subscriber channels. When an event is published, the bus sends a copy to every
subscriber. Each subscriber goroutine ranges over its own channel, processing
events independently. Because each subscriber has its own buffered channel, a
slow analytics service does not block the notification service. And shutting down
is clean: close every subscriber channel, and all `range` loops exit.

This is the channel-based equivalent of observer/listener patterns in other
languages, but with built-in concurrency and zero callbacks.

## Step 1 -- Bus With One Subscriber

Create an `EventBus` that holds subscriber channels. A single subscriber receives order events and prints them. The bus publishes by iterating over all subscribers and sending the event.

```go
package main

import (
	"fmt"
	"time"
)

const drainDelay = 100 * time.Millisecond

// Event represents something that happened in the system.
type Event struct {
	Type    string
	Payload string
}

// EventBus delivers events to all registered subscribers.
type EventBus struct {
	subscribers []chan Event
}

// NewEventBus creates an empty event bus.
func NewEventBus() *EventBus {
	return &EventBus{}
}

// Subscribe registers a new subscriber and returns a receive-only channel.
// The buffer prevents a slow subscriber from blocking Publish.
func (bus *EventBus) Subscribe(bufferSize int) <-chan Event {
	ch := make(chan Event, bufferSize)
	bus.subscribers = append(bus.subscribers, ch)
	return ch
}

// Publish sends an event to every subscriber channel.
func (bus *EventBus) Publish(event Event) {
	for _, ch := range bus.subscribers {
		ch <- event
	}
}

func main() {
	bus := NewEventBus()

	// One subscriber: a simple logger.
	logCh := bus.Subscribe(10)

	go func() {
		for event := range logCh {
			fmt.Printf("[logger] %s: %s\n", event.Type, event.Payload)
		}
	}()

	bus.Publish(Event{Type: "OrderPlaced", Payload: "order-1001: 2x Widget"})
	bus.Publish(Event{Type: "OrderPlaced", Payload: "order-1002: 1x Gadget"})
	bus.Publish(Event{Type: "OrderPlaced", Payload: "order-1003: 5x Bolt"})

	time.Sleep(drainDelay)
	fmt.Println("Done: 3 events published, 1 subscriber received them")
}
```

Key observations:
- `Subscribe` creates and returns a buffered channel, keeping ownership inside the bus
- `Publish` iterates over all subscriber channels and sends the event to each
- The subscriber goroutine uses `range` to consume events until the channel closes
- A buffer of 10 means the publisher can send up to 10 events before blocking on a slow subscriber

### Verification
```bash
go run main.go
# Expected:
#   [logger] OrderPlaced: order-1001: 2x Widget
#   [logger] OrderPlaced: order-1002: 1x Gadget
#   [logger] OrderPlaced: order-1003: 5x Bolt
#   Done: 3 events published, 1 subscriber received them
```

## Step 2 -- Three Subscribers Processing Differently

Add an inventory service that counts stock decrements, a notification service that formats email summaries, and the logger from Step 1. All three receive the same events and process them independently.

```go
package main

import (
	"fmt"
	"sync"
)

const subscriberBuffer = 10

// Event represents something that happened in the system.
type Event struct {
	Type    string
	Payload string
}

// EventBus delivers events to all registered subscribers.
type EventBus struct {
	subscribers []chan Event
}

func NewEventBus() *EventBus {
	return &EventBus{}
}

func (bus *EventBus) Subscribe(bufferSize int) <-chan Event {
	ch := make(chan Event, bufferSize)
	bus.subscribers = append(bus.subscribers, ch)
	return ch
}

func (bus *EventBus) Publish(event Event) {
	for _, ch := range bus.subscribers {
		ch <- event
	}
}

// Close shuts down the bus by closing all subscriber channels.
func (bus *EventBus) Close() {
	for _, ch := range bus.subscribers {
		close(ch)
	}
}

// runLogger prints every event it receives.
func runLogger(events <-chan Event, wg *sync.WaitGroup) {
	defer wg.Done()
	for event := range events {
		fmt.Printf("[logger]    %s: %s\n", event.Type, event.Payload)
	}
	fmt.Println("[logger]    shut down")
}

// runInventory counts the number of orders to track stock changes.
func runInventory(events <-chan Event, wg *sync.WaitGroup) {
	defer wg.Done()
	decrements := 0
	for range events {
		decrements++
		fmt.Printf("[inventory] stock decremented (total adjustments: %d)\n", decrements)
	}
	fmt.Printf("[inventory] shut down (processed %d adjustments)\n", decrements)
}

// runNotifier formats each event as an email-like summary.
func runNotifier(events <-chan Event, wg *sync.WaitGroup) {
	defer wg.Done()
	for event := range events {
		fmt.Printf("[notifier]  email: \"Your %s is confirmed: %s\"\n", event.Type, event.Payload)
	}
	fmt.Println("[notifier]  shut down")
}

func main() {
	bus := NewEventBus()
	var wg sync.WaitGroup

	logCh := bus.Subscribe(subscriberBuffer)
	invCh := bus.Subscribe(subscriberBuffer)
	notCh := bus.Subscribe(subscriberBuffer)

	wg.Add(3)
	go runLogger(logCh, &wg)
	go runInventory(invCh, &wg)
	go runNotifier(notCh, &wg)

	orders := []Event{
		{Type: "OrderPlaced", Payload: "order-1001: 2x Widget"},
		{Type: "OrderPlaced", Payload: "order-1002: 1x Gadget"},
		{Type: "OrderPlaced", Payload: "order-1003: 5x Bolt"},
	}

	for _, order := range orders {
		bus.Publish(order)
	}

	bus.Close()
	wg.Wait()
	fmt.Println()
	fmt.Println("All subscribers finished processing")
}
```

Each subscriber receives its own copy of every event through its dedicated channel. The inventory service counts, the notifier formats, and the logger prints raw data. They run concurrently and independently.

### Verification
```bash
go run main.go
# Expected:
#   All 3 subscribers process each of the 3 events
#   Each subscriber prints "shut down" after bus.Close()
#   "All subscribers finished processing" at the end
```

## Step 3 -- Typed Events With Subscriber Filtering

In production, not every subscriber cares about every event type. Add `OrderShipped` and `OrderCancelled` events. The inventory service only reacts to `OrderPlaced` and `OrderCancelled`, the notifier reacts to all events, and analytics only counts `OrderShipped`.

```go
package main

import (
	"fmt"
	"sync"
)

const eventBuffer = 20

// Event represents a domain event with a type discriminator.
type Event struct {
	Type    string
	Payload string
}

// Standard event types for the order lifecycle.
const (
	EventOrderPlaced    = "OrderPlaced"
	EventOrderShipped   = "OrderShipped"
	EventOrderCancelled = "OrderCancelled"
)

// EventBus delivers events to all registered subscribers.
type EventBus struct {
	subscribers []chan Event
}

func NewEventBus() *EventBus {
	return &EventBus{}
}

func (bus *EventBus) Subscribe(bufferSize int) <-chan Event {
	ch := make(chan Event, bufferSize)
	bus.subscribers = append(bus.subscribers, ch)
	return ch
}

func (bus *EventBus) Publish(event Event) {
	for _, ch := range bus.subscribers {
		ch <- event
	}
}

func (bus *EventBus) Close() {
	for _, ch := range bus.subscribers {
		close(ch)
	}
}

// runInventoryService adjusts stock only for placed and cancelled orders.
func runInventoryService(events <-chan Event, wg *sync.WaitGroup) {
	defer wg.Done()
	stock := 100
	for event := range events {
		switch event.Type {
		case EventOrderPlaced:
			stock -= 1
			fmt.Printf("[inventory] -1 stock for %s (remaining: %d)\n", event.Payload, stock)
		case EventOrderCancelled:
			stock += 1
			fmt.Printf("[inventory] +1 stock for %s (remaining: %d)\n", event.Payload, stock)
		default:
			// Ignore event types this service does not handle.
		}
	}
	fmt.Printf("[inventory] final stock: %d\n", stock)
}

// runNotificationService sends a message for every event type.
func runNotificationService(events <-chan Event, wg *sync.WaitGroup) {
	defer wg.Done()
	for event := range events {
		switch event.Type {
		case EventOrderPlaced:
			fmt.Printf("[notifier]  email: \"Order confirmed: %s\"\n", event.Payload)
		case EventOrderShipped:
			fmt.Printf("[notifier]  email: \"Your order shipped: %s\"\n", event.Payload)
		case EventOrderCancelled:
			fmt.Printf("[notifier]  email: \"Order cancelled: %s\"\n", event.Payload)
		}
	}
	fmt.Println("[notifier]  done")
}

// runAnalyticsService only counts shipped orders.
func runAnalyticsService(events <-chan Event, wg *sync.WaitGroup) {
	defer wg.Done()
	shipped := 0
	for event := range events {
		if event.Type == EventOrderShipped {
			shipped++
			fmt.Printf("[analytics] shipment #%d recorded: %s\n", shipped, event.Payload)
		}
	}
	fmt.Printf("[analytics] total shipments: %d\n", shipped)
}

func main() {
	bus := NewEventBus()
	var wg sync.WaitGroup

	invCh := bus.Subscribe(eventBuffer)
	notCh := bus.Subscribe(eventBuffer)
	anaCh := bus.Subscribe(eventBuffer)

	wg.Add(3)
	go runInventoryService(invCh, &wg)
	go runNotificationService(notCh, &wg)
	go runAnalyticsService(anaCh, &wg)

	events := []Event{
		{Type: EventOrderPlaced, Payload: "order-1001"},
		{Type: EventOrderPlaced, Payload: "order-1002"},
		{Type: EventOrderShipped, Payload: "order-1001"},
		{Type: EventOrderCancelled, Payload: "order-1002"},
		{Type: EventOrderShipped, Payload: "order-1003"},
	}

	fmt.Println("=== Publishing Events ===")
	for _, event := range events {
		fmt.Printf("  -> %s: %s\n", event.Type, event.Payload)
		bus.Publish(event)
	}

	fmt.Println()
	fmt.Println("=== Shutting Down ===")
	bus.Close()
	wg.Wait()

	fmt.Println()
	fmt.Println("=== Event Processing Complete ===")
}
```

Each subscriber receives all events but filters internally. The inventory service ignores `OrderShipped`, analytics ignores everything except `OrderShipped`, and the notifier handles all three types. This is subscriber-side filtering -- the bus itself remains simple and unaware of event types.

### Verification
```bash
go run main.go
# Expected:
#   inventory processes OrderPlaced (-1) and OrderCancelled (+1), ignores OrderShipped
#   notifier sends an email for all 5 events
#   analytics only counts the 2 OrderShipped events
#   All services print their final state and shut down
```

## Step 4 -- Clean Shutdown: Closing Bus Closes All Subscribers

This step demonstrates the full lifecycle: start the bus, subscribe services, publish events, then shut down cleanly. It also shows what happens if you publish after the bus is closed (panic from sending on a closed channel).

```go
package main

import (
	"fmt"
	"sync"
)

const shutdownBuffer = 20

// Event represents a domain event.
type Event struct {
	Type    string
	Payload string
}

const (
	EventOrderPlaced    = "OrderPlaced"
	EventOrderShipped   = "OrderShipped"
	EventOrderCancelled = "OrderCancelled"
)

// EventBus delivers events to all registered subscribers.
// After Close is called, Publish must not be called.
type EventBus struct {
	subscribers []chan Event
	closed      bool
}

func NewEventBus() *EventBus {
	return &EventBus{}
}

func (bus *EventBus) Subscribe(bufferSize int) <-chan Event {
	if bus.closed {
		panic("cannot subscribe to a closed bus")
	}
	ch := make(chan Event, bufferSize)
	bus.subscribers = append(bus.subscribers, ch)
	return ch
}

func (bus *EventBus) Publish(event Event) {
	if bus.closed {
		panic("cannot publish to a closed bus")
	}
	for _, ch := range bus.subscribers {
		ch <- event
	}
}

// Close shuts down the bus by closing all subscriber channels.
// Subscribers that range over their channel will exit cleanly.
func (bus *EventBus) Close() {
	if bus.closed {
		return
	}
	bus.closed = true
	for _, ch := range bus.subscribers {
		close(ch)
	}
}

// runFilteredService processes events matching allowedTypes and counts the rest.
func runFilteredService(name string, events <-chan Event, allowedTypes map[string]bool, wg *sync.WaitGroup) {
	defer wg.Done()
	received, filtered := 0, 0
	for event := range events {
		received++
		if allowedTypes[event.Type] {
			filtered++
			fmt.Printf("[%-12s] processed %s: %s\n", name, event.Type, event.Payload)
		}
	}
	fmt.Printf("[%-12s] shut down: %d received, %d processed\n",
		name, received, filtered)
}

func main() {
	bus := NewEventBus()
	var wg sync.WaitGroup

	invCh := bus.Subscribe(shutdownBuffer)
	notCh := bus.Subscribe(shutdownBuffer)
	anaCh := bus.Subscribe(shutdownBuffer)

	// Launch subscribers. wg.Add before go -- never inside the goroutine.
	wg.Add(3)
	go runFilteredService("inventory", invCh, map[string]bool{
		EventOrderPlaced:    true,
		EventOrderCancelled: true,
	}, &wg)

	go runFilteredService("notification", notCh, map[string]bool{
		EventOrderPlaced:    true,
		EventOrderShipped:   true,
		EventOrderCancelled: true,
	}, &wg)

	go runFilteredService("analytics", anaCh, map[string]bool{
		EventOrderShipped: true,
	}, &wg)

	events := []Event{
		{Type: EventOrderPlaced, Payload: "order-2001"},
		{Type: EventOrderPlaced, Payload: "order-2002"},
		{Type: EventOrderShipped, Payload: "order-2001"},
		{Type: EventOrderCancelled, Payload: "order-2002"},
		{Type: EventOrderShipped, Payload: "order-2003"},
		{Type: EventOrderPlaced, Payload: "order-2004"},
	}

	fmt.Println("=== Publishing 6 Events ===")
	for _, event := range events {
		bus.Publish(event)
	}

	fmt.Println()
	fmt.Println("=== Closing Bus ===")
	bus.Close()
	wg.Wait()

	fmt.Println()
	fmt.Println("=== Clean Shutdown Complete ===")
	fmt.Println("All subscriber goroutines have exited.")
	fmt.Println("No goroutine leaks, no blocked channels.")

	// Demonstrate that publishing after close panics.
	fmt.Println()
	fmt.Println("=== Demonstrate Post-Close Safety ===")
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("Caught panic: %v\n", r)
				fmt.Println("This is correct: publishing to a closed bus is a programming error.")
			}
		}()
		bus.Publish(Event{Type: "OrderPlaced", Payload: "should-fail"})
	}()
}
```

The shutdown sequence:
1. Stop publishing (no more calls to `Publish`)
2. Call `bus.Close()` which closes every subscriber channel
3. Each subscriber's `range` loop exits, the goroutine finishes
4. `wg.Wait()` returns when all subscribers have exited
5. No goroutine leaks, no dangling channels

### Verification
```bash
go run -race main.go
# Expected:
#   6 events published to 3 subscribers
#   Each subscriber prints its processed count on shutdown
#   Post-close Publish panics with "cannot publish to a closed bus"
#   Clean exit, no race warnings
```

## Common Mistakes

### Publishing After Close

**Wrong:**
```go
bus.Close()
bus.Publish(Event{Type: "OrderPlaced", Payload: "too-late"})
// panic: send on closed channel
```

**What happens:** `Close` already closed all subscriber channels. `Publish` tries to send on a closed channel, which panics.

**Fix:** Ensure all publishing is complete before calling `Close`. Use a `closed` flag to catch this at the bus level with a clear error message, as shown in Step 4.

### Subscriber Closes Its Own Channel

**Wrong:**
```go
go func() {
    for event := range events {
        process(event)
    }
    close(events) // subscriber does not own this channel!
}()
```

**What happens:** The bus also tries to close the channel during shutdown -- double close panics.

**Fix:** Only the bus closes subscriber channels. Subscribers range over them and exit when the channel closes.

### Unbuffered Subscriber Channels

**Wrong:**
```go
func (bus *EventBus) Subscribe() <-chan Event {
    ch := make(chan Event) // unbuffered!
    bus.subscribers = append(bus.subscribers, ch)
    return ch
}
```

**What happens:** `Publish` sends to each subscriber sequentially. A slow subscriber blocks the send, which delays delivery to all other subscribers. One slow service stalls the entire bus.

**Fix:** Use buffered channels. The buffer size should match the expected burst rate. If a subscriber falls behind by more than the buffer, the publisher blocks -- that is backpressure, not a bug.

## Review

The bus is a slice of subscriber channels and nothing more: `Publish` walks the
slice and sends each event to every channel, so fan-out is automatic -- one call
reaches N subscribers. `Subscribe` creates and returns a buffered channel that
the bus owns and the subscriber ranges over, which is the ownership rule that
makes shutdown clean: `Close` closes every subscriber channel, each `range` loop
ends, and no goroutine leaks. The buffer is what decouples a fast publisher from
a slow subscriber; when a subscriber falls further behind than its buffer, the
publisher blocks, and that backpressure is a feature, not a bug. Filtering lives
on the subscriber side -- each service ignores the event types it does not care
about -- so the bus stays simple and unaware of any event taxonomy. The lifecycle
is the one thing you must respect: publishing after `Close` sends on a closed
channel and panics, which is why Step 4 guards it with a `closed` flag.

Make sure you can answer why each subscriber gets its own channel instead of all
sharing one, what happens when a subscriber's buffer fills during `Publish`, how
`bus.Close()` causes every subscriber goroutine to exit, and why subscriber-side
filtering is simpler than teaching the bus about event types.

## Resources

- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the design principle behind moving events through channels rather than shared state.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- idiomatic buffered-channel and range-over-channel patterns the bus is built from.
- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- fan-out and fan-in slides that generalize this pub-sub structure.

---

Back to [Concurrency](../../concurrency.md) | Next: [16-channel-state-machine](../16-channel-state-machine/16-channel-state-machine.md)
