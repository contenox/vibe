package apiframework

// MessageResponse is the preferred success body for simple endpoints that only
// need to return a human-readable message without introducing a route-specific
// DTO shape.
//
// Route handlers should still use Encode and annotate the call with:
//   // @response apiframework.MessageResponse
type MessageResponse struct {
	Message string `json:"message"`
}
