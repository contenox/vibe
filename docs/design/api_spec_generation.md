# OpenAPI Generator Guide

This guide explains how to write and structure Go HTTP handlers and data types so that the custom OpenAPI generator tool can correctly produce a complete and accurate API specification. By following these conventions, the documentation is always in sync with the code.

## 1. Documenting Endpoints (Handlers)

The documentation for each API endpoint is derived directly from the GoDoc comments above its handler function.

### Summary and Description

The generator uses a simple rule:
- The **first line** of the comment block becomes the **summary**
- The **entire comment block** becomes the **description**

To create a clean separation, always write a concise summary on the first line, leave a blank line, and then write the more detailed description.

✅ **Example**
```go
// Create a new backend connection.
//
// Creates a new backend connection to an LLM provider. Backends represent
// connections to LLM services (e.g., Ollama, OpenAI) that can host models.
// Note: Creating a backend will be provisioned on the next synchronization cycle.
func (b *backendManager) createBackend(w http.ResponseWriter, r *http.Request) {
    // ...
}
```

🤖 **Generated OpenAPI**
```yaml
paths:
  /backends:
    post:
      summary: Create a new backend connection.
      description: |-
        Create a new backend connection.

        Creates a new backend connection to an LLM provider. Backends represent
        connections to LLM services (e.g., Ollama, OpenAI) that can host models.
        Note: Creating a backend will be provisioned on the next synchronization cycle.
```

## 2. Documenting Parameters

Parameters are documented in the code by using dedicated helper functions. This makes the documentation explicit and verifiable by the compiler.

### Path Parameters

To document a path parameter, use the `apiframework.GetPathParam` function instead of the standard `r.PathValue()`.

- `name`: The name of the parameter as it appears in the route (e.g., "id")
- `description`: A clear and concise description of the parameter

✅ **Example**
```go
// In the handler
id := apiframework.GetPathParam(r, "id", "The unique identifier for the backend.")
```

🤖 **Generated OpenAPI**
```yaml
parameters:
  - name: id
    in: path
    required: true
    description: The unique identifier for the backend.
    schema:
      type: string
```

### Query Parameters

To document a query parameter, use the `apiframework.GetQueryParam` function.

- `name`: The name of the query parameter (e.g., "limit")
- `defaultValue`: The default value if the parameter is not provided. An empty string "" means no default
- `description`: A clear description of the parameter

✅ **Example**
```go
// In the handler
limitStr := apiframework.GetQueryParam(r, "limit", "100", "The maximum number of items to return.")
cursorStr := apiframework.GetQueryParam(r, "cursor", "", "An optional timestamp for pagination.")
```

🤖 **Generated OpenAPI**
```yaml
parameters:
  - name: limit
    in: query
    required: false
    description: The maximum number of items to return.
    schema:
      type: string
      default: "100"
  - name: cursor
    in: query
    required: false
    description: An optional timestamp for pagination.
    schema:
      type: string
```

## 3. Documenting Request and Response Bodies

The request and response bodies are documented using special inline comments immediately following the `serverops.Decode` and `serverops.Encode` calls.

The format is `// @<type> <package>.<structName>` or `// @<type> []<package>.<structName>`.

### Request Body

Use `// @request` after a call to `serverops.Decode`.

✅ **Example**
```go
// In the handler
backend, err := serverops.Decode[runtimetypes.Backend](r) // @request runtimetypes.Backend
```

### Response Body

Use `// @response` after a call to `serverops.Encode`. This annotation is mandatory for generating the response schema.

✅ **Examples**

**Single Object Response:**
```go
// In the handler
_ = serverops.Encode(w, r, http.StatusOK, backend) // @response runtimetypes.Backend
```

**Slice of Objects Response:**
```go
// In the handler
_ = serverops.Encode(w, r, http.StatusOK, backends) // @response []runtimetypes.Backend
```

**Slice of Pointers to Objects Response:**
```go
// In the handler
_ = serverops.Encode(w, r, http.StatusOK, models) // @response []*runtimetypes.Model
```

**Simple Type Response (e.g., string):**
```go
// In the handler
_ = serverops.Encode(w, r, http.StatusOK, "deleted") // @response string
```

## 4. Documenting Schemas (Structs)

The generator automatically converts exported Go structs into OpenAPI schemas. Detail are added using standard Go comments and struct tags.
In most cases, the generator is able to recursively scan exported structs and their fields to generate a references inside the type schemas.
The Final Schema will be pruned from unused type definitions.

### Field Descriptions and Examples

- **Description**: A standard Go comment directly above a field becomes its description
- **Example**: The `example:"..."` struct tag provides an example value
- **JSON Name**: The `json:"..."` tag is required to define the property name in the OpenAPI spec

✅ **Example**
```go
type BackendRuntimeState struct {
    // ID is the unique identifier for the backend.
    ID string `json:"id" example:"b7d9e1a3-8f0c-4a7d-9b1e-2f3a4b5c6d7e"`

    // Error stores a description of the last encountered error, if any.
    Error string `json:"error,omitempty" example:"connection timeout"`
}
```

🤖 **Generated OpenAPI**
```yaml
statetype_BackendRuntimeState:
  type: object
  properties:
    id:
      type: string
      description: ID is the unique identifier for the backend.
      example: b7d9e1a3-8f0c-4a7d-9b1e-2f3a4b5c6d7e
    error:
      type: string
      description: Error stores a description of the last encountered error, if any.
      example: connection timeout
```

### Documenting Slices of Structs

When a struct contains a slice of another struct, that is located in the same package, provide the `openapi_include_type` tag to tell the generator what the items in the slice are.

✅ **Example**
```go
type backendSummary struct {
    // ...
    PulledModels []statetype.ModelPullStatus `json:"pulledModels" openapi_include_type:"statetype.ModelPullStatus"`
}
```

🤖 **Generated OpenAPI**
```yaml
backendapi_backendSummary:
  type: object
  properties:
    pulledModels:
      type: array
      items:
        $ref: '#/components/schemas/statetype_ModelPullStatus'
```

### Primitive Type Overrides

For primitive types in slices or custom type aliases, the `openapi_include_type` tag can be used to specify the exact type:

```go
type CapturedStateUnit struct {
    InputType  DataType `json:"inputType" example:"string" openapi_include_type:"string"`
    OutputType DataType `json:"outputType" example:"string" openapi_include_type:"string"`
}
```

## 5. Error Responses

The generator automatically includes a standard error response schema for all operations:

🤖 **Generated OpenAPI**
```yaml
ErrorResponse:
  type: object
  required:
    - error
  properties:
    error:
      type: string
```

## 6. Authentication

API key authentication is automatically configured:

🤖 **Generated OpenAPI**
```yaml
components:
  securitySchemes:
    X-API-Key:
      type: apiKey
      name: X-API-Key
      in: header
security:
  - X-API-Key: []
```

## 7. Server-Sent Events (SSE) Endpoints

For SSE endpoints, include "Server-Sent Events" or "streams status updates" in the handler comment:

✅ **Example**
```go
// Get real-time backend status updates.
//
// Server-Sent Events stream of backend status changes and health checks.
func (b *backendManager) streamBackendStatus(w http.ResponseWriter, r *http.Request) {
    // SSE implementation
}
```

## 9. Generating the Documentation

To generate the OpenAPI specification:

```bash
# Generate both JSON and YAML specs
make docs-gen

# Generate Markdown documentation
make docs-markdown

# Set version and commit documentation updates
make set-version
make commit-docs
```

The generated files will be placed in the `docs/` directory:
- `openapi.json` - JSON format OpenAPI spec
- `openapi.yaml` - YAML format OpenAPI spec
- `api-reference.md` - Markdown documentation

By following these conventions, the API documentation is always accurate, complete, and synchronized with the codebase.
