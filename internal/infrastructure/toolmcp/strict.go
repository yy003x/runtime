package toolmcp

import (
	"bytes"

	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
)

func decodeObject(data []byte, target any) error {
	return strictjson.DecodeObjectNoNulls(
		bytes.NewReader(data), int64(len(data)), target,
	)
}
