package main

import (
	"os"

	"github.com/jdblackstar/pin/app"
)

func main() {
	os.Exit(app.RunCLI(os.Args[1:], os.Stdout, os.Stderr))
}
