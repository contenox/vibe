package schema

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"reflect"
	"sort"
	"strconv"
	"strings"

	commenttext "github.com/contenox/contenox/tools/openapi-gen/internal/comment"
	"github.com/getkin/kin-openapi/openapi3"
)

// AddTypes walks parsed packages and emits component schemas for all struct types.
func AddTypes(swagger *openapi3.T, pkgs map[string]*ast.Package) {
	ensureComponents(swagger)

	pkgNames := make([]string, 0, len(pkgs))
	for name := range pkgs {
		pkgNames = append(pkgNames, name)
	}
	sort.Strings(pkgNames)

	for _, pkgName := range pkgNames {
		pkg := pkgs[pkgName]
		filePaths := make([]string, 0, len(pkg.Files))
		for path := range pkg.Files {
			filePaths = append(filePaths, path)
		}
		sort.Strings(filePaths)

		for _, path := range filePaths {
			file := pkg.Files[path]
			filePkgName := file.Name.Name

			ast.Inspect(file, func(n ast.Node) bool {
				typeSpec, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				switch typeNode := typeSpec.Type.(type) {
				case *ast.StructType:
					addStructSchema(swagger, filePkgName, typeSpec.Name.Name, typeNode, commenttext.Text(typeSpec.Doc))
				case *ast.Ident:
					if isPrimitiveType(typeNode.Name) {
						addAliasSchema(swagger, filePkgName, typeSpec.Name.Name, typeNode.Name, commenttext.Text(typeSpec.Doc))
					}
				case *ast.SelectorExpr:
					if isPrimitiveType(typeNode.Sel.Name) {
						addAliasSchema(swagger, filePkgName, typeSpec.Name.Name, typeNode.Sel.Name, commenttext.Text(typeSpec.Doc))
					}
				}
				return true
			})
		}
	}
}

func addAliasSchema(swagger *openapi3.T, pkgName, typeName, primitiveType, description string) {
	ensureComponents(swagger)

	fullName := pkgName + "_" + typeName
	if _, exists := swagger.Components.Schemas[fullName]; exists {
		return
	}

	aliasSchema := openapi3.NewSchema()
	aliasSchema.Type = goTypeToSwaggerType(primitiveType)
	aliasSchema.Format = goTypeToSwaggerFormat(primitiveType)
	aliasSchema.Description = description
	swagger.Components.Schemas[fullName] = &openapi3.SchemaRef{Value: aliasSchema}

	arrayName := "array_" + fullName
	if _, exists := swagger.Components.Schemas[arrayName]; !exists {
		arraySchema := openapi3.NewSchema()
		arraySchema.Type = &openapi3.Types{openapi3.TypeArray}
		arraySchema.Items = &openapi3.SchemaRef{Ref: fmt.Sprintf("#/components/schemas/%s", fullName)}
		swagger.Components.Schemas[arrayName] = openapi3.NewSchemaRef("", arraySchema)
	}
}

func addStructSchema(swagger *openapi3.T, pkgName, typeName string, structType *ast.StructType, description string) {
	ensureComponents(swagger)

	fullName := pkgName + "_" + typeName
	if _, exists := swagger.Components.Schemas[fullName]; exists {
		return
	}

	schema := openapi3.NewSchema()
	schema.Type = &openapi3.Types{openapi3.TypeObject}
	schema.Properties = make(openapi3.Schemas)
	schema.Description = description

	for _, field := range structType.Fields.List {
		fieldName := ""
		if len(field.Names) > 0 {
			fieldName = field.Names[0].Name
			if !ast.IsExported(fieldName) {
				continue
			}
		}

		if fieldName == "" {
			if ident, ok := field.Type.(*ast.Ident); ok {
				if !ast.IsExported(ident.Name) {
					continue
				}
				if obj := ident.Obj; obj != nil {
					if spec, ok := obj.Decl.(*ast.TypeSpec); ok {
						if st, ok := spec.Type.(*ast.StructType); ok {
							addStructSchema(swagger, pkgName, ident.Name, st, commenttext.Text(spec.Doc))
						}
					}
				}
			}
			continue
		}

		jsonTag := ""
		isOmitempty := false
		if field.Tag != nil {
			tag := strings.Trim(field.Tag.Value, "`")
			if jsonTagVal, hasJSON := reflect.StructTag(tag).Lookup("json"); hasJSON {
				parts := strings.Split(jsonTagVal, ",")
				jsonTag = parts[0]
				if len(parts) > 1 && parts[1] == "omitempty" {
					isOmitempty = true
				}
			}
		}

		if jsonTag == "" {
			jsonTag = fieldName
		} else if jsonTag == "-" {
			continue
		}

		if !isOmitempty && jsonTag != "" {
			schema.Required = append(schema.Required, jsonTag)
		}

		fieldSchema := openapi3.NewSchema()
		fieldSchemaRef := &openapi3.SchemaRef{Value: fieldSchema}
		isRef := false

		if doc := commenttext.Text(field.Doc); doc != "" {
			fieldSchema.Description = doc
		} else if c := commenttext.Text(field.Comment); c != "" {
			fieldSchema.Description = c
		}

		hasOverride := false
		if field.Tag != nil {
			tag := strings.Trim(field.Tag.Value, "`")
			if strings.Contains(tag, "openapi_include_type:") {
				if includeStart := strings.Index(tag, `openapi_include_type:"`); includeStart != -1 {
					includeStart += len(`openapi_include_type:"`)
					includeEnd := strings.Index(tag[includeStart:], `"`)
					if includeEnd != -1 {
						overrideType := tag[includeStart : includeStart+includeEnd]
						if strings.HasPrefix(overrideType, "[]") {
							fieldSchema.Type = &openapi3.Types{openapi3.TypeArray}
							itemType := strings.TrimPrefix(overrideType, "[]")
							itemType = strings.TrimPrefix(itemType, "*")
							itemType = strings.ReplaceAll(itemType, ".", "_")
							if isPrimitiveType(itemType) {
								fieldSchema.Items = &openapi3.SchemaRef{Value: &openapi3.Schema{
									Type:   goTypeToSwaggerType(itemType),
									Format: goTypeToSwaggerFormat(itemType),
								}}
							} else {
								fieldSchema.Items = &openapi3.SchemaRef{Ref: fmt.Sprintf("#/components/schemas/%s", itemType)}
							}
						} else {
							overrideType = strings.TrimPrefix(overrideType, "*")
							overrideType = strings.ReplaceAll(overrideType, ".", "_")
							if isPrimitiveType(overrideType) {
								fieldSchema.Type = goTypeToSwaggerType(overrideType)
								fieldSchema.Format = goTypeToSwaggerFormat(overrideType)
							} else {
								fieldSchemaRef.Ref = fmt.Sprintf("#/components/schemas/%s", overrideType)
								isRef = true
							}
						}
						hasOverride = true
					}
				}
			}
		}

		if field.Type != nil {
			var typeName string
			if starExpr, ok := field.Type.(*ast.StarExpr); ok {
				if ident, ok := starExpr.X.(*ast.Ident); ok {
					typeName = ident.Name
				}
			} else if ident, ok := field.Type.(*ast.Ident); ok {
				typeName = ident.Name
			} else if selExpr, ok := field.Type.(*ast.SelectorExpr); ok {
				typeName = selExpr.Sel.Name
			}

			if typeName == "RawMessage" {
				fieldSchema.Type = &openapi3.Types{openapi3.TypeString}
				fieldSchema.Format = "json"
				fieldSchema.Description = "JSON-encoded string"
				hasOverride = true
			}
		}

		if !hasOverride {
			switch fieldType := field.Type.(type) {
			case *ast.Ident:
				if isPrimitiveType(fieldType.Name) {
					fieldSchema.Type = goTypeToSwaggerType(fieldType.Name)
					fieldSchema.Format = goTypeToSwaggerFormat(fieldType.Name)
				} else {
					fieldSchemaRef.Ref = fmt.Sprintf("#/components/schemas/%s_%s", pkgName, fieldType.Name)
					isRef = true
				}
			case *ast.SelectorExpr:
				typeName := fieldType.Sel.Name
				if isPrimitiveType(typeName) {
					fieldSchema.Type = goTypeToSwaggerType(typeName)
					fieldSchema.Format = goTypeToSwaggerFormat(typeName)
				} else if pkgIdent, ok := fieldType.X.(*ast.Ident); ok {
					fieldSchemaRef.Ref = fmt.Sprintf("#/components/schemas/%s_%s", pkgIdent.Name, typeName)
					isRef = true
				}
			case *ast.ArrayType:
				fieldSchema.Type = &openapi3.Types{openapi3.TypeArray}
				fieldSchema.Items = schemaRefForFieldType(pkgName, fieldType.Elt)
				if fieldSchema.Items == nil {
					fieldSchema.Items = &openapi3.SchemaRef{Value: openapi3.NewObjectSchema()}
				}
			case *ast.StarExpr:
				ref := schemaRefForFieldType(pkgName, fieldType.X)
				if ref != nil {
					if ref.Ref != "" {
						fieldSchemaRef.Ref = ref.Ref
						isRef = true
					} else if ref.Value != nil {
						fieldSchema.Type = ref.Value.Type
						fieldSchema.Format = ref.Value.Format
						fieldSchema.Description = ref.Value.Description
						fieldSchema.Example = ref.Value.Example
					}
				}
			case *ast.MapType:
				has := true
				fieldSchema.Type = &openapi3.Types{openapi3.TypeObject}
				fieldSchema.AdditionalProperties = openapi3.AdditionalProperties{
					Has:    &has,
					Schema: &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}}},
				}
			}
		}

		if field.Tag != nil {
			tag := strings.Trim(field.Tag.Value, "`")
			if strings.Contains(tag, `example:"`) {
				var goTypeName string
				switch ft := field.Type.(type) {
				case *ast.Ident:
					goTypeName = ft.Name
				case *ast.SelectorExpr:
					goTypeName = ft.Sel.Name
				case *ast.ArrayType:
					switch elem := ft.Elt.(type) {
					case *ast.Ident:
						goTypeName = "[]" + elem.Name
					case *ast.SelectorExpr:
						goTypeName = "[]" + elem.Sel.Name
					}
				case *ast.MapType:
					goTypeName = "map"
				}
				if exampleStr, found := extractExampleValue(tag); found && !isRef {
					if exampleValue := convertExampleToType(exampleStr, goTypeName); exampleValue != nil {
						fieldSchema.Example = exampleValue
					}
				}
			}
		}

		if isRef {
			schema.Properties[jsonTag] = fieldSchemaRef
		} else {
			schema.Properties[jsonTag] = &openapi3.SchemaRef{Value: fieldSchema}
		}
	}

	swagger.Components.Schemas[fullName] = &openapi3.SchemaRef{Value: schema}
	arrayName := "array_" + fullName
	if _, exists := swagger.Components.Schemas[arrayName]; !exists {
		arraySchema := openapi3.NewSchema()
		arraySchema.Type = &openapi3.Types{openapi3.TypeArray}
		arraySchema.Items = &openapi3.SchemaRef{Ref: fmt.Sprintf("#/components/schemas/%s", fullName)}
		swagger.Components.Schemas[arrayName] = openapi3.NewSchemaRef("", arraySchema)
	}
}

func isPrimitiveType(typeName string) bool {
	switch typeName {
	case "string",
		"int", "int32", "int64",
		"float32", "float64",
		"bool",
		"Time", "Duration", "time.Time", "time.Duration",
		"any", "interface{}":
		return true
	default:
		return false
	}
}

func goTypeToSwaggerType(goType string) *openapi3.Types {
	switch goType {
	case "string":
		return &openapi3.Types{openapi3.TypeString}
	case "int", "int32", "int64", "Duration":
		return &openapi3.Types{openapi3.TypeInteger}
	case "float32", "float64":
		return &openapi3.Types{openapi3.TypeNumber}
	case "bool":
		return &openapi3.Types{openapi3.TypeBoolean}
	case "Time":
		return &openapi3.Types{openapi3.TypeString}
	default:
		return &openapi3.Types{openapi3.TypeObject}
	}
}

func goTypeToSwaggerFormat(goType string) string {
	switch goType {
	case "Time":
		return "date-time"
	case "Duration":
		return "nanoseconds"
	default:
		return ""
	}
}

func schemaRefForFieldType(pkgName string, expr ast.Expr) *openapi3.SchemaRef {
	switch t := expr.(type) {
	case *ast.Ident:
		if isPrimitiveType(t.Name) {
			return &openapi3.SchemaRef{Value: &openapi3.Schema{
				Type:   goTypeToSwaggerType(t.Name),
				Format: goTypeToSwaggerFormat(t.Name),
			}}
		}
		return &openapi3.SchemaRef{Ref: fmt.Sprintf("#/components/schemas/%s_%s", pkgName, t.Name)}
	case *ast.SelectorExpr:
		typeName := t.Sel.Name
		if isPrimitiveType(typeName) {
			return &openapi3.SchemaRef{Value: &openapi3.Schema{
				Type:   goTypeToSwaggerType(typeName),
				Format: goTypeToSwaggerFormat(typeName),
			}}
		}
		if pkgIdent, ok := t.X.(*ast.Ident); ok {
			return &openapi3.SchemaRef{Ref: fmt.Sprintf("#/components/schemas/%s_%s", pkgIdent.Name, typeName)}
		}
	case *ast.StarExpr:
		return schemaRefForFieldType(pkgName, t.X)
	}
	return nil
}

func convertExampleToType(exampleStr, goTypeName string) interface{} {
	if goTypeName == "bool" {
		if exampleStr == "true" {
			return true
		}
		if exampleStr == "false" {
			return false
		}
	}

	if goTypeName == "int" || goTypeName == "int8" || goTypeName == "int16" || goTypeName == "int32" || goTypeName == "int64" ||
		goTypeName == "uint" || goTypeName == "uint8" || goTypeName == "uint16" || goTypeName == "uint32" || goTypeName == "uint64" ||
		goTypeName == "Duration" {
		if val, err := strconv.ParseInt(exampleStr, 10, 64); err == nil {
			return val
		}
	}

	if goTypeName == "float32" || goTypeName == "float64" {
		if val, err := strconv.ParseFloat(exampleStr, 64); err == nil {
			return val
		}
	}

	if strings.HasPrefix(goTypeName, "[]") {
		var result []interface{}
		if err := json.Unmarshal([]byte(exampleStr), &result); err == nil {
			return result
		}
		return nil
	}

	if goTypeName == "any" || goTypeName == "interface{}" {
		var result interface{}
		if err := json.Unmarshal([]byte(exampleStr), &result); err == nil {
			return result
		}
		return nil
	}

	if goTypeName == "map" {
		var result interface{}
		if err := json.Unmarshal([]byte(exampleStr), &result); err == nil {
			return result
		}
		return nil
	}

	return exampleStr
}

func extractExampleValue(tag string) (string, bool) {
	if exampleStart := strings.Index(tag, `example:"`); exampleStart != -1 {
		exampleStart += len(`example:"`)
		inEscape := false
		for i, c := range tag[exampleStart:] {
			if c == '\\' {
				inEscape = !inEscape
				continue
			}
			if c == '"' && !inEscape {
				return tag[exampleStart : exampleStart+i], true
			}
			inEscape = false
		}
	}
	return "", false
}
