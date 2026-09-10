package cli

import (
	"encoding/json"
	"strings"

	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/contract"
)

func addModelParam(options *contract.GenerateOptions, argument string) error {
	name, value, found := strings.Cut(argument, "=")
	if !found || name == "" {
		return cliValidationf("--param requires name=JSON, for example 'metadata=null'")
	}
	var raw json.RawMessage
	if err := strictjson.Decode(strings.NewReader(value), 1<<20, &raw); err != nil {
		return cliValidationf("--param %s: %v", name, err)
	}
	if options.ExtraBody == nil {
		options.ExtraBody = make(contract.ExtraBody)
	}
	options.ExtraBody[name] = raw
	if err := options.ExtraBody.Validate(); err != nil {
		return cliValidation(err)
	}
	return nil
}
