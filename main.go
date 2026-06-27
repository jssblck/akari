// Command akari is the entry point for the akari application.
package main

import (
	"fmt"
	"os"

	"github.com/jssblck/akari/internal/greet"
)

func main() {
	name := "world"
	if len(os.Args) > 1 {
		name = os.Args[1]
	}
	fmt.Println(greet.Greeting(name))
}
