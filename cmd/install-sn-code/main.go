package main

import (
	"fmt"
	"os"

	"github.com/yy003x/sn-code/internal/application/activation"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: install-sn-code <binary> <runtime-home> <command-dir>")
		os.Exit(2)
	}
	result, err := activation.InstallCodeBinary(os.Args[1], os.Args[2], os.Args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, "sn-code install:", err)
		os.Exit(1)
	}
	fmt.Print(result)
}
