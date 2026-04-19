package vfsstore

import "time"

// File represents a logical file or folder in the VFS.
type File struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Meta      []byte    `json:"meta"`
	BlobsID   string    `json:"blobsId,omitempty"`
	IsFolder  bool      `json:"isFolder"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Blob holds binary data associated with a file.
type Blob struct {
	ID        string    `json:"id"`
	Meta      []byte    `json:"meta"`
	Data      []byte    `json:"data"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// FileTreeEntry represents a node in the hierarchical file tree.
type FileTreeEntry struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parentId,omitempty"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
