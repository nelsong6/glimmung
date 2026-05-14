package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "glimmung-supervisor is not supported on windows")
	os.Exit(1)
}
