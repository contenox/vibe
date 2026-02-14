package apiframework

import (
	"errors"
	"net/http"

	"github.com/contenox/vibe/eventstore"
	"github.com/contenox/vibe/libauth"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
)

// Standard error constants
var (
	ErrInvalidParameterValue = errors.New("serverops: invalid parameter value type")
	ErrBadPathValue          = errors.New("serverops: bad path value")
	ErrBadQueryValue         = errors.New("serverops: bad query value")
	ErrImmutableModel        = errors.New("serverops: immutable model")
	ErrImmutableGroup        = errors.New("serverops: immutable group")
	ErrMissingParameter      = errors.New("serverops: missing parameter")
	ErrEmptyRequest          = errors.New("serverops: empty request")
	ErrEmptyRequestBody      = errors.New("serverops: empty request body")
	ErrInvalidChain          = errors.New("serverops: invalid chain definition")

	// The generic error types for common HTTP status codes
	ErrBadRequest            = errors.New("serverops: bad request")
	ErrUnprocessableEntity   = errors.New("serverops: unprocessable entity")
	ErrNotFound              = errors.New("serverops: not found")
	ErrConflict              = errors.New("serverops: conflict")
	ErrForbidden             = errors.New("serverops: forbidden")
	ErrInternalServerError   = errors.New("serverops: internal server error")
	ErrUnsupportedMediaType  = errors.New("serverops: unsupported media type")
	ErrUnauthorized          = errors.New("serverops: unauthorized")
	ErrFileSizeLimitExceeded = errors.New("serverops: file size limit exceeded")
	ErrFileEmpty             = errors.New("serverops: file cannot be empty")
)

// ErrorType/ErrorCode Mappings for Standard Errors
var errorMappings = map[error]struct {
	errorType string
	errorCode string
}{
	ErrInvalidParameterValue: {"invalid_request_error", "invalid_parameter_value"},
	ErrBadPathValue:          {"invalid_request_error", "bad_path_value"},
	ErrImmutableModel:        {"invalid_request_error", "immutable_model"},
	ErrImmutableGroup:        {"invalid_request_error", "immutable_group"},
	ErrMissingParameter:      {"invalid_request_error", "missing_parameter"},
	ErrEmptyRequest:          {"invalid_request_error", "empty_request"},
	ErrEmptyRequestBody:      {"invalid_request_error", "empty_request_body"},
	ErrBadRequest:            {"invalid_request_error", "bad_request"},
	ErrUnprocessableEntity:   {"invalid_request_error", "unprocessable_entity"},
	ErrNotFound:              {"invalid_request_error", "not_found"},
	ErrConflict:              {"invalid_request_error", "conflict"},
	ErrForbidden:             {"authorization_error", "forbidden"},
	ErrInternalServerError:   {"api_error", "internal_server_error"},
	ErrUnsupportedMediaType:  {"invalid_request_error", "unsupported_media_type"},
	ErrUnauthorized:          {"authentication_error", "unauthorized"},
	ErrFileSizeLimitExceeded: {"invalid_request_error", "file_size_limit_exceeded"},
	ErrFileEmpty:             {"invalid_request_error", "file_empty"},
	ErrInvalidChain:          {"invalid_request_error", "invalid_chain"},
}

// getErrorMapping finds specific errorType/errorCode for standard errors
func getErrorMapping(err error) (string, string) {
	for standardErr, mapping := range errorMappings {
		if errors.Is(err, standardErr) {
			return mapping.errorType, mapping.errorCode
		}
	}
	return "", ""
}

// getErrorTypeAndCode maps HTTP status codes to error types and codes
func getErrorTypeAndCode(status int) (string, string) {
	switch status {
	case 400:
		return "invalid_request_error", "bad_request"
	case 401:
		return "authentication_error", "unauthorized"
	case 403:
		return "authorization_error", "forbidden"
	case 404:
		return "invalid_request_error", "not_found"
	case 409:
		return "invalid_request_error", "conflict"
	case 413:
		return "invalid_request_error", "request_too_large"
	case 415:
		return "invalid_request_error", "unsupported_media"
	case 422:
		return "invalid_request_error", "unprocessable_entity"
	case 429:
		return "rate_limit_error", "rate_limit_exceeded"
	case 500:
		return "api_error", "internal_error"
	default:
		return "api_error", "unknown_error"
	}
}

// Operation defines API operation types for error mapping
type Operation uint16

const (
	CreateOperation Operation = iota
	GetOperation
	UpdateOperation
	DeleteOperation
	ListOperation
	AuthorizeOperation
	ServerOperation
	ExecuteOperation
)

// mapErrorToStatus maps errors to appropriate HTTP status codes
func mapErrorToStatus(op Operation, err error) int {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return http.StatusRequestEntityTooLarge // 413
	}
	if errors.Is(err, ErrFileSizeLimitExceeded) {
		return http.StatusRequestEntityTooLarge // 413
	}
	if errors.Is(err, http.ErrNotMultipart) {
		return http.StatusUnsupportedMediaType // 415
	}
	if errors.Is(err, http.ErrMissingFile) {
		return http.StatusBadRequest // 400
	}
	if errors.Is(err, ErrFileEmpty) {
		return http.StatusBadRequest // 400
	}
	if errors.Is(err, libauth.ErrNotAuthorized) {
		return http.StatusUnauthorized // 401
	}
	if op == AuthorizeOperation {
		return http.StatusForbidden // 403
	}
	if errors.Is(err, libauth.ErrTokenExpired) {
		return http.StatusUnauthorized // 401
	}
	// Token format/validation issues
	if errors.Is(err, libauth.ErrIssuedAtMissing) ||
		errors.Is(err, libauth.ErrIssuedAtInFuture) ||
		errors.Is(err, libauth.ErrIdentityMissing) ||
		errors.Is(err, libauth.ErrInvalidTokenClaims) ||
		errors.Is(err, libauth.ErrTokenMissing) ||
		errors.Is(err, libauth.ErrUnexpectedSigningMethod) ||
		errors.Is(err, libauth.ErrTokenParsingFailed) ||
		errors.Is(err, libauth.ErrTokenSigningFailed) {
		return http.StatusBadRequest // 400
	}

	if errors.Is(err, ErrEmptyRequest) {
		return http.StatusBadRequest // 400
	}
	if errors.Is(err, ErrEmptyRequestBody) {
		return http.StatusBadRequest // 400
	}
	if errors.Is(err, ErrBadRequest) {
		return http.StatusBadRequest // 400
	}

	if errors.Is(err, ErrUnauthorized) {
		return http.StatusUnauthorized // 401
	}
	if errors.Is(err, ErrForbidden) {
		return http.StatusForbidden // 403
	}
	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound // 404
	}
	if errors.Is(err, ErrConflict) {
		return http.StatusConflict // 409
	}
	if errors.Is(err, ErrUnsupportedMediaType) {
		return http.StatusUnsupportedMediaType // 415
	}
	if errors.Is(err, ErrInternalServerError) {
		return http.StatusInternalServerError // 500
	}
	if errors.Is(err, ErrUnprocessableEntity) {
		return http.StatusUnprocessableEntity // 422
	}

	if errors.Is(err, libdb.ErrNotFound) {
		return http.StatusNotFound // 404
	}
	// Constraint violations
	if errors.Is(err, libdb.ErrUniqueViolation) ||
		errors.Is(err, libdb.ErrForeignKeyViolation) ||
		errors.Is(err, libdb.ErrNotNullViolation) ||
		errors.Is(err, libdb.ErrCheckViolation) ||
		errors.Is(err, libdb.ErrConstraintViolation) {
		return http.StatusConflict // 409
	}

	if errors.Is(err, libdb.ErrMaxRowsReached) {
		return http.StatusTooManyRequests
	}
	// These DB errors might be client input
	if errors.Is(err, libdb.ErrDataTruncation) ||
		errors.Is(err, libdb.ErrNumericOutOfRange) ||
		errors.Is(err, libdb.ErrInvalidInputSyntax) ||
		errors.Is(err, libdb.ErrUndefinedColumn) ||
		errors.Is(err, libdb.ErrUndefinedTable) {
		return http.StatusBadRequest
	}
	// Concurrency/Server-side DB issues
	if errors.Is(err, libdb.ErrDeadlockDetected) ||
		errors.Is(err, libdb.ErrSerializationFailure) ||
		errors.Is(err, libdb.ErrLockNotAvailable) ||
		errors.Is(err, libdb.ErrQueryCanceled) {
		return http.StatusConflict
	}
	if errors.Is(err, runtimetypes.ErrLimitParamExceeded) {
		return http.StatusBadRequest
	}
	if errors.Is(err, runtimetypes.ErrAppendLimitExceeded) {
		return http.StatusBadRequest
	}

	if errors.Is(err, ErrInvalidParameterValue) || errors.Is(err, ErrBadPathValue) {
		return http.StatusBadRequest
	}

	if errors.Is(err, ErrImmutableModel) {
		return http.StatusForbidden
	}
	if errors.Is(err, ErrImmutableGroup) {
		return http.StatusForbidden
	}
	if errors.Is(err, ErrMissingParameter) {
		return http.StatusBadRequest
	}

	if errors.Is(err, ErrInvalidChain) {
		return http.StatusBadRequest
	}

	if errors.Is(err, eventstore.ErrNotFound) {
		return http.StatusNotFound
	}

	// fallbacks based on operation
	switch op {
	case CreateOperation, UpdateOperation:
		return http.StatusUnprocessableEntity
	case GetOperation, ListOperation:
		return http.StatusNotFound
	case DeleteOperation:
		return http.StatusNotFound
	case AuthorizeOperation:
		return http.StatusForbidden
	case ServerOperation, ExecuteOperation:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// NewAPIError creates a generic APIError with context.
// If message is empty, it falls back to the underlying error's message.
func NewAPIError(err error, message, param string) *APIError {
	errorType, errorCode := getErrorMapping(err)
	if message == "" {
		message = err.Error()
	}
	return &APIError{
		err:       err,
		message:   message,
		param:     param,
		errorType: errorType,
		errorCode: errorCode,
	}
}

// Specific Error Constructors
func InvalidParameterValue(param string, message ...string) *APIError {
	msg := "Invalid parameter value"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrInvalidParameterValue, msg, param)
}

func MissingParameter(param string, message ...string) *APIError {
	msg := "Missing required parameter"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrMissingParameter, msg, param)
}

func Unauthorized(message ...string) *APIError {
	msg := "Unauthorized access"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrUnauthorized, msg, "")
}

func Forbidden(message ...string) *APIError {
	msg := "Forbidden access"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrForbidden, msg, "")
}

func NotFound(message ...string) *APIError {
	msg := "Resource not found"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrNotFound, msg, "")
}

func BadRequest(message ...string) *APIError {
	msg := "Bad request"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrBadRequest, msg, "")
}

func UnprocessableEntity(message ...string) *APIError {
	msg := "Unprocessable entity"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrUnprocessableEntity, msg, "")
}

func Conflict(message ...string) *APIError {
	msg := "Conflict"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrConflict, msg, "")
}

func InternalServerError(message ...string) *APIError {
	msg := "Internal server error"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrInternalServerError, msg, "")
}

func UnsupportedMediaType(message ...string) *APIError {
	msg := "Unsupported media type"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrUnsupportedMediaType, msg, "")
}

func FileSizeLimitExceeded(message ...string) *APIError {
	msg := "File size limit exceeded"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrFileSizeLimitExceeded, msg, "")
}

func FileEmpty(message ...string) *APIError {
	msg := "File cannot be empty"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrFileEmpty, msg, "")
}

func InvalidChain(message ...string) *APIError {
	msg := "Invalid chain definition"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrInvalidChain, msg, "")
}

func BadPathValue(param string, message ...string) *APIError {
	msg := "Bad path value"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrBadPathValue, msg, param)
}

func ImmutableModel(message ...string) *APIError {
	msg := "Model is immutable"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrImmutableModel, msg, "")
}

func ImmutableGroup(message ...string) *APIError {
	msg := "Group is immutable"
	if len(message) > 0 && message[0] != "" {
		msg = message[0]
	}
	return NewAPIError(ErrImmutableGroup, msg, "")
}
