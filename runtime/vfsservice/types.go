package vfsservice

import "time"

// File represents a file in the VFS.
type File struct {
	ID          string    `json:"id"`
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	ParentID    string    `json:"parentId"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType"`
	Data        []byte    `json:"data"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	// IsDirectory is set for directory entries returned by GetFilesByPath; omitted for normal files.
	IsDirectory bool `json:"isDirectory,omitempty"`
}

// Folder represents a directory in the VFS.
type Folder struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	ParentID  string    `json:"parentId"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Metadata holds file content metadata stored alongside blob data.
type Metadata struct {
	SpecVersion string `json:"specVersion"`
	Path        string `json:"path"`
	Hash        string `json:"hash"`
	Size        int64  `json:"size"`
	FileID      string `json:"fileId"`
}
