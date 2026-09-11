package cli

// takeStreamOverride consumes the next argv token when it is exactly
// "true" or "false". Those values override Profile parameters.stream and
// are independent of the bare --stream flag, which still selects downstream
// NDJSON events.
func takeStreamOverride(args []string, index int) (*bool, int, bool) {
	if index+1 >= len(args) {
		return nil, index, false
	}
	switch args[index+1] {
	case "true":
		value := true
		return &value, index + 1, true
	case "false":
		value := false
		return &value, index + 1, true
	default:
		return nil, index, false
	}
}
