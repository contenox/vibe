package schema

import (
	"github.com/getkin/kin-openapi/openapi3"
)

// CleanUnused removes component schemas not referenced from operations (transitive closure).
func CleanUnused(swagger *openapi3.T) {
	if swagger.Components == nil || swagger.Components.Schemas == nil {
		return
	}

	directlyUsed := make(map[string]bool)
	for _, pathItem := range swagger.Paths.Map() {
		for _, op := range []*openapi3.Operation{
			pathItem.Get, pathItem.Put, pathItem.Post,
			pathItem.Delete, pathItem.Options, pathItem.Head,
			pathItem.Patch, pathItem.Trace,
		} {
			if op == nil {
				continue
			}
			collectDirectRefs(op, directlyUsed)
		}
	}

	usedSchemas := make(map[string]bool)
	queue := make([]string, 0, len(directlyUsed))
	for name := range directlyUsed {
		usedSchemas[name] = true
		queue = append(queue, name)
	}

	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]

		schemaRef, exists := swagger.Components.Schemas[name]
		if !exists {
			continue
		}

		for _, ref := range findRefsInSchema(schemaRef) {
			if !usedSchemas[ref] {
				usedSchemas[ref] = true
				queue = append(queue, ref)
			}
		}
	}

	cleaned := openapi3.Schemas{}
	for name, s := range swagger.Components.Schemas {
		if usedSchemas[name] {
			cleaned[name] = s
		}
	}
	swagger.Components.Schemas = cleaned
}

func collectDirectRefs(op *openapi3.Operation, refs map[string]bool) {
	if op.RequestBody != nil && op.RequestBody.Value != nil {
		for _, mediaType := range op.RequestBody.Value.Content {
			if mediaType.Schema != nil && mediaType.Schema.Ref != "" {
				if name := getSchemaNameFromRef(mediaType.Schema.Ref); name != "" {
					refs[name] = true
				}
			}
		}
	}

	for _, response := range op.Responses.Map() {
		if response.Value == nil {
			continue
		}
		for _, mediaType := range response.Value.Content {
			if mediaType.Schema != nil && mediaType.Schema.Ref != "" {
				if name := getSchemaNameFromRef(mediaType.Schema.Ref); name != "" {
					refs[name] = true
				}
			}
		}
	}
}

func findRefsInSchema(schemaRef *openapi3.SchemaRef) []string {
	var refs []string

	if schemaRef.Ref != "" {
		if name := getSchemaNameFromRef(schemaRef.Ref); name != "" {
			refs = append(refs, name)
		}
		return refs
	}

	if schemaRef.Value == nil {
		return refs
	}

	if schemaRef.Value.Items != nil {
		refs = append(refs, findRefsInSchema(schemaRef.Value.Items)...)
	}

	for _, propRef := range schemaRef.Value.Properties {
		refs = append(refs, findRefsInSchema(propRef)...)
	}

	if schemaRef.Value.AdditionalProperties.Schema != nil {
		refs = append(refs, findRefsInSchema(schemaRef.Value.AdditionalProperties.Schema)...)
	}

	for _, subRef := range schemaRef.Value.AllOf {
		refs = append(refs, findRefsInSchema(subRef)...)
	}
	for _, subRef := range schemaRef.Value.AnyOf {
		refs = append(refs, findRefsInSchema(subRef)...)
	}
	for _, subRef := range schemaRef.Value.OneOf {
		refs = append(refs, findRefsInSchema(subRef)...)
	}

	return refs
}
