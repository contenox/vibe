// Package vfsapi provides HTTP handlers for file and folder management.
package vfsapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/runtime/vfsservice"
)

const (
	MaxRequestSize      = vfsservice.MaxUploadSize + 10*1024
	multipartFormMemory = 8 << 20 // 8 MiB
	formFieldFile       = "file"
	formFieldName       = "name"
	formFieldParent     = "parentid"
)

// AddRoutes sets up the routes for file and folder operations.
func AddRoutes(mux *http.ServeMux, fileService vfsservice.Service) {
	f := &fileManager{service: fileService}

	// File operations
	mux.HandleFunc("POST /files", f.create)                // Create a new file
	mux.HandleFunc("GET /files/{id}", f.getMetadata)       // Get file metadata
	mux.HandleFunc("PUT /files/{id}", f.update)            // Update file content
	mux.HandleFunc("DELETE /files/{id}", f.deleteFile)     // Delete a file
	mux.HandleFunc("GET /files/{id}/download", f.download) // Download file content
	mux.HandleFunc("PUT /files/{id}/name", f.renameFile)   // Rename a file
	mux.HandleFunc("PUT /files/{id}/path", f.renameFile)   // UI-compat alias
	mux.HandleFunc("PUT /files/{id}/move", f.moveFile)     // Move a file

	// Folder operations
	mux.HandleFunc("POST /folders", f.createFolder)          // Create a new folder
	mux.HandleFunc("PUT /folders/{id}/name", f.renameFolder) // Rename a folder
	mux.HandleFunc("PUT /folders/{id}/path", f.renameFolder) // UI-compat alias
	mux.HandleFunc("DELETE /folders/{id}", f.deleteFolder)   // Delete a folder
	mux.HandleFunc("PUT /folders/{id}/move", f.moveFolder)   // Move a folder

	// Listing operations
	mux.HandleFunc("GET /files", f.listFiles) // List files and folders
}

type fileManager struct {
	service vfsservice.Service
}

// FileResponse represents a file in API responses.
type FileResponse struct {
	ID          string    `json:"id" example:"file_abc123"`
	Path        string    `json:"path" example:"/documents/report.pdf"`
	Name        string    `json:"name" example:"report.pdf"`
	ContentType string    `json:"contentType,omitempty" example:"application/pdf"`
	Size        int64     `json:"size" example:"102400"`
	CreatedAt   time.Time `json:"createdAt" example:"2024-06-01T12:00:00Z"`
	UpdatedAt   time.Time `json:"updatedAt" example:"2024-06-01T12:00:00Z"`
	IsDirectory bool      `json:"isDirectory,omitempty" example:"false"`
}

func fileToResponse(f vfsservice.File) FileResponse {
	return FileResponse{
		ID:          f.ID,
		Path:        f.Path,
		Name:        f.Name,
		ContentType: f.ContentType,
		Size:        f.Size,
		CreatedAt:   f.CreatedAt,
		UpdatedAt:   f.UpdatedAt,
		IsDirectory: f.IsDirectory,
	}
}

// FolderResponse represents a folder in API responses.
type FolderResponse struct {
	ID        string    `json:"id" example:"folder_xyz789"`
	Path      string    `json:"path" example:"/documents/projects"`
	Name      string    `json:"name" example:"projects"`
	ParentID  string    `json:"parentId,omitempty" example:"folder_root"`
	CreatedAt time.Time `json:"createdAt" example:"2024-06-01T12:00:00Z"`
	UpdatedAt time.Time `json:"updatedAt" example:"2024-06-01T12:00:00Z"`
}

// nameUpdateRequest is used for rename operations.
type nameUpdateRequest struct {
	Name string `json:"name" example:"new-name.txt"`
	Path string `json:"path,omitempty" example:"new-name.txt"`
}

// moveRequest is used for move operations.
type moveRequest struct {
	NewParentID string `json:"newParentId" example:"folder_abc123"`
}

// folderCreateRequest is used to create a new folder.
type folderCreateRequest struct {
	Name     string `json:"name" example:"New Folder"`
	Path     string `json:"path,omitempty" example:"New Folder"`
	ParentID string `json:"parentId,omitempty" example:"folder_root"`
}

// It validates the request, size, and MIME type. Reads the file content into memory,
// ensuring not to read more than MaxUploadSize bytes even if headers are manipulated.
func (f *fileManager) processAndReadFileUpload(w http.ResponseWriter, r *http.Request) (
	header *multipart.FileHeader,
	fileData []byte,
	name string,
	parentID string,
	mimeType string,
	err error,
) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestSize)

	if err := r.ParseMultipartForm(multipartFormMemory); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return nil, nil, "", "", "", fmt.Errorf("request body too large (limit %d bytes): %w", maxBytesErr.Limit, apiframework.ErrFileSizeLimitExceeded)
		}
		if errors.Is(err, http.ErrNotMultipart) {
			return nil, nil, "", "", "", fmt.Errorf("invalid request format (not multipart): %w", apiframework.ErrUnprocessableEntity)
		}
		return nil, nil, "", "", "", fmt.Errorf("failed to parse multipart form: %w", apiframework.ErrUnprocessableEntity)
	}

	filePart, header, err := r.FormFile(formFieldFile)
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) {
			return nil, nil, "", "", "", fmt.Errorf("missing required file field '%s': %w", formFieldFile, apiframework.ErrUnprocessableEntity)
		}
		return nil, nil, "", "", "", fmt.Errorf("invalid file upload: %w", apiframework.ErrUnprocessableEntity)
	}
	defer filePart.Close()

	if header.Size == 0 {
		return nil, nil, "", "", "", apiframework.ErrFileEmpty
	}
	if header.Size > vfsservice.MaxUploadSize {
		return nil, nil, "", "", "", apiframework.ErrFileSizeLimitExceeded
	}

	limitedReader := io.LimitReader(filePart, vfsservice.MaxUploadSize+1)
	fileData, err = io.ReadAll(limitedReader)
	if err != nil {
		return nil, nil, "", "", "", fmt.Errorf("failed to read file content: %w", apiframework.ErrUnprocessableEntity)
	}

	if int64(len(fileData)) > vfsservice.MaxUploadSize {
		return nil, nil, "", "", "", apiframework.ErrFileSizeLimitExceeded
	}

	mimeType = http.DetectContentType(fileData)
	name = r.FormValue(formFieldName)
	if name == "" {
		name = header.Filename
	}
	parentID = r.FormValue(formFieldParent)

	return header, fileData, name, parentID, mimeType, nil
}

// Creates a new file by uploading binary content via multipart/form-data.
//
// The 'file' field is required. Optional 'name' and 'parentid' fields control naming and placement.
// Files are limited to 100 MiB (configurable via vfsservice.MaxUploadSize).
func (f *fileManager) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	header, fileData, name, parentID, mimeType, err := f.processAndReadFileUpload(w, r)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	req := vfsservice.File{
		Name:        name,
		ParentID:    parentID,
		ContentType: mimeType,
		Data:        fileData,
		Size:        header.Size,
	}

	file, err := f.service.CreateFile(ctx, &req)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	apiframework.Encode(w, r, http.StatusCreated, fileToResponse(*file)) // @response vfsapi.FileResponse
}

// Retrieves metadata for a specific file.
//
// Returns 404 if the file does not exist.
func (f *fileManager) getMetadata(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id := apiframework.GetPathParam(r, "id", "The unique identifier of the file.") // @param id string
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("file ID is required: %w", apiframework.ErrBadPathValue), apiframework.GetOperation)
		return
	}

	file, err := f.service.GetFileByID(ctx, id)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}

	apiframework.Encode(w, r, http.StatusOK, fileToResponse(*file)) // @response vfsapi.FileResponse
}

// Updates an existing file's content via multipart/form-data.
//
// Replaces the entire file content. The file ID is taken from the URL path.
func (f *fileManager) update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id := apiframework.GetPathParam(r, "id", "The unique identifier of the file.") // @param id string
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("file ID is required: %w", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}

	header, fileData, _, parentID, mimeType, err := f.processAndReadFileUpload(w, r)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	req := vfsservice.File{
		ID:          id,
		ParentID:    parentID,
		ContentType: mimeType,
		Data:        fileData,
		Size:        header.Size,
	}

	file, err := f.service.UpdateFile(ctx, &req)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	apiframework.Encode(w, r, http.StatusOK, fileToResponse(*file)) // @response vfsapi.FileResponse
}

// Deletes a file from the system.
//
// Returns a confirmation message on success.
func (f *fileManager) deleteFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id := apiframework.GetPathParam(r, "id", "The unique identifier of the file.") // @param id string
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("file ID is required: %w", apiframework.ErrBadPathValue), apiframework.DeleteOperation)
		return
	}

	if err := f.service.DeleteFile(ctx, id); err != nil {
		apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}

	apiframework.Encode(w, r, http.StatusOK, apiframework.MessageResponse{Message: "file removed"}) // @response apiframework.MessageResponse
}

// Downloads the raw content of a file.
//
// The 'skip' query parameter (if "true") omits the Content-Disposition header.
func (f *fileManager) download(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id := apiframework.GetPathParam(r, "id", "The unique identifier of the file.") // @param id string
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("file ID is required: %w", apiframework.ErrBadPathValue), apiframework.GetOperation)
		return
	}

	skip := apiframework.GetQueryParam(r, "skip", "false", "If 'true', skips Content-Disposition header.") // @query skip string

	file, err := f.service.GetFileByID(ctx, id)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))

	if skip != "true" {
		sanitized := strconv.Quote(file.Path)
		w.Header().Set("Content-Disposition", "attachment; filename="+sanitized)
	}

	if _, err := bytes.NewReader(file.Data).WriteTo(w); err != nil {
		// Log error internally, but cannot recover mid-response
		http.Error(w, "failed to stream file", http.StatusInternalServerError)
	}
}

// Lists files and folders, optionally filtered by path.
//
// Use the 'path' query parameter to list contents of a specific directory.
func (f *fileManager) listFiles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	pathFilter := apiframework.GetQueryParam(r, "path", "", "Filter results by file path prefix.") // @query path string
	decodedPath, err := url.QueryUnescape(pathFilter)
	if err != nil {
		apiframework.Error(w, r, fmt.Errorf("invalid 'path' parameter: %w", apiframework.ErrUnprocessableEntity), apiframework.ListOperation)
		return
	}

	files, err := f.service.GetFilesByPath(ctx, decodedPath)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	response := make([]FileResponse, len(files))
	for i, file := range files {
		response[i] = fileToResponse(file)
	}

	apiframework.Encode(w, r, http.StatusOK, response) // @response []vfsapi.FileResponse
}

// Creates a new folder.
//
// Requires a 'name'. Optionally accepts 'parentId' to place it inside another folder.
func (f *fileManager) createFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, err := apiframework.Decode[folderCreateRequest](r) // @request vfsapi.folderCreateRequest
	if err != nil {
		apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	name := resolveName(req.Name, req.Path)
	if name == "" {
		apiframework.Error(w, r, fmt.Errorf("'name' is required: %w", apiframework.ErrUnprocessableEntity), apiframework.CreateOperation)
		return
	}

	folder, err := f.service.CreateFolder(ctx, req.ParentID, name)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	resp := FolderResponse{
		ID:        folder.ID,
		Path:      folder.Path,
		Name:      folder.Name,
		ParentID:  folder.ParentID,
		CreatedAt: folder.CreatedAt,
		UpdatedAt: folder.UpdatedAt,
	}

	apiframework.Encode(w, r, http.StatusCreated, resp) // @response vfsapi.FolderResponse
}

// Renames a folder.
//
// Accepts a JSON body with the new 'name'.
func (f *fileManager) renameFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id := apiframework.GetPathParam(r, "id", "The unique identifier of the folder.") // @param id string
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("folder ID is required: %w", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}

	req, err := apiframework.Decode[nameUpdateRequest](r) // @request vfsapi.nameUpdateRequest
	if err != nil {
		apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	name := resolveName(req.Name, req.Path)
	if name == "" {
		apiframework.Error(w, r, fmt.Errorf("'name' is required: %w", apiframework.ErrUnprocessableEntity), apiframework.UpdateOperation)
		return
	}

	folder, err := f.service.RenameFolder(ctx, id, name)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	resp := FolderResponse{
		ID:        folder.ID,
		Path:      folder.Path,
		Name:      folder.Name,
		ParentID:  folder.ParentID,
		CreatedAt: folder.CreatedAt,
		UpdatedAt: folder.UpdatedAt,
	}

	apiframework.Encode(w, r, http.StatusOK, resp) // @response vfsapi.FolderResponse
}

// Renames a file.
//
// Accepts a JSON body with the new 'name'.
func (f *fileManager) renameFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id := apiframework.GetPathParam(r, "id", "The unique identifier of the file.") // @param id string
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("file ID is required: %w", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}

	req, err := apiframework.Decode[nameUpdateRequest](r) // @request vfsapi.nameUpdateRequest
	if err != nil {
		apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	name := resolveName(req.Name, req.Path)
	if name == "" {
		apiframework.Error(w, r, fmt.Errorf("'name' is required: %w", apiframework.ErrUnprocessableEntity), apiframework.UpdateOperation)
		return
	}

	file, err := f.service.RenameFile(ctx, id, name)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	apiframework.Encode(w, r, http.StatusOK, fileToResponse(*file)) // @response vfsapi.FileResponse
}

// Deletes a folder and all its contents.
//
// Returns a confirmation message on success.
func (f *fileManager) deleteFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id := apiframework.GetPathParam(r, "id", "The unique identifier of the folder.") // @param id string
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("folder ID is required: %w", apiframework.ErrBadPathValue), apiframework.DeleteOperation)
		return
	}

	if err := f.service.DeleteFolder(ctx, id); err != nil {
		apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}

	apiframework.Encode(w, r, http.StatusOK, apiframework.MessageResponse{Message: "folder removed"}) // @response apiframework.MessageResponse
}

// Moves a file to a new parent folder.
//
// Accepts a JSON body with 'newParentId'.
func (f *fileManager) moveFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id := apiframework.GetPathParam(r, "id", "The unique identifier of the file.") // @param id string
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("file ID is required: %w", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}

	req, err := apiframework.Decode[moveRequest](r) // @request vfsapi.moveRequest
	if err != nil {
		apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	movedFile, err := f.service.MoveFile(ctx, id, req.NewParentID)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	apiframework.Encode(w, r, http.StatusOK, fileToResponse(*movedFile)) // @response vfsapi.FileResponse
}

// Moves a folder to a new parent folder.
//
// Accepts a JSON body with 'newParentId'.
func (f *fileManager) moveFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id := apiframework.GetPathParam(r, "id", "The unique identifier of the folder.") // @param id string
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("folder ID is required: %w", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}

	req, err := apiframework.Decode[moveRequest](r) // @request vfsapi.moveRequest
	if err != nil {
		apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	movedFolder, err := f.service.MoveFolder(ctx, id, req.NewParentID)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	resp := FolderResponse{
		ID:        movedFolder.ID,
		Path:      movedFolder.Path,
		Name:      movedFolder.Name,
		ParentID:  movedFolder.ParentID,
		CreatedAt: movedFolder.CreatedAt,
		UpdatedAt: movedFolder.UpdatedAt,
	}

	apiframework.Encode(w, r, http.StatusOK, resp) // @response vfsapi.FolderResponse
}

func resolveName(name string, fromPath string) string {
	candidate := strings.TrimSpace(name)
	if candidate != "" {
		return candidate
	}
	clean := strings.TrimSpace(fromPath)
	if clean == "" {
		return ""
	}
	trimmed := strings.Trim(path.Clean(clean), "/")
	if trimmed == "." || trimmed == "" {
		return ""
	}
	return path.Base(trimmed)
}
