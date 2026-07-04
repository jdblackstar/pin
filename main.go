package main

import (
	"os"

	"github.com/jdblackstar/pin/internal/pin"
)

func main() {
	os.Exit(pin.RunCLI(os.Args[1:], os.Stdout, os.Stderr))
}
