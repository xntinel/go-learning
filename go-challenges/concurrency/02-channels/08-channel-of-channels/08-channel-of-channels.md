# Exercise 8: Channel of Channels

Most channel examples show one-way data flow: a producer sends, a consumer
receives. Real systems need something harder -- request-response, where a client
sends a request and waits for its own specific answer. HTTP servers, database
queries, and RPC calls all follow this shape. The trick is to embed a response
channel inside the request itself, so each client carries its own return
address. Get that wrong and concurrent clients see each other's responses; get
it right and you have safe concurrent access to shared state with no mutex
anywhere.

## What you'll build

```text
08-channel-of-channels/
  main.go        request structs carrying reply channels, driving a
                 service goroutine that owns all state
```

- Build: a request-response service where each client gets its own reply channel.
- Implement: `LookupRequest`/`LookupResponse` with an embedded `Reply` channel; a `UserService` whose `run` loop owns the map; then `ConfigService`, `RateLimiter`, and `AccountService` variants that add operations and error fields.
- Verify: `go run main.go`

### Why a reply channel replaces a mutex

In Go, you model request-response by embedding a response channel inside the
request. The client creates a one-shot response channel, includes it in the
request struct, and sends the request on a shared channel. The service goroutine
receives the request, processes it, and sends the result back on the embedded
response channel.

This is how you build safe concurrent access to shared state without mutexes.
The service goroutine is the only one that touches the state. Clients
communicate through channels, which are inherently safe. The Go proverb applies
perfectly: "Don't communicate by sharing memory; share memory by
communicating."

## Step 1 -- The Request-Response Pattern

Define a request struct that carries a response channel -- the "return address." Each client creates its own Reply channel, so responses go to the right caller even with concurrent requests.

```go
package main

import "fmt"

// LookupRequest carries the user ID to look up and a reply channel
// where the service will send back the result.
type LookupRequest struct {
	UserID int
	Reply  chan LookupResponse
}

// LookupResponse carries the result of a user lookup.
type LookupResponse struct {
	Name  string
	Email string
	Found bool
}

// UserRecord is a single entry in the user directory.
type UserRecord struct {
	Name  string
	Email string
}

// UserService processes lookup requests sequentially. It owns the data
// exclusively -- no mutex needed because only this goroutine reads/writes it.
type UserService struct {
	requests chan LookupRequest
	users    map[int]UserRecord
}

// NewUserService creates a service with seed data and starts its event loop.
func NewUserService(users map[int]UserRecord) *UserService {
	svc := &UserService{
		requests: make(chan LookupRequest),
		users:    users,
	}
	go svc.run()
	return svc
}

func (svc *UserService) run() {
	for req := range svc.requests {
		if user, ok := svc.users[req.UserID]; ok {
			req.Reply <- LookupResponse{Name: user.Name, Email: user.Email, Found: true}
		} else {
			req.Reply <- LookupResponse{Found: false}
		}
	}
}

// Lookup sends a request and waits for the response.
func (svc *UserService) Lookup(userID int) LookupResponse {
	reply := make(chan LookupResponse, 1)
	svc.requests <- LookupRequest{UserID: userID, Reply: reply}
	return <-reply
}

// Close shuts down the service event loop.
func (svc *UserService) Close() {
	close(svc.requests)
}

func main() {
	svc := NewUserService(map[int]UserRecord{
		1: {"Alice", "alice@corp.com"},
		2: {"Bob", "bob@corp.com"},
		3: {"Carol", "carol@corp.com"},
	})

	resp := svc.Lookup(2)
	fmt.Printf("User 2: %s <%s> (found=%v)\n", resp.Name, resp.Email, resp.Found)

	resp = svc.Lookup(99)
	fmt.Printf("User 99: found=%v\n", resp.Found)

	svc.Close()
}
```

Because the service is a single goroutine processing requests sequentially, all state access is safe -- no mutexes needed.

### Verification
```bash
go run main.go
# Expected:
#   User 2: Bob <bob@corp.com> (found=true)
#   User 99: found=false
```

## Step 2 -- Concurrent Clients

Multiple goroutines send requests to the same service simultaneously. Each gets its own response because each creates its own reply channel.

```go
package main

import (
	"fmt"
	"sync"
)

// LookupRequest carries a user ID and a private reply channel.
type LookupRequest struct {
	UserID int
	Reply  chan string
}

// runUserService processes lookup requests until the channel is closed.
func runUserService(requests <-chan LookupRequest) {
	users := map[int]string{
		1: "Alice", 2: "Bob", 3: "Carol",
		4: "Dave", 5: "Eve",
	}
	for req := range requests {
		if name, ok := users[req.UserID]; ok {
			req.Reply <- name
		} else {
			req.Reply <- "unknown"
		}
	}
}

// lookupUser sends a request and returns the resolved name.
func lookupUser(requests chan<- LookupRequest, userID int) string {
	reply := make(chan string, 1)
	requests <- LookupRequest{UserID: userID, Reply: reply}
	return <-reply
}

// runConcurrentLookups launches one goroutine per user ID, each performing
// an independent lookup through the shared request channel.
func runConcurrentLookups(requests chan<- LookupRequest, userIDs []int) {
	var wg sync.WaitGroup
	for _, userID := range userIDs {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			name := lookupUser(requests, id)
			fmt.Printf("Client looked up user %d: %s\n", id, name)
		}(userID)
	}
	wg.Wait()
}

func main() {
	requests := make(chan LookupRequest)
	go runUserService(requests)

	userIDs := []int{1, 2, 3, 4, 5}
	runConcurrentLookups(requests, userIDs)

	close(requests)
}
```

### Verification
```bash
go run main.go
# Expected (order may vary):
#   Client looked up user 1: Alice
#   Client looked up user 2: Bob
#   Client looked up user 3: Carol
#   Client looked up user 4: Dave
#   Client looked up user 5: Eve
```

## Step 3 -- RPC-Style Broker with Multiple Operations

Build a more realistic request-response broker that supports multiple operations: get, set, and delete. This simulates an in-memory configuration service that multiple microservices query concurrently.

```go
package main

import "fmt"

const (
	opGet    = "get"
	opSet    = "set"
	opDelete = "delete"
)

// ConfigResponse carries the result of a config operation.
type ConfigResponse struct {
	Value string
	Found bool
}

// ConfigRequest describes an operation (get/set/delete) with a reply channel.
type ConfigRequest struct {
	Op    string
	Key   string
	Value string
	Reply chan ConfigResponse
}

// ConfigService manages an in-memory key-value store, processing all
// mutations through a single goroutine to avoid data races.
type ConfigService struct {
	requests chan ConfigRequest
}

// NewConfigService creates and starts a config service.
func NewConfigService() *ConfigService {
	svc := &ConfigService{requests: make(chan ConfigRequest)}
	go svc.run()
	return svc
}

func (svc *ConfigService) run() {
	store := make(map[string]string)
	for req := range svc.requests {
		switch req.Op {
		case opSet:
			store[req.Key] = req.Value
			req.Reply <- ConfigResponse{Value: req.Value, Found: true}
		case opGet:
			val, ok := store[req.Key]
			req.Reply <- ConfigResponse{Value: val, Found: ok}
		case opDelete:
			delete(store, req.Key)
			req.Reply <- ConfigResponse{Found: true}
		}
	}
}

// Set stores a key-value pair.
func (svc *ConfigService) Set(key, value string) {
	reply := make(chan ConfigResponse, 1)
	svc.requests <- ConfigRequest{Op: opSet, Key: key, Value: value, Reply: reply}
	<-reply
}

// Get retrieves a value by key.
func (svc *ConfigService) Get(key string) (string, bool) {
	reply := make(chan ConfigResponse, 1)
	svc.requests <- ConfigRequest{Op: opGet, Key: key, Reply: reply}
	resp := <-reply
	return resp.Value, resp.Found
}

// Delete removes a key from the store.
func (svc *ConfigService) Delete(key string) {
	reply := make(chan ConfigResponse, 1)
	svc.requests <- ConfigRequest{Op: opDelete, Key: key, Reply: reply}
	<-reply
}

// Close shuts down the service.
func (svc *ConfigService) Close() {
	close(svc.requests)
}

func main() {
	config := NewConfigService()

	config.Set("db.host", "postgres.prod.internal")
	config.Set("db.port", "5432")
	config.Set("cache.ttl", "300s")

	if val, ok := config.Get("db.host"); ok {
		fmt.Printf("db.host = %s\n", val)
	}
	if val, ok := config.Get("cache.ttl"); ok {
		fmt.Printf("cache.ttl = %s\n", val)
	}

	config.Delete("cache.ttl")
	val, ok := config.Get("cache.ttl")
	fmt.Printf("cache.ttl after delete: %q (found=%v)\n", val, ok)

	config.Close()
}
```

### Verification
```bash
go run main.go
# Expected:
#   db.host = postgres.prod.internal
#   cache.ttl = 300s
#   cache.ttl after delete: "" (found=false)
```

## Step 4 -- Rate Limiter Service

A richer example: a rate limiter that tracks request counts per API key. Multiple API gateway goroutines check limits concurrently. The service goroutine owns the counter state -- no race conditions possible.

```go
package main

import (
	"fmt"
	"sync"
)

const maxRequestsPerKey = 3

// RateLimitResponse carries the allow/deny decision and remaining quota.
type RateLimitResponse struct {
	Allowed   bool
	Remaining int
}

// RateLimitRequest carries an API key and a reply channel for the decision.
type RateLimitRequest struct {
	APIKey string
	Reply  chan RateLimitResponse
}

// RateLimiter tracks request counts per API key through a single goroutine,
// eliminating the need for mutexes on the counter state.
type RateLimiter struct {
	requests  chan RateLimitRequest
	maxPerKey int
}

// NewRateLimiter creates and starts a rate limiter with the given per-key limit.
func NewRateLimiter(maxPerKey int) *RateLimiter {
	rl := &RateLimiter{
		requests:  make(chan RateLimitRequest),
		maxPerKey: maxPerKey,
	}
	go rl.run()
	return rl
}

func (rl *RateLimiter) run() {
	counts := make(map[string]int)
	for req := range rl.requests {
		current := counts[req.APIKey]
		if current < rl.maxPerKey {
			counts[req.APIKey]++
			req.Reply <- RateLimitResponse{
				Allowed:   true,
				Remaining: rl.maxPerKey - current - 1,
			}
		} else {
			req.Reply <- RateLimitResponse{Allowed: false, Remaining: 0}
		}
	}
}

// Check sends a rate-limit request and returns the decision.
func (rl *RateLimiter) Check(apiKey string) RateLimitResponse {
	reply := make(chan RateLimitResponse, 1)
	rl.requests <- RateLimitRequest{APIKey: apiKey, Reply: reply}
	return <-reply
}

// Close shuts down the rate limiter.
func (rl *RateLimiter) Close() {
	close(rl.requests)
}

// formatStatus returns a human-readable string for the rate limit decision.
func formatStatus(resp RateLimitResponse) string {
	if resp.Allowed {
		return "ALLOWED"
	}
	return "BLOCKED"
}

func main() {
	limiter := NewRateLimiter(maxRequestsPerKey)

	var wg sync.WaitGroup
	apiKeys := []string{"key-alice", "key-alice", "key-alice", "key-alice", "key-bob", "key-bob"}

	for i, key := range apiKeys {
		wg.Add(1)
		go func(reqNum int, apiKey string) {
			defer wg.Done()
			resp := limiter.Check(apiKey)
			fmt.Printf("Request %d [%s]: %s (remaining: %d)\n",
				reqNum+1, apiKey, formatStatus(resp), resp.Remaining)
		}(i, key)
	}

	wg.Wait()
	limiter.Close()
}
```

### Verification
```bash
go run main.go
# Expected: first 3 requests for key-alice are ALLOWED, 4th is BLOCKED
# Both requests for key-bob are ALLOWED
```

## Step 5 -- Request with Error Handling

In production, service operations can fail. Include an error field in the response to communicate failures back to the caller, simulating a real RPC system.

```go
package main

import (
	"fmt"
	"sync"
)

const (
	opDeposit  = "deposit"
	opWithdraw = "withdraw"
	opBalance  = "balance"

	depositAmount  = 100.0
	withdrawAmount = 80.0
	depositorCount = 5
	withdrawerCount = 5
)

// AccountResponse carries the result of a bank operation, including
// an optional error message for failed transactions.
type AccountResponse struct {
	Balance float64
	Error   string
}

// AccountRequest describes a bank operation with a reply channel.
type AccountRequest struct {
	Op     string
	Amount float64
	Reply  chan AccountResponse
}

// AccountService manages a bank account balance through sequential
// request processing, eliminating race conditions without mutexes.
type AccountService struct {
	requests chan AccountRequest
}

// NewAccountService creates and starts an account service.
func NewAccountService() *AccountService {
	svc := &AccountService{requests: make(chan AccountRequest)}
	go svc.run()
	return svc
}

func (svc *AccountService) run() {
	var balance float64
	for req := range svc.requests {
		switch req.Op {
		case opDeposit:
			if req.Amount <= 0 {
				req.Reply <- AccountResponse{Balance: balance, Error: "deposit amount must be positive"}
				continue
			}
			balance += req.Amount
			req.Reply <- AccountResponse{Balance: balance}
		case opWithdraw:
			if req.Amount > balance {
				req.Reply <- AccountResponse{
					Balance: balance,
					Error:   fmt.Sprintf("insufficient funds: have $%.2f, want $%.2f", balance, req.Amount),
				}
				continue
			}
			balance -= req.Amount
			req.Reply <- AccountResponse{Balance: balance}
		case opBalance:
			req.Reply <- AccountResponse{Balance: balance}
		}
	}
}

func (svc *AccountService) send(op string, amount float64) AccountResponse {
	reply := make(chan AccountResponse, 1)
	svc.requests <- AccountRequest{Op: op, Amount: amount, Reply: reply}
	return <-reply
}

// Deposit adds funds to the account.
func (svc *AccountService) Deposit(amount float64) AccountResponse {
	return svc.send(opDeposit, amount)
}

// Withdraw removes funds from the account.
func (svc *AccountService) Withdraw(amount float64) AccountResponse {
	return svc.send(opWithdraw, amount)
}

// Balance returns the current balance.
func (svc *AccountService) Balance() AccountResponse {
	return svc.send(opBalance, 0)
}

// Close shuts down the service.
func (svc *AccountService) Close() {
	close(svc.requests)
}

func main() {
	account := NewAccountService()
	var wg sync.WaitGroup

	for i := 0; i < depositorCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			resp := account.Deposit(depositAmount)
			fmt.Printf("Client %d: deposited $%.0f, balance: $%.2f\n",
				id, depositAmount, resp.Balance)
		}(i)
	}

	for i := depositorCount; i < depositorCount+withdrawerCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			resp := account.Withdraw(withdrawAmount)
			if resp.Error != "" {
				fmt.Printf("Client %d: withdraw FAILED: %s\n", id, resp.Error)
			} else {
				fmt.Printf("Client %d: withdrew $%.0f, balance: $%.2f\n",
					id, withdrawAmount, resp.Balance)
			}
		}(i)
	}

	wg.Wait()
	resp := account.Balance()
	fmt.Printf("Final balance: $%.2f\n", resp.Balance)
	account.Close()
}
```

### Verification
```bash
go run main.go
# Expected: deposits and withdrawals with correct balance tracking
# Some withdrawals may fail with "insufficient funds"
# Final balance reflects all successful operations
```

## Verification

Run the programs and confirm:
1. Each client receives the correct response via its private reply channel
2. The service goroutine processes requests without any mutex or shared state
3. Multiple concurrent clients get correct, non-mixed responses
4. Error cases are communicated through the response struct

## Common Mistakes

### Forgetting to Create the Reply Channel

**Wrong:**
```go
req := LookupRequest{UserID: 42}
requests <- req
// req.Reply is nil -- service blocks forever trying to send to nil channel
```

**What happens:** The service tries to send to a nil channel, which blocks forever. The service goroutine is stuck and cannot process any further requests.

**Fix:** Always initialize the Reply channel: `Reply: make(chan LookupResponse, 1)`.

### Unbuffered Reply Channel Blocking the Service

**Wrong:**
```go
reply := make(chan string) // unbuffered
requests <- LookupRequest{UserID: 1, Reply: reply}
// If the client does not receive promptly, the service blocks
```

**What happens:** The service goroutine is stuck waiting for the client to receive. It cannot process other requests from other clients.

**Fix:** Use `make(chan string, 1)` for the reply channel. The service can send and immediately move to the next request.

## Review

The pattern hangs on a single structural decision: the reply channel lives
inside the request. Because each requester makes its own private
`make(chan T, 1)` and hands it over, responses route back to exactly the caller
that asked, even when dozens of clients hit the shared request channel at once.
The service goroutine is the sole owner of the state -- the user map, the config
store, the rate-limit counters, the account balance -- and it processes requests
one at a time in its `run` loop, so there is no shared memory to protect and no
mutex to forget. The buffered reply channel matters too: capacity one lets the
service send the response and immediately move to the next request instead of
blocking until the client happens to receive. This is Go's "share memory by
communicating" made concrete, and it generalizes cleanly to config services,
rate limiters, and account systems.

Make sure you can answer the three questions the pattern raises. Why does each
client need its own reply channel rather than a shared one? Why is
`make(chan T, 1)` preferred over an unbuffered `make(chan T)` for that reply?
And how does confining all state to one goroutine eliminate the need for a mutex
entirely? If those answers are clear, you can build the request-response service
from an empty file.

## Resources
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the proverb behind this pattern, with runnable examples.
- [Effective Go: Channels of channels](https://go.dev/doc/effective_go#chan_of_chan) -- the language guide's treatment of embedding a channel in a request.
- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- the talk that popularized request-response and service goroutines.

---

Back to [Concurrency](../../concurrency.md) | Next: [09-buffered-channel-as-semaphore](../09-buffered-channel-as-semaphore/09-buffered-channel-as-semaphore.md)
