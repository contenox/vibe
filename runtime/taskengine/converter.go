package taskengine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)


// ConvertToType converts a value to the specified DataType
func ConvertToType(value interface{}, dataType DataType) (interface{}, error) {
	switch dataType {
	case DataTypeChatHistory:
		return convertToChatHistory(value)
	case DataTypeString:
		return convertToString(value)
	case DataTypeInt:
		return convertToInt(value)
	case DataTypeJSON:
		return convertToJSON(value)
	case DataTypeNil:
		return nil, nil
	case DataTypeAny:
		if value == nil {
			return nil, nil
		}
		inferred := InferDataType(value)
		if inferred == DataTypeNil {
			return nil, nil
		}
		return ConvertToType(value, inferred)
	default:
		return value, nil
	}
}

// InferDataType picks the narrowest concrete DataType for a runtime value.
func InferDataType(v any) DataType {
	if v == nil {
		return DataTypeNil
	}
	switch v.(type) {
	case ChatHistory:
		return DataTypeChatHistory
	case string, []byte, json.RawMessage:
		return DataTypeString
	case int, int8, int16, int32, int64:
		return DataTypeInt
	case uint, uint8, uint16, uint32, uint64, uintptr:
		return DataTypeInt
	case map[string]any:
		return DataTypeJSON
	case []any:
		return DataTypeJSON
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array:
		return DataTypeJSON
	case reflect.Struct:
		return DataTypeJSON
	case reflect.Pointer:
		if rv.IsNil() {
			return DataTypeNil
		}
		return InferDataType(rv.Elem().Interface())
	case reflect.String:
		return DataTypeString
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return DataTypeInt
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return DataTypeInt
	default:
		return DataTypeString
	}
}

// NormalizeDataType upgrades DataTypeAny to a concrete type and coerces the value with ConvertToType.
func NormalizeDataType(v any, dt DataType) (any, DataType, error) {
	if dt != DataTypeAny {
		return v, dt, nil
	}
	inferred := InferDataType(v)
	if inferred == DataTypeNil {
		return nil, DataTypeNil, nil
	}
	out, err := ConvertToType(v, inferred)
	if err != nil {
		s, err2 := convertToString(v)
		if err2 != nil {
			return fmt.Sprint(v), DataTypeString, nil
		}
		return s, DataTypeString, nil
	}
	return out, inferred, nil
}

func convertToChatHistory(value interface{}) (ChatHistory, error) {
	switch v := value.(type) {
	case ChatHistory:
		return v, nil
	case map[string]interface{}:
		data, err := json.Marshal(v)
		if err != nil {
			return ChatHistory{}, err
		}
		var hist ChatHistory
		err = json.Unmarshal(data, &hist)
		return hist, err
	default:
		return ChatHistory{}, fmt.Errorf("cannot convert %T to ChatHistory", value)
	}
}

// Basic type conversions
func convertToString(value interface{}) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

func convertToInt(value interface{}) (int, error) {
	switch v := value.(type) {
	case int:
		return v, nil
	case float64:
		return int(v), nil
	case string:
		return strconv.Atoi(v)
	default:
		return 0, fmt.Errorf("cannot convert %T to int", value)
	}
}

func convertToJSON(value interface{}) (interface{}, error) {
	switch v := value.(type) {
	case map[string]interface{}, []interface{}:
		return v, nil
	case string:
		// If it's a string that looks like JSON, try to unmarshal it
		if strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") {
			var result interface{}
			if err := json.Unmarshal([]byte(v), &result); err == nil {
				return result, nil
			}
		}
		return v, nil // Keep as string if not valid JSON
	default:
		// Try to marshal to JSON and back to get proper structure
		data, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		var result interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, err
		}
		return result, nil
	}
}
