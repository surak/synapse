package types

import (
	"encoding/json"
	"net/http"
)

// ModelInfo 存储模型信息
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Owned   bool   `json:"owned_by"`
}

// ModelsResponse 上游API返回的模型列表
type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// ClientRegistration 客户端注册信息
type ClientRegistration struct {
	ClientID string      `json:"client_id"`
	Models   []ModelInfo `json:"models"`
}

// ForwardRequest 转发请求的结构
type ForwardRequest struct {
	Model  string          `json:"model"`
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Query  string          `json:"query"`
	Header http.Header     `json:"header"`
	Body   json.RawMessage `json:"body"`
}
