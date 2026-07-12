package main

import (
	"fmt"
	"time"

	ttlcache "github.com/sentinel/go-learning/go-solutions/03-control-flow/01-if-else-and-init-statements/04-cache-comma-ok-ttl"
)

func main() {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := ttlcache.New[string, string](func() time.Time {
		return clock
	})
	c.Set("session:alice", "token-abc", 30*time.Second)
	if v, ok := c.Get("session:alice"); ok {
		fmt.Printf("before expire: %s (len=%d)\n", v, c.Len())
	}
	clock = clock.Add(time.Minute)
	if _, ok := c.Get("session:alice"); !ok {
		fmt.Printf("after expiry: miss (len=%d)\n", c.Len())
	}
}
