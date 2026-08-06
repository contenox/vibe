package libacp

import "encoding/json"

type ContentKind string

const (
	ContentKindText         ContentKind = "text"
	ContentKindImage        ContentKind = "image"
	ContentKindAudio        ContentKind = "audio"
	ContentKindResource     ContentKind = "resource"
	ContentKindResourceLink ContentKind = "resource_link"
)

type Annotations struct {
	Audience []string `json:"audience,omitempty"`
	// LastModified is an ISO 8601 timestamp indicating when the underlying
	// resource was last modified.
	LastModified string          `json:"lastModified,omitempty"`
	Priority     *float64        `json:"priority,omitempty"`
	Meta         json.RawMessage `json:"_meta,omitempty"`
}

type EmbeddedResource struct {
	URI      string          `json:"uri"`
	MimeType string          `json:"mimeType,omitempty"`
	Text     string          `json:"text,omitempty"`
	Blob     string          `json:"blob,omitempty"`
	Meta     json.RawMessage `json:"_meta,omitempty"`
}

type ContentBlock struct {
	Type        string            `json:"type"`
	Text        string            `json:"text,omitempty"`
	Data        string            `json:"data,omitempty"`
	MimeType    string            `json:"mimeType,omitempty"`
	URI         string            `json:"uri,omitempty"`
	Name        string            `json:"name,omitempty"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Size        *int64            `json:"size,omitempty"`
	Resource    *EmbeddedResource `json:"resource,omitempty"`
	Annotations *Annotations      `json:"annotations,omitempty"`
	Meta        json.RawMessage   `json:"_meta,omitempty"`
}

func NewTextContent(text string) ContentBlock {
	return ContentBlock{Type: string(ContentKindText), Text: text}
}

func NewImageContent(data, mimeType string) ContentBlock {
	return ContentBlock{Type: string(ContentKindImage), Data: data, MimeType: mimeType}
}

func NewResourceLink(uri, name string) ContentBlock {
	return ContentBlock{Type: string(ContentKindResourceLink), URI: uri, Name: name}
}

func NewResourceContent(resource EmbeddedResource) ContentBlock {
	return ContentBlock{Type: string(ContentKindResource), Resource: &resource}
}
