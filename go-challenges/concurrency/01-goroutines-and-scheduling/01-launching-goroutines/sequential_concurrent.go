package main

import (
	"fmt"
	"time"
	"sync"
)

type ServiceEndpoint struct{
	Name string
	Latency time.Duration
}



func checkService(name string, latency time.Duration) string{
	time.Sleep(latency)
	return fmt.Sprintf("%-18s UP (%v)",name,latency)
}


func runSequentialChecks(services []ServiceEndpoint) time.Duration{
	start := time.Now()
	for _,svc := range services {
		result := checkService(svc.Name,svc.Latency)
		fmt.Printf(" %s\n",result)
	}
	return time.Since(start)
}


func runConcurrentChecks(services []ServiceEndpoint) time.Duration {
	start := time.Now()
	var wg sync.WaitGroup

	for _, svc := range services {
		wg.Add(1)
		go func(name string, latency time.Duration){
			defer wg.Done()
			result:=checkService(name,latency)
			fmt.Printf("%s \n",result)
		}(svc.Name,svc.Latency)
	}
	wg.Wait()
	return time.Since(start)
}


func main() {
	services := []ServiceEndpoint{
		{"auth-api",120*time.Millisecond},
		{"payment-gateway",200*time.Millisecond},
		{"notification-svc",80*time.Millisecond},
		{"investiry-api",150*time.Millisecond},
		{"search-engine",90*time.Millisecond},
	}

	fmt.Println("--- sequential health check")
	seqDuration := runSequentialChecks(services)
	fmt.Printf("Sequential total: %v\n",seqDuration.Round(time.Millisecond))


	fmt.Println("--- concurrent health check")
	concDuration := runConcurrentChecks(services)
	fmt.Printf("concurrent total: %v\n",concDuration.Round(time.Millisecond))

}
