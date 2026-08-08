package main

import (
	"os"

	"simplerouter/internal/simplerouter"
)

func main() {
	os.Exit(simplerouter.Main(os.Args[1:]))
}
