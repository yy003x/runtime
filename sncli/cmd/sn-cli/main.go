package main

import (
	"os"

	"agent-arch/sncli/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
