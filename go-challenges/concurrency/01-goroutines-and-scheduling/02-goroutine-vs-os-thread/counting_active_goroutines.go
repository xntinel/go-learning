package main


import (
	"fmt"
	"runtime"
	"time"
)


func main(){
	fmt.Printf("Goroutines at start: %d\n",runtime.NumGoroutine())
	
	done := make(chan struct{})

	for i:=0; i<10; i++{
		go func(){
			<-done
		}()
	}

	time.Sleep(10*time.Millisecond)
	fmt.Printf("After launching 10: %d\n",runtime.NumGoroutine())


	close(done)
	time.Sleep(10*time.Millisecond)
	fmt.Printf("After releasing: %d\n",runtime.NumGoroutine)


}



