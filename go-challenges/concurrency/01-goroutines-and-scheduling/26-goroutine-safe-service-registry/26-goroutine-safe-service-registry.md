# Exercise 26: Goroutine-Safe Service Registry

API gateways need to know which backend services are alive. In Kubernetes pods
come and go constantly -- a deployment rolls new pods, old ones terminate, health
checks fail intermittently -- so the gateway keeps an in-memory registry that
tracks each backend as registered, healthy, unhealthy, or deregistered.
Registration goroutines add and remove services, health-check goroutines probe
and update state, and router goroutines look up healthy instances, all at the
same time. The hard part is not the mutex; it is deciding which goroutine is
allowed to make which change.

## What you'll build

```text
26-goroutine-safe-service-registry/
  main.go        a state machine, an append-only event log, and a
                 Registry driven by registrar, health-checker, and router
                 goroutines under an RWMutex
```

- Build: a concurrent service registry with an ownership model and an audit event log.
- Implement: `ServiceState` transitions with `validTransitions`, an append-only `EventLog`, a `Registry` with `Register`/`Deregister`/`UpdateHealth`/`GetHealthy`, and the `registrar`, `healthChecker`, and `router` goroutines.
- Verify: `go run main.go` (add `-race` to prove the RWMutex guards the shared state).

### Why ownership, not locking, is the real problem

The concurrency challenge here is not about locks -- it is about ownership. When a health-check goroutine discovers a service is unhealthy, should it deregister it? Or should it only mark it unhealthy and let a separate goroutine decide? When a registration goroutine deregisters a service, what happens if a health-check is running against that service at the same time? The state machine has rules: a deregistered service cannot become healthy again. A service must be registered before it can be health-checked. These invariants must hold under concurrent access.

The event log adds a second dimension: every state change is recorded as an immutable event. This is the same pattern used in production service meshes for debugging. When something goes wrong ("Why did service X stop receiving traffic?"), the event log provides a causal trace: registered at T1, healthy at T2, unhealthy at T3 (health check failed: connection refused), deregistered at T4 (3 consecutive failures).


## Step 1 -- State Machine and Event Log

Define the service states, valid transitions, and an append-only event log. No concurrency yet -- establish the domain model first.

```go
package main

import (
	"fmt"
	"time"
)

type ServiceState int

const (
	StateRegistered   ServiceState = iota
	StateHealthy
	StateUnhealthy
	StateDeregistered
)

func (s ServiceState) String() string {
	switch s {
	case StateRegistered:
		return "registered"
	case StateHealthy:
		return "healthy"
	case StateUnhealthy:
		return "unhealthy"
	case StateDeregistered:
		return "deregistered"
	default:
		return "unknown"
	}
}

var validTransitions = map[ServiceState][]ServiceState{
	StateRegistered:   {StateHealthy, StateUnhealthy, StateDeregistered},
	StateHealthy:      {StateUnhealthy, StateDeregistered},
	StateUnhealthy:    {StateHealthy, StateDeregistered},
	StateDeregistered: {},
}

func isValidTransition(from, to ServiceState) bool {
	for _, allowed := range validTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

type Event struct {
	Timestamp   time.Time
	ServiceName string
	FromState   ServiceState
	ToState     ServiceState
	Reason      string
}

func (e Event) String() string {
	return fmt.Sprintf("[%s] %s: %s -> %s (%s)",
		e.Timestamp.Format("15:04:05.000"),
		e.ServiceName, e.FromState, e.ToState, e.Reason)
}

type EventLog struct {
	events []Event
}

func NewEventLog() *EventLog {
	return &EventLog{events: make([]Event, 0, 64)}
}

func (el *EventLog) Append(e Event) {
	el.events = append(el.events, e)
}

func (el *EventLog) All() []Event {
	copied := make([]Event, len(el.events))
	copy(copied, el.events)
	return copied
}

func (el *EventLog) ForService(name string) []Event {
	var result []Event
	for _, e := range el.events {
		if e.ServiceName == name {
			result = append(result, e)
		}
	}
	return result
}

func main() {
	log := NewEventLog()

	log.Append(Event{
		Timestamp:   time.Now(),
		ServiceName: "orders-api",
		FromState:   StateRegistered,
		ToState:     StateHealthy,
		Reason:      "initial health check passed",
	})

	time.Sleep(10 * time.Millisecond)

	log.Append(Event{
		Timestamp:   time.Now(),
		ServiceName: "orders-api",
		FromState:   StateHealthy,
		ToState:     StateUnhealthy,
		Reason:      "health check failed: connection refused",
	})

	fmt.Println("=== State Machine Transitions ===")
	fmt.Printf("  registered -> healthy:      %v\n", isValidTransition(StateRegistered, StateHealthy))
	fmt.Printf("  healthy -> unhealthy:       %v\n", isValidTransition(StateHealthy, StateUnhealthy))
	fmt.Printf("  deregistered -> healthy:    %v\n", isValidTransition(StateDeregistered, StateHealthy))
	fmt.Printf("  unhealthy -> deregistered:  %v\n", isValidTransition(StateUnhealthy, StateDeregistered))

	fmt.Println()
	fmt.Println("=== Event Log ===")
	for _, e := range log.All() {
		fmt.Printf("  %s\n", e)
	}

	fmt.Printf("\n=== Events for orders-api: %d ===\n", len(log.ForService("orders-api")))
}
```

**What's happening here:** The state machine defines four states and the legal transitions between them. `StateDeregistered` is terminal -- no transitions out of it. The `EventLog` is an append-only slice that records every state change with a timestamp, service name, previous state, new state, and a reason string. This event-sourcing approach means you can reconstruct the full history of any service.

**Key insight:** By defining `validTransitions` as a map, the state machine is data-driven rather than embedded in if-else chains. Adding a new state (e.g., `StateDraining`) means adding one map entry, not modifying every transition function.

### Verification
```bash
go run main.go
```
Expected output:
```
=== State Machine Transitions ===
  registered -> healthy:      true
  healthy -> unhealthy:       true
  deregistered -> healthy:    false
  unhealthy -> deregistered:  true

=== Event Log ===
  [HH:MM:SS.mmm] orders-api: registered -> healthy (initial health check passed)
  [HH:MM:SS.mmm] orders-api: healthy -> unhealthy (health check failed: connection refused)

=== Events for orders-api: 2 ===
```


## Step 2 -- Concurrent Registry with Ownership Model

Build the registry with clear ownership rules: only the registrar creates and removes services, only the health-checker transitions health state, routers only read. The mutex protects the data, but the ownership model determines who calls which methods.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type ServiceState int

const (
	StateRegistered   ServiceState = iota
	StateHealthy
	StateUnhealthy
	StateDeregistered
)

func (s ServiceState) String() string {
	switch s {
	case StateRegistered:
		return "registered"
	case StateHealthy:
		return "healthy"
	case StateUnhealthy:
		return "unhealthy"
	case StateDeregistered:
		return "deregistered"
	default:
		return "unknown"
	}
}

var validTransitions = map[ServiceState][]ServiceState{
	StateRegistered:   {StateHealthy, StateUnhealthy, StateDeregistered},
	StateHealthy:      {StateUnhealthy, StateDeregistered},
	StateUnhealthy:    {StateHealthy, StateDeregistered},
	StateDeregistered: {},
}

type Event struct {
	Timestamp   time.Time
	ServiceName string
	FromState   ServiceState
	ToState     ServiceState
	Reason      string
}

type ServiceInfo struct {
	Name  string
	State ServiceState
	Addr  string
}

type Registry struct {
	mu       sync.RWMutex
	services map[string]*ServiceInfo
	events   []Event
}

func NewRegistry() *Registry {
	return &Registry{
		services: make(map[string]*ServiceInfo),
		events:   make([]Event, 0, 128),
	}
}

func (r *Registry) Register(name, addr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if svc, exists := r.services[name]; exists && svc.State != StateDeregistered {
		return false
	}

	r.services[name] = &ServiceInfo{Name: name, State: StateRegistered, Addr: addr}
	r.events = append(r.events, Event{
		Timestamp:   time.Now(),
		ServiceName: name,
		ToState:     StateRegistered,
		Reason:      fmt.Sprintf("registered at %s", addr),
	})
	return true
}

func (r *Registry) Deregister(name, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	svc, exists := r.services[name]
	if !exists || svc.State == StateDeregistered {
		return false
	}

	from := svc.State
	svc.State = StateDeregistered
	r.events = append(r.events, Event{
		Timestamp:   time.Now(),
		ServiceName: name,
		FromState:   from,
		ToState:     StateDeregistered,
		Reason:      reason,
	})
	return true
}

func (r *Registry) UpdateHealth(name string, healthy bool, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	svc, exists := r.services[name]
	if !exists || svc.State == StateDeregistered {
		return false
	}

	var target ServiceState
	if healthy {
		target = StateHealthy
	} else {
		target = StateUnhealthy
	}

	if svc.State == target {
		return true
	}

	valid := false
	for _, allowed := range validTransitions[svc.State] {
		if allowed == target {
			valid = true
			break
		}
	}
	if !valid {
		return false
	}

	from := svc.State
	svc.State = target
	r.events = append(r.events, Event{
		Timestamp:   time.Now(),
		ServiceName: name,
		FromState:   from,
		ToState:     target,
		Reason:      reason,
	})
	return true
}

func (r *Registry) GetHealthy() []ServiceInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []ServiceInfo
	for _, svc := range r.services {
		if svc.State == StateHealthy {
			result = append(result, *svc)
		}
	}
	return result
}

func (r *Registry) Events() []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()
	copied := make([]Event, len(r.events))
	copy(copied, r.events)
	return copied
}

func main() {
	reg := NewRegistry()

	reg.Register("auth-svc", "10.0.1.1:8080")
	reg.Register("orders-svc", "10.0.1.2:8080")
	reg.Register("payments-svc", "10.0.1.3:8080")

	reg.UpdateHealth("auth-svc", true, "health check passed")
	reg.UpdateHealth("orders-svc", true, "health check passed")
	reg.UpdateHealth("payments-svc", false, "connection refused")

	healthy := reg.GetHealthy()
	fmt.Printf("=== Healthy Services: %d ===\n", len(healthy))
	for _, svc := range healthy {
		fmt.Printf("  %s at %s\n", svc.Name, svc.Addr)
	}

	reg.Deregister("payments-svc", "3 consecutive health check failures")

	reregistered := reg.UpdateHealth("payments-svc", true, "recovered")
	fmt.Printf("\nUpdate deregistered payments-svc: %v (should be false)\n", reregistered)

	fmt.Println()
	fmt.Println("=== Full Event Log ===")
	for _, e := range reg.Events() {
		fmt.Printf("  [%s] %s: %s -> %s (%s)\n",
			e.Timestamp.Format("15:04:05.000"),
			e.ServiceName, e.FromState, e.ToState, e.Reason)
	}
}
```

**What's happening here:** The registry exposes three mutation methods with clear ownership semantics: `Register`/`Deregister` are for the registrar goroutine, `UpdateHealth` is for health-checker goroutines, and `GetHealthy` is for router goroutines. Each method enforces the state machine internally. A deregistered service cannot be updated -- the method returns `false`. This ownership model means you do not need external coordination between goroutines; the registry's API enforces the rules.

**Key insight:** The ownership model is a design decision, not a language feature. The mutex prevents data races, but it does not prevent logical errors like a router goroutine deregistering a service. Ownership is enforced by API design: you only expose methods appropriate for each goroutine's role. In production, you might separate these into different interfaces (`RegistrarAPI`, `HealthCheckerAPI`, `RouterAPI`).

### Verification
```bash
go run main.go
```
Expected output:
```
=== Healthy Services: 2 ===
  auth-svc at 10.0.1.1:8080
  orders-svc at 10.0.1.2:8080

Update deregistered payments-svc: false (should be false)

=== Full Event Log ===
  [HH:MM:SS.mmm] auth-svc:  -> registered (registered at 10.0.1.1:8080)
  [HH:MM:SS.mmm] orders-svc:  -> registered (registered at 10.0.1.2:8080)
  [HH:MM:SS.mmm] payments-svc:  -> registered (registered at 10.0.1.3:8080)
  [HH:MM:SS.mmm] auth-svc: registered -> healthy (health check passed)
  [HH:MM:SS.mmm] orders-svc: registered -> healthy (health check passed)
  [HH:MM:SS.mmm] payments-svc: registered -> unhealthy (connection refused)
  [HH:MM:SS.mmm] payments-svc: unhealthy -> deregistered (3 consecutive health check failures)
```


## Step 3 -- Full Concurrent System

Launch all three goroutine roles simultaneously: a registrar adding/removing services, health-checkers probing services, and routers querying for healthy backends. Observe the event log that captures the full lifecycle.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

type ServiceState int

const (
	StateRegistered   ServiceState = iota
	StateHealthy
	StateUnhealthy
	StateDeregistered
)

func (s ServiceState) String() string {
	switch s {
	case StateRegistered:
		return "registered"
	case StateHealthy:
		return "healthy"
	case StateUnhealthy:
		return "unhealthy"
	case StateDeregistered:
		return "deregistered"
	default:
		return "unknown"
	}
}

var validTransitions = map[ServiceState][]ServiceState{
	StateRegistered:   {StateHealthy, StateUnhealthy, StateDeregistered},
	StateHealthy:      {StateUnhealthy, StateDeregistered},
	StateUnhealthy:    {StateHealthy, StateDeregistered},
	StateDeregistered: {},
}

const (
	numServices            = 6
	healthCheckInterval    = 80 * time.Millisecond
	routerQueryInterval    = 100 * time.Millisecond
	simulationDuration     = 800 * time.Millisecond
	maxConsecutiveFailures = 3
)

type Event struct {
	Timestamp   time.Time
	ServiceName string
	FromState   ServiceState
	ToState     ServiceState
	Reason      string
}

type ServiceInfo struct {
	Name              string
	State             ServiceState
	Addr              string
	ConsecutiveFails  int
}

type Registry struct {
	mu       sync.RWMutex
	services map[string]*ServiceInfo
	events   []Event
}

func NewRegistry() *Registry {
	return &Registry{
		services: make(map[string]*ServiceInfo),
		events:   make([]Event, 0, 256),
	}
}

func (r *Registry) Register(name, addr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if svc, exists := r.services[name]; exists && svc.State != StateDeregistered {
		return false
	}
	r.services[name] = &ServiceInfo{Name: name, State: StateRegistered, Addr: addr}
	r.events = append(r.events, Event{
		Timestamp: time.Now(), ServiceName: name,
		ToState: StateRegistered, Reason: fmt.Sprintf("registered at %s", addr),
	})
	return true
}

func (r *Registry) Deregister(name, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	svc, exists := r.services[name]
	if !exists || svc.State == StateDeregistered {
		return false
	}
	from := svc.State
	svc.State = StateDeregistered
	r.events = append(r.events, Event{
		Timestamp: time.Now(), ServiceName: name,
		FromState: from, ToState: StateDeregistered, Reason: reason,
	})
	return true
}

func (r *Registry) UpdateHealth(name string, healthy bool, reason string) (deregister bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	svc, exists := r.services[name]
	if !exists || svc.State == StateDeregistered {
		return false
	}

	if healthy {
		svc.ConsecutiveFails = 0
	} else {
		svc.ConsecutiveFails++
	}

	if svc.ConsecutiveFails >= maxConsecutiveFailures {
		return true
	}

	var target ServiceState
	if healthy {
		target = StateHealthy
	} else {
		target = StateUnhealthy
	}

	if svc.State == target {
		return false
	}

	valid := false
	for _, allowed := range validTransitions[svc.State] {
		if allowed == target {
			valid = true
			break
		}
	}
	if !valid {
		return false
	}

	from := svc.State
	svc.State = target
	r.events = append(r.events, Event{
		Timestamp: time.Now(), ServiceName: name,
		FromState: from, ToState: target, Reason: reason,
	})
	return false
}

func (r *Registry) GetHealthy() []ServiceInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []ServiceInfo
	for _, svc := range r.services {
		if svc.State == StateHealthy {
			result = append(result, *svc)
		}
	}
	return result
}

func (r *Registry) ActiveNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for _, svc := range r.services {
		if svc.State != StateDeregistered {
			names = append(names, svc.Name)
		}
	}
	return names
}

func (r *Registry) Events() []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()
	copied := make([]Event, len(r.events))
	copy(copied, r.events)
	return copied
}

func registrar(reg *Registry, stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()

	services := []struct{ name, addr string }{
		{"auth-svc", "10.0.1.1:8080"},
		{"orders-svc", "10.0.1.2:8080"},
		{"payments-svc", "10.0.1.3:8080"},
		{"users-svc", "10.0.1.4:8080"},
		{"inventory-svc", "10.0.1.5:8080"},
		{"notifications-svc", "10.0.1.6:8080"},
	}

	for i, svc := range services {
		select {
		case <-stop:
			return
		default:
		}
		reg.Register(svc.name, svc.addr)
		if i < len(services)-1 {
			time.Sleep(30 * time.Millisecond)
		}
	}

	<-stop
}

func healthChecker(reg *Registry, stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0))

	for {
		select {
		case <-ticker.C:
			names := reg.ActiveNames()
			for _, name := range names {
				healthy := rng.Float64() > 0.3
				var reason string
				if healthy {
					reason = "TCP check passed"
				} else {
					reason = "connection refused"
				}
				shouldDeregister := reg.UpdateHealth(name, healthy, reason)
				if shouldDeregister {
					reg.Deregister(name, fmt.Sprintf("%d consecutive failures", maxConsecutiveFailures))
				}
			}
		case <-stop:
			return
		}
	}
}

func router(id int, reg *Registry, stop <-chan struct{}, wg *sync.WaitGroup, lookups *int64) {
	defer wg.Done()
	ticker := time.NewTicker(routerQueryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			healthy := reg.GetHealthy()
			atomic.AddInt64(lookups, 1)
			_ = healthy
		case <-stop:
			return
		}
	}
}

func main() {
	reg := NewRegistry()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	var totalLookups int64

	fmt.Println("=== Service Registry Simulation ===")
	fmt.Printf("  Services: %d | Health interval: %v | Duration: %v\n\n",
		numServices, healthCheckInterval, simulationDuration)

	wg.Add(1)
	go registrar(reg, stop, &wg)

	time.Sleep(50 * time.Millisecond)

	wg.Add(1)
	go healthChecker(reg, stop, &wg)

	for i := 1; i <= 3; i++ {
		wg.Add(1)
		go router(i, reg, stop, &wg, &totalLookups)
	}

	time.Sleep(simulationDuration)
	close(stop)
	wg.Wait()

	events := reg.Events()
	fmt.Printf("=== Event Log (%d events) ===\n", len(events))
	for _, e := range events {
		fmt.Printf("  [%s] %-20s %s -> %s (%s)\n",
			e.Timestamp.Format("15:04:05.000"),
			e.ServiceName, e.FromState, e.ToState, e.Reason)
	}

	healthy := reg.GetHealthy()
	lookups := atomic.LoadInt64(&totalLookups)

	fmt.Printf("\n=== Final State ===\n")
	fmt.Printf("  Healthy services: %d\n", len(healthy))
	for _, svc := range healthy {
		fmt.Printf("    %s at %s\n", svc.Name, svc.Addr)
	}
	fmt.Printf("  Total router lookups: %d\n", lookups)
	fmt.Printf("  Total events recorded: %d\n", len(events))

	fmt.Printf("\n=== Goroutine Roles ===\n")
	fmt.Printf("  1 registrar   (owns: Register, Deregister)\n")
	fmt.Printf("  1 health-checker (owns: UpdateHealth, triggers Deregister)\n")
	fmt.Printf("  3 routers     (owns: GetHealthy -- read only)\n")
}
```

**What's happening here:** Five goroutines interact with the registry simultaneously. The registrar goroutine registers six services with staggered timing. The health-checker goroutine runs every 80ms, probing all active services with a 70% success probability. When a service accumulates 3 consecutive failures, the health-checker triggers deregistration -- it signals the need, but the registry enforces the state machine rules. Three router goroutines query for healthy services every 100ms. All mutations are recorded in the event log.

**Key insight:** The `UpdateHealth` method returns a `bool` indicating whether the service should be deregistered. The health-checker goroutine then calls `Deregister`. This two-step pattern is intentional: `UpdateHealth` detects the condition, `Deregister` performs the action. The health-checker orchestrates the flow but the registry enforces the transition rules. This separation of detection from action is the core of the ownership model.

### Verification
```bash
go run main.go
```
Expected output (events vary due to randomness):
```
=== Service Registry Simulation ===
  Services: 6 | Health interval: 80ms | Duration: 800ms

=== Event Log (18 events) ===
  [HH:MM:SS.mmm] auth-svc               -> registered (registered at 10.0.1.1:8080)
  [HH:MM:SS.mmm] orders-svc             -> registered (registered at 10.0.1.2:8080)
  [HH:MM:SS.mmm] payments-svc           -> registered (registered at 10.0.1.3:8080)
  [HH:MM:SS.mmm] users-svc              -> registered (registered at 10.0.1.4:8080)
  [HH:MM:SS.mmm] inventory-svc          -> registered (registered at 10.0.1.5:8080)
  [HH:MM:SS.mmm] notifications-svc      -> registered (registered at 10.0.1.6:8080)
  [HH:MM:SS.mmm] auth-svc              registered -> healthy (TCP check passed)
  [HH:MM:SS.mmm] orders-svc            registered -> unhealthy (connection refused)
  ...

=== Final State ===
  Healthy services: 3
    auth-svc at 10.0.1.1:8080
    users-svc at 10.0.1.4:8080
    inventory-svc at 10.0.1.5:8080
  Total router lookups: 24
  Total events recorded: 18

=== Goroutine Roles ===
  1 registrar   (owns: Register, Deregister)
  1 health-checker (owns: UpdateHealth, triggers Deregister)
  3 routers     (owns: GetHealthy -- read only)
```


## Common Mistakes

### Allowing Any Goroutine to Mutate Any State

```go
// Wrong: router goroutine directly deregisters a service it cannot reach
func routerHandler(reg *Registry, serviceName string) {
	_, err := callService(serviceName)
	if err != nil {
		reg.Deregister(serviceName, "router got error")
		// Now the router owns deregistration decisions, conflicting
		// with the health-checker that also deregisters
	}
}
```
**What happens:** Two goroutines (router and health-checker) both make deregistration decisions. A router might deregister a service due to a transient network blip that the health-checker would have tolerated. The ownership boundary is violated -- there is no single source of truth for "when should a service be removed."

**Fix:** Routers only read (`GetHealthy`). If a router encounters an error, it reports it through a channel or metric, but it does not deregister. The health-checker is the sole authority on health transitions.


### Not Checking State Machine Validity on Transitions

```go
// Wrong: transitions without validation
func (r *Registry) SetState(name string, state ServiceState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services[name].State = state // no validation: deregistered -> healthy is allowed
}
```
**What happens:** A service marked deregistered can suddenly become healthy again. Downstream systems (load balancers, monitoring) receive contradictory signals. In production, this causes traffic to be routed to a pod that is shutting down.

**Fix:** Every state change must validate against `validTransitions`. The method returns `false` for illegal transitions rather than silently applying them.


### Mutating Returned Slices That Share Memory

```go
// Wrong: returning the internal slice directly
func (r *Registry) Events() []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.events // caller holds a reference to the internal slice
}
```
**What happens:** The caller can read events while another goroutine appends new ones via `Append`. Even though the read lock was held during the return, the caller continues using the slice after the lock is released. If the registry appends new events and the slice grows, the caller may see partially written data.

**Fix:** Return a copy of the slice. The copy is a snapshot that the caller can read freely without holding any lock.


## Review

The design rests on an ownership model layered on top of the mutex: the registrar
owns `Register` and `Deregister`, the health-checker owns `UpdateHealth` and only
triggers deregistration, and routers hold read-only access through `GetHealthy`.
The `RWMutex` prevents data races and lets many router lookups run concurrently
while serializing writes, but it does nothing to stop a router from logically
corrupting state -- that is what the API surface enforces by exposing only the
methods each role should call. Every transition is checked against a transition
table so illegal moves like `deregistered -> healthy` are rejected rather than
silently applied, and every change is appended to an immutable event log that
turns "why did service X stop receiving traffic?" into a readable causal trace.
Two details keep it correct under concurrency: returning copies of internal
slices so a caller cannot read a slice another goroutine is appending to, and
separating detection from action so the health-checker decides when to remove a
service while the registry decides whether the removal is legal.

Prove you can extend the machine without breaking its invariants: add a
`StateDraining` between healthy and deregistered, with valid transitions
`healthy -> draining` and `draining -> deregistered`. A draining service should
still surface in `GetHealthy` results but carry a `Draining` flag, and a new
drainer goroutine should wait out a simulated connection drain before
deregistering, so the event log records the full `registered -> healthy ->
draining -> deregistered` lifecycle. The open question you should be able to
answer is how the drainer discovers services that entered draining -- by polling
the registry, or by having the health-checker signal it through a channel --
without violating the ownership boundaries the rest of the system depends on.

## Resources

- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) -- the read-write lock that lets router reads run concurrently while writes serialize.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) -- goroutine design patterns and the share-by-communicating mindset behind the ownership model.
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the philosophy that motivates assigning state to a single owner.
- [Event Sourcing Pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/event-sourcing) -- the append-only event-log pattern this registry uses for its audit trail.

---

Back to [Concurrency](../../concurrency.md) | Next: [27-scatter-gather-partial-results](../27-scatter-gather-partial-results/27-scatter-gather-partial-results.md)
