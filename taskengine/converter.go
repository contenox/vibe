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
	case DataTypeOpenAIChat:
		return convertToOpenAIChatRequest(value)
	case DataTypeOpenAIChatResponse:
		return convertToOpenAIChatResponse(value)
	case DataTypeSearchResults:
		return convertToSearchResults(value)
	case DataTypeString:
		return convertToString(value)
	case DataTypeBool:
		return convertToBool(value)
	case DataTypeInt:
		return convertToInt(value)
	case DataTypeFloat:
		return convertToFloat(value)
	case DataTypeVector:
		return convertToFloatSlice(value)
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
	case OpenAIChatRequest:
		return DataTypeOpenAIChat
	case OpenAIChatResponse:
		return DataTypeOpenAIChatResponse
	case []SearchResult:
		return DataTypeSearchResults
	case []float64:
		return DataTypeVector
	case string, []byte, json.RawMessage:
		return DataTypeString
	case bool:
		return DataTypeBool
	case int, int8, int16, int32, int64:
		return DataTypeInt
	case uint, uint8, uint16, uint32, uint64, uintptr:
		return DataTypeInt
	case float32, float64:
		return DataTypeFloat
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
	case reflect.Bool:
		return DataTypeBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return DataTypeInt
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return DataTypeInt
	case reflect.Float32, reflect.Float64:
		return DataTypeFloat
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

func convertToOpenAIChatRequest(value interface{}) (OpenAIChatRequest, error) {
	switch v := value.(type) {
	case OpenAIChatRequest:
		return v, nil
	case map[string]interface{}:
		data, err := json.Marshal(v)
		if err != nil {
			return OpenAIChatRequest{}, err
		}
		var req OpenAIChatRequest
		err = json.Unmarshal(data, &req)
		return req, err
	default:
		return OpenAIChatRequest{}, fmt.Errorf("cannot convert %T to OpenAIChatRequest", value)
	}
}

func convertToOpenAIChatResponse(value interface{}) (OpenAIChatResponse, error) {
	switch v := value.(type) {
	case OpenAIChatResponse:
		return v, nil
	case map[string]interface{}:
		data, err := json.Marshal(v)
		if err != nil {
			return OpenAIChatResponse{}, err
		}
		var resp OpenAIChatResponse
		err = json.Unmarshal(data, &resp)
		return resp, err
	default:
		return OpenAIChatResponse{}, fmt.Errorf("cannot convert %T to OpenAIChatResponse", value)
	}
}

func convertToSearchResults(value interface{}) ([]SearchResult, error) {
	switch v := value.(type) {
	case []SearchResult:
		return v, nil
	case []interface{}:
		results := make([]SearchResult, len(v))
		for i, item := range v {
			if sr, ok := item.(SearchResult); ok {
				results[i] = sr
			} else if m, ok := item.(map[string]interface{}); ok {
				data, err := json.Marshal(m)
				if err != nil {
					return nil, err
				}
				var sr SearchResult
				if err := json.Unmarshal(data, &sr); err != nil {
					return nil, err
				}
				results[i] = sr
			} else {
				return nil, fmt.Errorf("invalid search result type: %T", item)
			}
		}
		return results, nil
	default:
		return nil, fmt.Errorf("cannot convert %T to []SearchResult", value)
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

func convertToBool(value interface{}) (bool, error) {
	switch v := value.(type) {
	case bool:
		return v, nil
	case string:
		return strconv.ParseBool(v)
	case int, float64:
		return v != 0, nil
	default:
		return false, fmt.Errorf("cannot convert %T to bool", value)
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

func convertToFloat(value interface{}) (float64, error) {
	switch v := value.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case string:
		return strconv.ParseFloat(v, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float", value)
	}
}

func convertToFloatSlice(value interface{}) ([]float64, error) {
	switch v := value.(type) {
	case []float64:
		return v, nil
	case []interface{}:
		floats := make([]float64, len(v))
		for i, item := range v {
			if f, ok := item.(float64); ok {
				floats[i] = f
			} else {
				return nil, fmt.Errorf("element %d is %T, not float64", i, item)
			}
		}
		return floats, nil
	default:
		return nil, fmt.Errorf("cannot convert %T to []float64", value)
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
