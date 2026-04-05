package schema

import "github.com/getkin/kin-openapi/openapi3"

// NewDocument returns the base OpenAPI document used for OSS runtime generation.
func NewDocument(version string) *openapi3.T {
	swagger := &openapi3.T{
		OpenAPI: "3.1.0",
		Info: &openapi3.Info{
			Title:   "contenox – LLM Backend Management API",
			Version: version,
		},
		Paths: openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: openapi3.Schemas{},
		},
	}

	swagger.Security = *openapi3.NewSecurityRequirements().
		With(openapi3.SecurityRequirement{"X-API-Key": []string{}})

	swagger.Components.SecuritySchemes = openapi3.SecuritySchemes{
		"X-API-Key": &openapi3.SecuritySchemeRef{
			Value: openapi3.NewSecurityScheme().
				WithType("apiKey").
				WithName("X-API-Key").
				WithIn("header"),
		},
	}

	addErrorResponseSchema(swagger)
	return swagger
}

// Finalize adds common shared schemas and removes unused component schemas.
func Finalize(swagger *openapi3.T) {
	ensureComponents(swagger)
	swagger.Components.Schemas["array_string"] = openapi3.NewSchemaRef("", openapi3.NewArraySchema().WithItems(openapi3.NewStringSchema()))
	CleanUnused(swagger)
}

func ensureComponents(swagger *openapi3.T) {
	if swagger.Components == nil {
		swagger.Components = &openapi3.Components{
			Schemas: openapi3.Schemas{},
		}
	}
	if swagger.Components.Schemas == nil {
		swagger.Components.Schemas = openapi3.Schemas{}
	}
}

func addErrorResponseSchema(swagger *openapi3.T) {
	ensureComponents(swagger)

	errorResponseSchema := openapi3.NewSchema()
	errorResponseSchema.Type = &openapi3.Types{openapi3.TypeObject}
	errorResponseSchema.Properties = openapi3.Schemas{
		"error": &openapi3.SchemaRef{
			Value: &openapi3.Schema{
				Type: &openapi3.Types{openapi3.TypeObject},
				Properties: openapi3.Schemas{
					"message": &openapi3.SchemaRef{
						Value: &openapi3.Schema{
							Type:        &openapi3.Types{openapi3.TypeString},
							Description: "A human-readable error message",
						},
					},
					"type": &openapi3.SchemaRef{
						Value: &openapi3.Schema{
							Type:        &openapi3.Types{openapi3.TypeString},
							Description: "The error type category (e.g., 'invalid_request_error', 'authentication_error')",
						},
					},
					"param": &openapi3.SchemaRef{
						Value: &openapi3.Schema{
							Type:        &openapi3.Types{openapi3.TypeString},
							Description: "The parameter that caused the error, if applicable",
							Nullable:    true,
						},
					},
					"code": &openapi3.SchemaRef{
						Value: &openapi3.Schema{
							Type:        &openapi3.Types{openapi3.TypeString},
							Description: "A specific error code identifier (e.g., 'invalid_parameter_value', 'unauthorized')",
						},
					},
				},
				Required: []string{"message", "type", "code"},
			},
		},
	}
	errorResponseSchema.Required = []string{"error"}

	swagger.Components.Schemas["ErrorResponse"] = &openapi3.SchemaRef{
		Value: errorResponseSchema,
	}
}
