package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yy003x/runtime/internal/agentrun"
	"github.com/yy003x/runtime/internal/cli/config"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
	"github.com/yy003x/runtime/runtimeapi"
)

const maxLLMRequestFileBytes int64 = 1 << 20

func runLLMNamespace(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: llm generate --request-file <path|-> [--stream]")
	}
	if args[0] != "generate" {
		return fmt.Errorf("unknown llm action: %s", args[0])
	}
	requestFile := ""
	stream := false
	for index := 1; index < len(args); index++ {
		switch args[index] {
		case "-h", "--help":
			fmt.Println(`sn-cli llm generate - execute a structured local LLM Runtime request

Usage:
  sn-cli llm generate --request-file <path|->
  sn-cli llm generate --request-file <path|-> --stream

The request file uses the same runtimeapi.Request JSON contract as the Go SDK
and POST /v1/llm/generate. --stream writes one runtimeapi.Event JSON per line.`)
			return nil
		case "--request-file":
			index++
			if index >= len(args) {
				return fmt.Errorf("--request-file requires value")
			}
			requestFile = args[index]
		case "--stream":
			stream = true
		default:
			return fmt.Errorf("unknown llm generate argument: %s", args[index])
		}
	}
	if strings.TrimSpace(requestFile) == "" {
		return fmt.Errorf("llm generate requires --request-file")
	}
	request, err := readLLMRequest(requestFile)
	if err != nil {
		return err
	}
	runtime, err := runtimebootstrap.New(agentrun.New(cfg.Home))
	if err != nil {
		return err
	}
	if !stream {
		response, err := runtime.Generate(context.Background(), request)
		if err != nil {
			return err
		}
		return printJSON(response)
	}
	encoder := json.NewEncoder(os.Stdout)
	_, err = runtime.GenerateStream(context.Background(), request, func(event runtimeapi.Event) error {
		return encoder.Encode(event)
	})
	return err
}

func readLLMRequest(path string) (runtimeapi.Request, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, maxLLMRequestFileBytes+1))
	} else {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return runtimeapi.Request{}, fmt.Errorf("stat LLM request file: %w", statErr)
		}
		if !info.Mode().IsRegular() {
			return runtimeapi.Request{}, fmt.Errorf("LLM request file must be a regular file")
		}
		if info.Size() > maxLLMRequestFileBytes {
			return runtimeapi.Request{}, fmt.Errorf("LLM request file exceeds %d bytes", maxLLMRequestFileBytes)
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return runtimeapi.Request{}, fmt.Errorf("read LLM request file: %w", err)
	}
	if int64(len(data)) > maxLLMRequestFileBytes {
		return runtimeapi.Request{}, fmt.Errorf("LLM request file exceeds %d bytes", maxLLMRequestFileBytes)
	}
	var request runtimeapi.Request
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return runtimeapi.Request{}, fmt.Errorf("decode LLM request file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return runtimeapi.Request{}, fmt.Errorf("LLM request file must contain one JSON object")
		}
		return runtimeapi.Request{}, fmt.Errorf("decode LLM request file: %w", err)
	}
	return request, nil
}
