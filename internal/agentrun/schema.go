package agentrun

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

type contractSchema struct {
	Required   []string                  `yaml:"required"`
	Properties map[string]propertySchema `yaml:"properties"`
}

type propertySchema struct {
	Type string `yaml:"type"`
	Enum []any  `yaml:"enum"`
}

func validateResultSchema(resultFile, schemaRef string) error {
	if schemaRef == "" || schemaRef == "result" || schemaRef == "builtin:result" {
		return nil
	}
	schemaData, err := os.ReadFile(schemaRef)
	if err != nil {
		return fmt.Errorf("read result schema: %w", err)
	}
	var schema contractSchema
	if err := yaml.Unmarshal(schemaData, &schema); err != nil {
		return fmt.Errorf("parse result schema: %w", err)
	}
	resultData, err := os.ReadFile(resultFile)
	if err != nil {
		return err
	}
	var result map[string]any
	if err := json.Unmarshal(resultData, &result); err != nil {
		return err
	}
	for _, key := range schema.Required {
		if _, ok := result[key]; !ok {
			return fmt.Errorf("缺少必填字段: %s", key)
		}
	}
	for key, property := range schema.Properties {
		value, ok := result[key]
		if !ok {
			continue
		}
		if property.Type != "" && !matchesType(value, property.Type) {
			return fmt.Errorf("%s 类型不匹配: 期望 %s", key, property.Type)
		}
		if len(property.Enum) > 0 {
			matched := false
			for _, allowed := range property.Enum {
				if reflect.DeepEqual(value, allowed) || fmt.Sprint(value) == fmt.Sprint(allowed) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("%s 不在允许枚举内: %v", key, value)
			}
		}
	}
	return nil
}

func matchesType(value any, expected string) bool {
	switch strings.ToLower(expected) {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		number, ok := value.(float64)
		return ok && number == float64(int(number))
	case "number":
		_, ok := value.(float64)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return true
	}
}
