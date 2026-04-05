package routes

import (
	"go/ast"
	"go/token"
	"log"
	"sort"
	"strconv"
	"strings"

	commenttext "github.com/contenox/contenox/tools/openapi-gen/internal/comment"
	"github.com/contenox/contenox/tools/openapi-gen/internal/schema"
	"github.com/getkin/kin-openapi/openapi3"
)

// Process extracts route operations from Add*Routes functions and appends them to swagger.
func Process(fset *token.FileSet, pkgs map[string]*ast.Package, swagger *openapi3.T) {
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
			log.Printf("Processing file: %s", path)

			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok {
					return true
				}
				if strings.HasPrefix(fn.Name.Name, "Add") && strings.HasSuffix(fn.Name.Name, "Routes") {
					extractRoutesFromFunction(fset, file, fn, swagger)
				}
				return true
			})
		}
	}
}

func extractRoutesFromFunction(fset *token.FileSet, file *ast.File, fn *ast.FuncDecl, swagger *openapi3.T) {
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "HandleFunc" {
			extractRoute(fset, file, call, swagger)
		}
		return true
	})
}

func extractRoute(_ *token.FileSet, file *ast.File, call *ast.CallExpr, swagger *openapi3.T) {
	var path string
	var method string
	if len(call.Args) > 0 {
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			parts := strings.Split(strings.Trim(lit.Value, `"`), " ")
			if len(parts) == 2 {
				method = parts[0]
				path = parts[1]
			} else {
				path = parts[0]
				method = "GET"
			}
		}
	}

	var handler *ast.FuncDecl
	var handlerDocs string
	if len(call.Args) > 1 {
		switch arg := call.Args[1].(type) {
		case *ast.FuncLit:
			handler = &ast.FuncDecl{
				Name: ast.NewIdent("handler"),
				Type: arg.Type,
				Body: arg.Body,
			}
		case *ast.SelectorExpr:
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if ok && fn.Name.Name == arg.Sel.Name {
					handler = fn
					handlerDocs = commenttext.Text(fn.Doc)
					return false
				}
				return true
			})
		}
	}

	if handler == nil {
		return
	}

	if strings.Contains(path, "{") {
		pathItem := swagger.Paths.Find(path)
		if pathItem == nil {
			pathItem = &openapi3.PathItem{}
			swagger.Paths.Set(path, pathItem)

			paramDescriptions := findPathParamDescriptions(handler)
			for _, paramName := range extractPathParams(path) {
				param := openapi3.NewPathParameter(paramName)
				param.Required = true
				param.Schema = openapi3.NewStringSchema().NewRef()
				if desc, ok := paramDescriptions[paramName]; ok {
					param.Description = desc
				}
				pathItem.Parameters = append(pathItem.Parameters, &openapi3.ParameterRef{Value: param})
			}
		}
	}

	if swagger.Paths.Find(path) == nil {
		swagger.Paths.Set(path, &openapi3.PathItem{})
	}
	pathItem := swagger.Paths.Find(path)

	op := openapi3.NewOperation()
	op.Summary = strings.TrimPrefix(handler.Name.Name, "handle")
	for _, param := range findQueryParams(handler) {
		op.AddParameter(param)
	}

	if handlerDocs != "" {
		parts := strings.SplitN(handlerDocs, "\n", 2)
		if len(parts) > 0 {
			op.Summary = parts[0]
		}
		op.Description = handlerDocs
	}

	if reqType := extractRequestType(handler, file); reqType != "" {
		content := openapi3.Content{
			"application/json": &openapi3.MediaType{
				Schema: schema.RefForAnnotation(reqType),
			},
		}
		op.RequestBody = &openapi3.RequestBodyRef{
			Value: &openapi3.RequestBody{
				Content:  content,
				Required: true,
			},
		}
	}

	statusCodes := extractStatusCodes(handler, file)
	for status, respType := range statusCodes {
		content := openapi3.Content{
			"application/json": &openapi3.MediaType{
				Schema: schema.RefForAnnotation(respType),
			},
		}
		response := openapi3.NewResponse()
		description := httpStatusToDescription(status)
		response.Description = &description
		response.Content = content
		op.AddResponse(status, response)
	}

	if op.Responses == nil {
		op.Responses = openapi3.NewResponses()
	}
	defaultResponse := openapi3.NewResponse()
	defaultDesc := "Default error response"
	defaultResponse.Description = &defaultDesc
	defaultResponse.Content = openapi3.Content{
		"application/json": &openapi3.MediaType{
			Schema: &openapi3.SchemaRef{Ref: "#/components/schemas/ErrorResponse"},
		},
	}
	op.Responses.Set("default", &openapi3.ResponseRef{Value: defaultResponse})

	if isSSEHandler(handler) {
		responseRef := op.Responses.Map()["200"]
		if responseRef == nil {
			responseRef = &openapi3.ResponseRef{Value: openapi3.NewResponse()}
			desc := "OK"
			responseRef.Value.Description = &desc
			op.Responses.Set("200", responseRef)
		}
		if responseRef.Value.Content == nil {
			responseRef.Value.Content = openapi3.NewContent()
		}
		mediaType := openapi3.NewMediaType()
		mediaType.Schema = openapi3.NewStringSchema().NewRef()
		responseRef.Value.Content["text/event-stream"] = mediaType
	} else if len(statusCodes) == 0 {
		response := openapi3.NewResponse()
		desc := "OK"
		response.Description = &desc
		op.AddResponse(200, response)
	}

	switch strings.ToUpper(method) {
	case "GET":
		pathItem.Get = op
	case "POST":
		pathItem.Post = op
	case "PUT":
		pathItem.Put = op
	case "DELETE":
		pathItem.Delete = op
	case "PATCH":
		pathItem.Patch = op
	}
}

func extractRequestType(handler *ast.FuncDecl, file *ast.File) string {
	var reqType string
	ast.Inspect(handler.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		gen, ok := call.Fun.(*ast.IndexExpr)
		if !ok {
			return true
		}
		sel, ok := gen.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Decode" {
			return true
		}
		if comment := findFollowingComment(call, file); comment != "" {
			if after, ok := strings.CutPrefix(comment, "@request "); ok {
				reqType = normalizeAnnotatedType(after)
				return false
			}
		}
		reqType = normalizeTypeExpr(gen.Index)
		return false
	})
	return reqType
}

func extractStatusCodes(handler *ast.FuncDecl, file *ast.File) map[int]string {
	statusCodes := make(map[int]string)
	ast.Inspect(handler.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Encode" {
			return true
		}
		if len(call.Args) < 4 {
			return true
		}

		status := 0
		switch arg := call.Args[2].(type) {
		case *ast.Ident:
			status = httpStatusToCode(arg.Name)
		case *ast.SelectorExpr:
			status = httpStatusToCode(arg.Sel.Name)
		case *ast.BasicLit:
			if i, err := strconv.Atoi(arg.Value); err == nil {
				status = i
			}
		}
		if status == 0 {
			return true
		}

		if comment := findFollowingComment(call, file); comment != "" {
			if after, ok := strings.CutPrefix(comment, "@response "); ok {
				statusCodes[status] = normalizeAnnotatedType(after)
				return true
			}
		}

		statusCodes[status] = normalizeTypeExpr(call.Args[3])
		return true
	})
	return statusCodes
}

func normalizeAnnotatedType(typeStr string) string {
	isArray := false
	if strings.HasPrefix(typeStr, "[]") {
		isArray = true
		typeStr = strings.TrimPrefix(typeStr, "[]")
	}
	typeStr = strings.TrimPrefix(typeStr, "*")
	typeStr = strings.ReplaceAll(typeStr, ".", "_")
	if isArray {
		typeStr = "array_" + typeStr
	}
	return typeStr
}

func normalizeTypeExpr(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if pkg, ok := v.X.(*ast.Ident); ok {
			return pkg.Name + "_" + v.Sel.Name
		}
		return v.Sel.Name
	case *ast.StarExpr:
		return normalizeTypeExpr(v.X)
	case *ast.ArrayType:
		elem := normalizeTypeExpr(v.Elt)
		if elem == "" {
			return "array_object"
		}
		return "array_" + elem
	case *ast.CompositeLit:
		return normalizeTypeExpr(v.Type)
	default:
		return "object"
	}
}

func findFollowingComment(node ast.Node, file *ast.File) string {
	pos := node.End()
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if comment.Slash == pos+1 || comment.Slash == pos+2 || comment.Slash == pos+3 {
				text := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
				if strings.HasPrefix(text, "@") {
					return text
				}
			}
		}
	}
	return ""
}

func httpStatusToCode(name string) int {
	switch name {
	case "StatusOK":
		return 200
	case "StatusCreated":
		return 201
	case "StatusNoContent":
		return 204
	case "StatusBadRequest":
		return 400
	case "StatusUnauthorized":
		return 401
	case "StatusForbidden":
		return 403
	case "StatusNotFound":
		return 404
	case "StatusConflict":
		return 409
	case "StatusUnprocessableEntity":
		return 422
	default:
		return 500
	}
}

func httpStatusToDescription(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	default:
		return "Internal Server Error"
	}
}

func isSSEHandler(handler *ast.FuncDecl) bool {
	if handler.Doc == nil {
		return false
	}
	for _, c := range handler.Doc.List {
		if strings.Contains(c.Text, "Server-Sent Events") || strings.Contains(c.Text, "streams status updates") {
			return true
		}
	}
	return false
}

func extractPathParams(path string) []string {
	seen := make(map[string]bool)
	var params []string
	for _, part := range strings.Split(path, "/") {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			name := strings.Trim(part, "{}")
			if !seen[name] {
				seen[name] = true
				params = append(params, name)
			}
		}
	}
	return params
}

func findPathParamDescriptions(handler *ast.FuncDecl) map[string]string {
	descriptions := make(map[string]string)
	if handler == nil {
		return descriptions
	}

	ast.Inspect(handler.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "GetPathParam" {
			return true
		}
		if len(call.Args) != 3 {
			return true
		}
		nameLit, nameOK := call.Args[1].(*ast.BasicLit)
		descLit, descOK := call.Args[2].(*ast.BasicLit)
		if nameOK && descOK && nameLit.Kind == token.STRING && descLit.Kind == token.STRING {
			descriptions[strings.Trim(nameLit.Value, `"`)] = strings.Trim(descLit.Value, `"`)
		}
		return true
	})

	return descriptions
}

func findQueryParams(handler *ast.FuncDecl) []*openapi3.Parameter {
	if handler == nil {
		return nil
	}

	queryParams := make(map[string]*openapi3.Parameter)
	ast.Inspect(handler.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "GetQueryParam" || len(call.Args) != 4 {
			return true
		}

		nameLit, nameOK := call.Args[1].(*ast.BasicLit)
		defLit, defOK := call.Args[2].(*ast.BasicLit)
		descLit, descOK := call.Args[3].(*ast.BasicLit)
		if !nameOK || !defOK || !descOK {
			return true
		}

		paramName := strings.Trim(nameLit.Value, `"`)
		param := openapi3.NewQueryParameter(paramName).
			WithSchema(openapi3.NewStringSchema()).
			WithDescription(strings.Trim(descLit.Value, `"`))
		param.Required = false

		defaultValue := strings.Trim(defLit.Value, `"`)
		if defaultValue != "" {
			param.Schema.Value.Default = defaultValue
		}
		queryParams[paramName] = param
		return true
	})

	names := make([]string, 0, len(queryParams))
	for name := range queryParams {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]*openapi3.Parameter, 0, len(names))
	for _, name := range names {
		result = append(result, queryParams[name])
	}
	return result
}
