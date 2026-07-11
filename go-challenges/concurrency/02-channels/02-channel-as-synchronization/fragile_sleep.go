package main


import (
	"fmt"
	"time"
)


func main(){
	worker := func(id int){
		fmt.Printf("worker %d: starting\n",id)
		time.Sleep(time.Duration(id*100)*time.Millisecond)
		fmt.Printf("Woerker %d: done\n",id)
	}
	
	for i:=1;i<3;i++{
		go worker(i)
	}


	time.Sleep(200*time.Millisecond)
	fmt.Println("main: exiting (worker 3 lost!)")

}
