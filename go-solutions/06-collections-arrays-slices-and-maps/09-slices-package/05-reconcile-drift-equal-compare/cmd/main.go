package main

import (
	"fmt"

	reconcile "github.com/sentinel/go-learning/go-solutions/06-collections-arrays-slices-and-maps/09-slices-package/05-reconcile-drift-equal-compare"
)

func main() {
	desired := []reconcile.Rule{
		{Name: "web", Target: "10.0.0.1"},
		{Name: "api", Target: "10.0.0.2"},
	}

	actual := []reconcile.Rule{
		{Name: "web", Target: "10.0.0.1", SyncedAt: 1700},
		{Name: "api", Target: "10.0.0.2", SyncedAt: 1700},
	}

	wrote := reconcile.Reconcile(desired, actual, func([]reconcile.Rule) {
		fmt.Println("apply called")
	})
	fmt.Printf("in-sync tick wrote: %v\n", wrote)

	actual[1].Target = "10.9.9.9"
	wrote = reconcile.Reconcile(desired, actual, func([]reconcile.Rule) {
		fmt.Println("apply called")
	})
	fmt.Printf("drifted tick wrote: %v\n", wrote)
	fmt.Printf("order([a b],[a c]) = %d\n", reconcile.Order([]string{"a", "b"}, []string{"a", "c"}))
}
