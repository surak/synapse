package types

import (
	"net/http"
)

// ModelInfo 存储模型信息
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Owned   string `json:"owned_by"`
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
	Type      int         `json:"type"`
	RequestID string      `json:"request_id"`
	Model     string      `json:"model"`
	Method    string      `json:"method"`
	Path      string      `json:"path"`
	Query     string      `json:"query"`
	Header    http.Header `json:"header"`
	Body      []byte      `json:"body"`
}

// ResponseType 表示响应类型
const (
	TypeNormal    = 0 // 普通响应
	TypeStream    = 1 // 流式响应
	TypeHeartbeat = 2 // 心跳类型
	TypePong      = 3 // Pong响应
	TypeClientClose = 4 // 客户端关闭请求
	TypeModelUpdate = 5 // 模型更新类型
)

type ForwardResponse struct {
	RequestID  string      `json:"request_id"`
	StatusCode int         `json:"status_code"`
	Header     http.Header `json:"header"`
	Body       []byte      `json:"body"`
	Type       int         `json:"type"` // TypeNormal 或 TypeStream
	Done       bool        `json:"done"` // 用于流式结束标记
	Timestamp  int64       `json:"timestamp"`
}

// 添加模型更新请求结构
type ModelUpdateRequest struct {
	ClientID string      `json:"client_id"`
	Models   []ModelInfo `json:"models"`
}
