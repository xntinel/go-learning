# Exercise 8: Multiplexing N Sources

A `select` needs its cases written out at compile time, so it cannot merge a
number of channels you only learn at runtime -- N application instances, a
variable set of sensor feeds, several API streams. The fan-in pattern solves
this: spawn one goroutine per source, each forwarding its values into a single
shared output channel, and let a `sync.WaitGroup` decide when the last source
has drained so the output can be closed exactly once. Get the closing wrong --
close from a forwarder, or never close at all -- and you get a panic or a leaked
goroutine. This exercise builds the merge four ways, adding generalization,
staggered completion, cancellation, and source tagging.

## What you'll build

```text
08-multiplexing-n-sources/
  main.go        two-stream merge, variadic mergeN, staggered-source handling,
                 a cancelable merge, and a source-tagged aggregator
```

- Build: a channel multiplexer that fans N source channels into one output, growing from two fixed streams to a cancelable, source-tagged aggregator.
- Implement: `mergeStreams`, the variadic `mergeN`, `mergeWithCancellation` with a done channel, and `mergeTaggedStreams` over a `TaggedEvent` struct.
- Verify: `go run main.go` on each step.

### Why fan-in beats a variadic select

You cannot write a `select` with a variable number of cases -- Go's `select`
requires cases to be lexically present at compile time. The fan-in pattern
sidesteps that: spawn one goroutine per source channel, each forwarding its
values to a single shared output channel. A `sync.WaitGroup` tracks when all
source goroutines have finished, and only then is the output channel closed.

This is the general-purpose channel multiplexer. It appears in Go's standard
patterns, in the `x/sync/errgroup` package, and in virtually every
pipeline-based architecture. Mastering it gives you the ability to compose
arbitrary channel topologies.

## Step 1 -- Merge Two Log Streams

Start with the simplest case: merge log events from two application instances into a single ordered stream.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type LogEntry = string

func mergeStreams(stream1, stream2 <-chan LogEntry) <-chan LogEntry {
	merged := make(chan LogEntry)
	var wg sync.WaitGroup

	forwardEvents := func(source <-chan LogEntry) {
		defer wg.Done()
		for event := range source {
			merged <- event
		}
	}

	wg.Add(2)
	go forwardEvents(stream1)
	go forwardEvents(stream2)

	go func() {
		wg.Wait()
		close(merged)
	}()

	return merged
}

func produceLogEntries(entries []LogEntry, interval time.Duration) <-chan LogEntry {
	stream := make(chan LogEntry)
	go func() {
		for _, entry := range entries {
			stream <- entry
			time.Sleep(interval)
		}
		close(stream)
	}()
	return stream
}

func main() {
	app1Logs := produceLogEntries(
		[]LogEntry{"app1: request received", "app1: db query 42ms", "app1: response 200"},
		30*time.Millisecond,
	)
	app2Logs := produceLogEntries(
		[]LogEntry{"app2: request received", "app2: cache hit", "app2: response 200", "app2: request received", "app2: response 500"},
		20*time.Millisecond,
	)

	for event := range mergeStreams(app1Logs, app2Logs) {
		fmt.Println(event)
	}
	fmt.Println("all log streams closed")
}
```

Each source gets its own goroutine that forwards events to `out`. When a source closes, `range` exits and `wg.Done()` is called. After all sources finish, the WaitGroup goroutine closes `out`, which terminates the consumer's `range`.

### Verification
```
app1: request received
app2: request received
app2: cache hit
app1: db query 42ms
app2: response 200
app2: request received
app1: response 200
app2: response 500
all log streams closed
```
The exact interleaving varies, but all 8 events appear and both streams are fully consumed.

## Step 2 -- Generalize to N Streams

Replace the two-stream merge with a variadic version that accepts any number of channels. This is what you need when the number of application instances is only known at runtime.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	instanceCount    = 4
	eventsPerInstance = 3
	baseInterval     = 20 * time.Millisecond
)

func mergeN(streams ...<-chan string) <-chan string {
	merged := make(chan string)
	var wg sync.WaitGroup

	forwardEvents := func(source <-chan string) {
		defer wg.Done()
		for event := range source {
			merged <- event
		}
	}

	wg.Add(len(streams))
	for _, source := range streams {
		go forwardEvents(source)
	}

	go func() {
		wg.Wait()
		close(merged)
	}()

	return merged
}

func createInstanceStream(instanceID int, eventCount int, interval time.Duration) <-chan string {
	stream := make(chan string)
	go func() {
		for eventSeq := 0; eventSeq < eventCount; eventSeq++ {
			stream <- fmt.Sprintf("instance-%d: event-%d", instanceID, eventSeq)
			time.Sleep(interval)
		}
		close(stream)
	}()
	return stream
}

func main() {
	streams := make([]<-chan string, instanceCount)
	for instanceID := 0; instanceID < instanceCount; instanceID++ {
		interval := time.Duration(instanceID+1) * baseInterval
		streams[instanceID] = createInstanceStream(instanceID, eventsPerInstance, interval)
	}

	for event := range mergeN(streams...) {
		fmt.Println(event)
	}
	fmt.Println("all instances reported, aggregation complete")
}
```

The pattern is identical to the two-stream version. The only change is iterating over the variadic slice instead of hardcoding two goroutines.

### Verification
```
instance-0: event-0
instance-1: event-0
instance-2: event-0
instance-3: event-0
instance-0: event-1
instance-1: event-1
...
all instances reported, aggregation complete
```
You should see 12 events (4 instances x 3 events each) in interleaved order.

## Step 3 -- Handle Sources Finishing at Different Times

In real systems, some sources produce data for minutes while others finish in seconds. The merge function must handle this gracefully: keep reading from active sources even after others close.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type SensorReading = string

func mergeN(streams ...<-chan SensorReading) <-chan SensorReading {
	merged := make(chan SensorReading)
	var wg sync.WaitGroup

	forwardReadings := func(source <-chan SensorReading) {
		defer wg.Done()
		for reading := range source {
			merged <- reading
		}
	}

	wg.Add(len(streams))
	for _, source := range streams {
		go forwardReadings(source)
	}

	go func() {
		wg.Wait()
		close(merged)
	}()

	return merged
}

func createSensorStream(name string, readings []SensorReading, interval time.Duration) <-chan SensorReading {
	stream := make(chan SensorReading)
	go func() {
		for _, reading := range readings {
			stream <- reading
			time.Sleep(interval)
		}
		close(stream)
		fmt.Printf("--- %s stream ended ---\n", name)
	}()
	return stream
}

func buildTemperatureReadings() []SensorReading {
	return []SensorReading{
		"sensor-A: temperature=22.5C",
		"sensor-A: temperature=22.6C",
	}
}

func buildPressureReadings(count int) []SensorReading {
	readings := make([]SensorReading, count)
	for i := 0; i < count; i++ {
		readings[i] = fmt.Sprintf("sensor-B: pressure=%dhPa", 1013+i)
	}
	return readings
}

func buildHumidityReadings(count int) []SensorReading {
	readings := make([]SensorReading, count)
	for i := 0; i < count; i++ {
		readings[i] = fmt.Sprintf("sensor-C: humidity=%d%%", 60+i)
	}
	return readings
}

func main() {
	sensorA := createSensorStream("sensor-A", buildTemperatureReadings(), 20*time.Millisecond)
	sensorB := createSensorStream("sensor-B", buildPressureReadings(5), 40*time.Millisecond)
	sensorC := createSensorStream("sensor-C", buildHumidityReadings(3), 50*time.Millisecond)

	for event := range mergeN(sensorA, sensorB, sensorC) {
		fmt.Println("aggregated:", event)
	}
	fmt.Println("all sensor streams closed")
}
```

### Verification
```
aggregated: sensor-A: temperature=22.5C
aggregated: sensor-B: pressure=1013hPa
aggregated: sensor-C: humidity=60%
aggregated: sensor-A: temperature=22.6C
--- sensor-A stream ended ---
aggregated: sensor-B: pressure=1014hPa
aggregated: sensor-C: humidity=61%
aggregated: sensor-B: pressure=1015hPa
aggregated: sensor-C: humidity=62%
--- sensor-C stream ended ---
aggregated: sensor-B: pressure=1016hPa
aggregated: sensor-B: pressure=1017hPa
--- sensor-B stream ended ---
all sensor streams closed
```
Sensor A finishes first, but the aggregator keeps reading from B and C until they also close.

## Step 4 -- Merge with Cancellation

Add a done channel so the consumer can cancel all forwarding goroutines without waiting for sources to close. This is essential when you want to stop aggregation early (e.g., after collecting enough data or on shutdown).

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	serviceCount    = 3
	eventsToConsume = 10
	logInterval     = 50 * time.Millisecond
)

func mergeWithCancellation(done <-chan struct{}, streams ...<-chan string) <-chan string {
	merged := make(chan string)
	var wg sync.WaitGroup

	forwardEvents := func(source <-chan string) {
		defer wg.Done()
		for event := range source {
			select {
			case <-done:
				return
			case merged <- event:
			}
		}
	}

	wg.Add(len(streams))
	for _, source := range streams {
		go forwardEvents(source)
	}

	go func() {
		wg.Wait()
		close(merged)
	}()

	return merged
}

func createInfiniteLogStream(done <-chan struct{}, serviceName string, interval time.Duration) <-chan string {
	stream := make(chan string)
	go func() {
		sequence := 0
		for {
			select {
			case <-done:
				close(stream)
				return
			case stream <- fmt.Sprintf("%s: log-entry-%d", serviceName, sequence):
				sequence++
				time.Sleep(interval)
			}
		}
	}()
	return stream
}

func main() {
	done := make(chan struct{})

	serviceNames := []string{"api-gateway", "auth-service", "payment-service"}
	streams := make([]<-chan string, len(serviceNames))
	for i, name := range serviceNames {
		streams[i] = createInfiniteLogStream(done, name, logInterval)
	}

	merged := mergeWithCancellation(done, streams...)

	for i := 0; i < eventsToConsume; i++ {
		fmt.Println(<-merged)
	}

	close(done)
	for range merged {
	} // Drain in-flight events.
	fmt.Println("aggregation cancelled and cleaned up")
}
```

The forward goroutines check `done` on every send to `out`. When `done` is closed, they exit immediately. The drain loop after `close(done)` consumes any values that were in flight.

### Verification
```
api-gateway: log-entry-0
auth-service: log-entry-0
payment-service: log-entry-0
api-gateway: log-entry-1
auth-service: log-entry-1
payment-service: log-entry-1
api-gateway: log-entry-2
auth-service: log-entry-2
payment-service: log-entry-2
api-gateway: log-entry-3
aggregation cancelled and cleaned up
```
Exactly 10 events appear, then clean shutdown. No goroutine leaks.

## Step 5 -- Tagged Events for Source Identification

In a real aggregator, you need to know which source produced each event. Wrap events in a struct that carries the source identifier.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type TaggedEvent struct {
	Source  string
	Payload string
}

type SourceConfig struct {
	Name     string
	Count    int
	Interval time.Duration
}

func mergeTaggedStreams(done <-chan struct{}, streams ...<-chan TaggedEvent) <-chan TaggedEvent {
	merged := make(chan TaggedEvent)
	var wg sync.WaitGroup

	forwardEvents := func(source <-chan TaggedEvent) {
		defer wg.Done()
		for event := range source {
			select {
			case <-done:
				return
			case merged <- event:
			}
		}
	}

	wg.Add(len(streams))
	for _, source := range streams {
		go forwardEvents(source)
	}

	go func() {
		wg.Wait()
		close(merged)
	}()

	return merged
}

func createTaggedSource(done <-chan struct{}, config SourceConfig) <-chan TaggedEvent {
	stream := make(chan TaggedEvent)
	go func() {
		defer close(stream)
		for sequence := 0; sequence < config.Count; sequence++ {
			event := TaggedEvent{
				Source:  config.Name,
				Payload: fmt.Sprintf("entry-%d", sequence),
			}
			select {
			case <-done:
				return
			case stream <- event:
			}
			time.Sleep(config.Interval)
		}
	}()
	return stream
}

func main() {
	done := make(chan struct{})

	sources := []SourceConfig{
		{Name: "nginx-access", Count: 3, Interval: 20 * time.Millisecond},
		{Name: "app-errors", Count: 2, Interval: 40 * time.Millisecond},
		{Name: "audit-log", Count: 4, Interval: 30 * time.Millisecond},
	}

	streams := make([]<-chan TaggedEvent, len(sources))
	for i, config := range sources {
		streams[i] = createTaggedSource(done, config)
	}

	for event := range mergeTaggedStreams(done, streams...) {
		fmt.Printf("[%s] %s\n", event.Source, event.Payload)
	}
	fmt.Println("all sources exhausted")
}
```

### Verification
```
[nginx-access] entry-0
[app-errors] entry-0
[audit-log] entry-0
[nginx-access] entry-1
[audit-log] entry-1
[nginx-access] entry-2
[app-errors] entry-1
[audit-log] entry-2
[audit-log] entry-3
all sources exhausted
```
Each event carries its source name, making it easy to filter, route, or aggregate by origin.

## Common Mistakes

### 1. Closing the Output Channel in Forward Goroutines
Only one goroutine should close `out`, and only after ALL forwarders have finished. This is the WaitGroup goroutine's job. If a forwarder closes `out`, other forwarders will panic when they try to send:

```go
// BAD: multiple goroutines might close out.
forward := func(ch <-chan string) {
    for event := range ch {
        out <- event
    }
    close(out) // PANIC if another forwarder is still sending.
}

// GOOD: one goroutine closes out after all forwarders finish.
go func() {
    wg.Wait()
    close(out)
}()
```

### 2. Not Closing Source Channels
The forwarder uses `range`, which blocks until the source closes. If a source never closes and there is no done channel, the forwarder goroutine leaks forever.

### 3. Forgetting to Drain After Cancellation
If forwarding goroutines sent values to `out` before noticing `done`, those values sit in the channel. Without draining, the goroutines block on the send forever:

```go
close(done)
// REQUIRED: drain in-flight values.
for range merged {
}
```

### 4. Capturing the Loop Variable (Go < 1.22)
In Go versions before 1.22, the loop variable in a `for range` is shared across iterations. Passing it as a function argument avoids the issue:

```go
// SAFE: ch is a function parameter, not a closure capture.
for _, ch := range streams {
    go forward(ch) // ch is passed by value.
}
```

## Review

Multiplexing N channels into one is the fan-in pattern: one goroutine per source
forwards events to a shared output channel, a `sync.WaitGroup` tracks the
forwarders, and a separate goroutine closes the output once every forwarder is
done. In a real-time aggregator -- log streams, sensor feeds, API events -- this
merges a dynamic number of sources into a single stream. A done channel adds
cancellation for stopping early, and sources that finish at different times are
handled naturally, because the merge keeps reading from whatever is still open.

You should be able to explain why the structure is shaped this way. Each source
gets its own forwarding goroutine because a single goroutine ranging over one
channel cannot also block on another -- one per source is what lets them all make
progress independently. The WaitGroup-closer goroutine has to be separate from
the forwarders because closing the output is a single action that must happen
only after all of them finish; a forwarder cannot know it is the last one, and
closing from inside one would panic the others mid-send. Draining after
cancellation is required because a forwarder may already be parked on a send to
the output when `done` closes; unless the consumer keeps receiving, that
goroutine blocks forever and leaks. And the natural next step is generics -- a
`mergeN[T any](...<-chan T) <-chan T` merges channels of any element type with
the exact same body.

## Resources
- [Go Concurrency Patterns: Fan-in](https://go.dev/blog/pipelines) -- the fan-in section of the pipelines post, the source of this pattern.
- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- the talk where fan-in and the channel multiplexer first appear.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) -- the counter that tells the closer goroutine when every forwarder has finished.

---

Back to [Concurrency](../../concurrency.md) | Next: [09-select-with-context](../09-select-with-context/09-select-with-context.md)
