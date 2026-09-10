package cli

import (
	"testing"

	"github.com/yy003x/runtime/pkg/model"
)

func TestCallParamPreservesJSONAndLastValue(t *testing.T) {
	request, output, err := parseDirectModelInput("api", model.Profile{}, []string{
		"--param", `reasoning={"effort":"low","summary":null}`,
		"--param", "frequency_penalty=1", "--param", "frequency_penalty=0",
		"--param", "metadata=null", "--param", "parallel_tool_calls=false",
		"--param", "seed=9007199254740993", "--param", `prompt_cache_key="a=b"`,
		"--raw", "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !output.Raw || request.Input.Messages[0].Content != "hello" {
		t.Fatalf("unexpected request or output: %+v %+v", request, output)
	}
	for name, want := range map[string]string{
		"reasoning": `{"effort":"low","summary":null}`, "frequency_penalty": "0",
		"metadata": "null", "parallel_tool_calls": "false", "seed": "9007199254740993",
		"prompt_cache_key": `"a=b"`,
	} {
		if got := string(request.Input.Options.ExtraBody[name]); got != want {
			t.Errorf("%s=%s, want %s", name, got, want)
		}
	}
}

func TestCallParamRejectsMalformedAndOwnedFields(t *testing.T) {
	for _, argument := range []string{
		"metadata", "=null", "metadata=", "metadata={", "metadata=null false",
		`metadata={"duplicate":0,"duplicate":1}`, "model=null", "stream=false",
		`previous_response_id="resp_fixture"`, "reasoning_effort=high",
	} {
		t.Run(argument, func(t *testing.T) {
			if _, _, err := parseDirectModelInput("api", model.Profile{}, []string{"--param", argument, "hello"}); err == nil {
				t.Fatal("invalid parameter accepted")
			}
		})
	}
	for _, args := range [][]string{
		{"--param"},
		{"--param", "metadata=null", "--request-file", "/does-not-exist.json"},
	} {
		if _, _, err := parseDirectModelInput("api", model.Profile{}, args); err == nil {
			t.Fatal("invalid combination accepted")
		}
	}
}
