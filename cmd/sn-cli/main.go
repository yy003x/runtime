package main

import (
	"os"

	"agent-runtime/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
