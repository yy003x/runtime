package providerhttp

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
)

func TestMarshalRequestNativeFields(t *testing.T) {
	base := struct {
		Model string `json:"model"`
		Store bool   `json:"store"`
	}{"fixture", false}
	extra := contract.ExtraBody{"store": json.RawMessage(`null`), "frequency_penalty": json.RawMessage(`0`), "parallel_tool_calls": json.RawMessage(`false`), "metadata": json.RawMessage(`{"integer":9007199254740993}`)}
	body, err := MarshalRequest(base, extra)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"model":"fixture"`, `"store":null`, `"frequency_penalty":0`, `"parallel_tool_calls":false`, `9007199254740993`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("missing %s: %s", want, body)
		}
	}
	if bytes.Contains(body, []byte(`"extra_body"`)) {
		t.Fatal("extra_body wrapper leaked onto the wire")
	}
	if _, err := MarshalRequest(base, contract.ExtraBody{"model": json.RawMessage(`"other"`)}); err == nil {
		t.Fatal("identity override accepted")
	}
	if _, err := MarshalRequest(make(chan int), nil); err == nil {
		t.Fatal("unencodable base accepted")
	}
	if _, err := MarshalRequest("not an object", extra); err == nil {
		t.Fatal("non-object base accepted")
	}
	if _, err := MarshalRequest(nil, extra); err == nil {
		t.Fatal("null base accepted")
	}
	without, err := MarshalRequest(base, nil)
	if err != nil || string(without) != `{"model":"fixture","store":false}` {
		t.Fatalf("unchanged encoding: %s, %v", without, err)
	}
}
