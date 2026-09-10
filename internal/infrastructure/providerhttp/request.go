package providerhttp

import (
	"encoding/json"
	"fmt"

	"github.com/yy003x/runtime/pkg/contract"
)

// MarshalRequest adds native options at the top level of the HTTP request, not
// under an extra_body envelope. No values are defaulted, coerced, or filtered.
func MarshalRequest(body any, extra contract.ExtraBody) ([]byte, error) {
	if err := extra.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(body)
	if err != nil || len(extra) == 0 {
		return encoded, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("Provider request must be a JSON object")
	}
	for name, value := range extra {
		fields[name] = value
	}
	return json.Marshal(fields)
}
