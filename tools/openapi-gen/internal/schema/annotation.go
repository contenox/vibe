package schema

import (
	"fmt"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// RefForAnnotation maps @request / @response type strings (dots already converted to underscores)
// to either an inline schema or a #/components/schemas/… ref.
func RefForAnnotation(typeName string) *openapi3.SchemaRef {
	if openapiType := toOpenAPIType(typeName); openapiType != nil {
		return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: openapiType}}
	}
	switch typeName {
	case "object":
		return &openapi3.SchemaRef{Value: openapi3.NewObjectSchema()}
	case "map[string]string":
		has := true
		return &openapi3.SchemaRef{Value: &openapi3.Schema{
			Type: &openapi3.Types{openapi3.TypeObject},
			AdditionalProperties: openapi3.AdditionalProperties{
				Has:    &has,
				Schema: openapi3.NewStringSchema().NewRef(),
			},
		}}
	default:
		return &openapi3.SchemaRef{Ref: fmt.Sprintf("#/components/schemas/%s", typeName)}
	}
}

func toOpenAPIType(goType string) *openapi3.Types {
	switch goType {
	case "string", "time.Time", "time.Duration":
		return &openapi3.Types{openapi3.TypeString}
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64":
		return &openapi3.Types{openapi3.TypeInteger}
	case "float32", "float64":
		return &openapi3.Types{openapi3.TypeNumber}
	case "bool":
		return &openapi3.Types{openapi3.TypeBoolean}
	case "interface{}", "any":
		return &openapi3.Types{openapi3.TypeObject}
	default:
		return nil
	}
}

func getSchemaNameFromRef(ref string) string {
	if after, ok := strings.CutPrefix(ref, "#/components/schemas/"); ok {
		return after
	}
	return ""
}
