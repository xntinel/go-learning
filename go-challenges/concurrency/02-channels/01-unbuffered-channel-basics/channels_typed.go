package main

import "fmt"

type Point struct{X,Y int}


func main(){
	pointCh := make(chan Point)
	go func(){
		pointCh <- Point{3,4}
	}()
	
	p:=<-pointCh
	fmt.Println("Point received:",p)
	errCh := make(chan error)

	go func(){
		errCh <- fmt.Errorf("something went wrong")
	}()

	err := <- errCh
	fmt.Println("error received:",err)
}






