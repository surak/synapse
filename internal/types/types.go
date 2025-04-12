package types

import (
	"net/http"
)

// ModelInfo stores model information
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Owned   string `json:"owned_by"`
}

// ModelsResponse Model list returned by upstream API
type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// ClientRegistration Client registration information
type ClientRegistration struct {
	ClientID string      `json:"client_id"`
	Models   []ModelInfo `json:"models"`
	Version  string      `json:"version"`
}

// ForwardRequest Structure of the forwarded request
type ForwardRequest struct {
	Type      int         `json:"type"`
	RequestID string      `json:"request_id"`
	Model     string      `json:"model"`
	Method    string      `json:"method"`
	Path      string      `json:"path"`
	Query     string      `json:"query"`
	Header    http.Header `json:"header"`
	Body      []byte      `json:"body"`
}

// ResponseType represents the response type
const (
	TypeNormal        = 0 // Normal response
	TypeStream        = 1 // Streaming response
	TypeHeartbeat     = 2 // Heartbeat type
	TypePong          = 3 // Pong response
	TypeClientClose   = 4 // Client close request
	TypeModelUpdate   = 5 // Model update type
	TypeUnregister    = 6 // Add: Unregister type
	TypeForceShutdown = 7 // Add: Force shutdown type
)

type ForwardResponse struct {
	RequestID  string      `json:"request_id"`
	StatusCode int         `json:"status_code"`
	Header     http.Header `json:"header"`
	Body       []byte      `json:"body"`
	Type       int         `json:"type"` // TypeNormal or TypeStream
	Done       bool        `json:"done"` // Used for stream end marker
	Timestamp  int64       `json:"timestamp"`
}

// Add model update request structure
type ModelUpdateRequest struct {
	ClientID string      `json:"client_id"`
	Models   []ModelInfo `json:"models"`
}

// Add unregister request structure
type UnregisterRequest struct {
	ClientID string `json:"client_id"`
}

// Add force shutdown request structure
type ForceShutdownRequest struct {
	ClientID string `json:"client_id"`
}
