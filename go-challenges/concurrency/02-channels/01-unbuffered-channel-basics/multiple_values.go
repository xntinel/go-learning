package main

import "fmt"

func main() {
	ch := make(chan int)
	go func(){
		ch <- 10
		ch <- 20
		ch <- 30
	}()

	for i:=0; i<3;i++{
		val:=<-ch
		fmt.Println("Received:",val)
	}

}
