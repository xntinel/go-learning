package main

import (
	"fmt"

	webhook "github.com/sentinel/go-learning/go-solutions/04-functions/01-function-declaration-and-multiple-return-values/05-comma-ok-type-assertion-payload"
)

func main() {
	body := []byte(`{"event":"charge.succeeded","amount":4200,"livemode":true}`)
	payload, err := webhook.Decode(body)
	if err != nil {
		panic(err)
	}
	event, _ := webhook.Field[string](payload, "event")
	amount, _ := webhook.Field[float64](payload, "amount")
	live, _ := webhook.Field[bool](payload, "livemode")
	fmt.Printf("event=%s amount=%.0f live=%t\n", event, amount, live)

	_, ok := webhook.Field[int](payload, "amount")
	fmt.Printf("Field[int](amount) ok=%t\n", ok)

	_, ok = webhook.Field[string](payload, "customer")
	fmt.Printf("Field[string](customer) ok=%t\n", ok)
}
