package main

import (
	"os"

	"github.com/yy003x/runtime/internal/interfaces/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
