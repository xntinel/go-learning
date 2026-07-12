package main

import (
	"fmt"

	pipeline "github.com/sentinel/go-learning/go-solutions/06-collections-arrays-slices-and-maps/09-slices-package/01-normalize-pipeline-stage"
)

func main() {
	in := []pipeline.Record{
		{Key: "banana", Value: 1},
		{Key: "Apple", Value: 9},
		{Key: "apple", Value: 2},
		{Key: "cherry", Value: 3},
	}
	out := pipeline.Process(in)
	for _, r := range out {
		fmt.Printf("%s=%d\n", r.Key, r.Value)
	}
	fmt.Printf("input[0] still %s\n", in[0].Key)
}
