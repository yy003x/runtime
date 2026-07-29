// Command releasefileserver serves a local release fixture for release-check.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	root := flag.String("root", "", "regular release fixture directory")
	addressFile := flag.String(
		"address-file", "", "private file that receives the listen address",
	)
	flag.Parse()
	if strings.TrimSpace(*root) == "" ||
		strings.TrimSpace(*addressFile) == "" {
		fatal(fmt.Errorf("--root and --address-file are required"))
	}
	info, err := os.Lstat(*root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		fatal(fmt.Errorf("--root must be a directory, not a symlink"))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatal(err)
	}
	defer listener.Close()
	if err := os.MkdirAll(filepath.Dir(*addressFile), 0o700); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(
		*addressFile, []byte(listener.Addr().String()+"\n"), 0o600,
	); err != nil {
		fatal(err)
	}
	server := &http.Server{
		Handler:           http.FileServer(http.Dir(*root)),
		ReadHeaderTimeout: 5_000_000_000,
	}
	if err := server.Serve(listener); err != nil &&
		err != http.ErrServerClosed {
		fatal(err)
	}
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
