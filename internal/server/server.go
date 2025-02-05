package server

import (
	"bytes"
	cryptoRand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/zeyugao/synapse/internal/types"
)

type Server struct {
	clients          map[string]*Client
	modelClients     map[string][]string // model -> []clientID
	mu               sync.RWMutex
	upgrader         websocket.Upgrader
	pendingRequests  map[string]chan types.ForwardResponse
	reqMu            sync.RWMutex
	apiAuthKey       string            // 新增API鉴权密钥
	wsAuthKey        string            // 新增WebSocket鉴权密钥
	requestClient    map[string]string // 新增：requestID -> clientID
	version          string            // 新增版本号字段
	clientBinaryPath string            // 新增客户端二进制文件路径
}

type Client struct {
	conn   *websocket.Conn
	models []types.ModelInfo
	mu     sync.Mutex
}

func (c *Client) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(v)
}

func NewServer(apiAuthKey, wsAuthKey string, version string, clientBinaryPath string) *Server {
	return &Server{
		clients:         make(map[string]*Client),
		modelClients:    make(map[string][]string),
		pendingRequests: make(map[string]chan types.ForwardResponse),
		apiAuthKey:      apiAuthKey,
		wsAuthKey:       wsAuthKey,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		requestClient:    make(map[string]string),
		version:          version,          // 设置版本号
		clientBinaryPath: clientBinaryPath, // 新增初始化
	}
}

func generateRequestID() string {
	b := make([]byte, 16)
	cryptoRand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 检查WebSocket鉴权
	if s.wsAuthKey != "" {
		authKey := r.URL.Query().Get("ws_auth_key")
		if authKey != s.wsAuthKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket升级失败: %v", err)
		http.Error(w, "Could not upgrade connection", http.StatusInternalServerError)
		return
	}

	// 等待客户端注册信息
	var registration types.ClientRegistration
	if err := conn.ReadJSON(&registration); err != nil {
		log.Printf("读取注册信息失败: %v", err)
		conn.Close()
		return
	}

	// 版本检查逻辑
	if registration.Version == "" {
		log.Printf("客户端 %s 未提供版本号，拒绝连接", registration.ClientID)
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "Version required"))
		conn.Close()
		return
	}

	if registration.Version != s.version {
		log.Printf("客户端版本不匹配 (client: %s, server: %s)", registration.Version, s.version)
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4001, fmt.Sprintf("Client version %s does not match server version %s", registration.Version, s.version)))
		conn.Close()
		return
	}

	log.Printf("新客户端连接: %s, 注册模型数: %d", registration.ClientID, len(registration.Models))

	s.mu.Lock()
	client := &Client{
		conn:   conn,
		models: registration.Models,
	}
	s.clients[registration.ClientID] = client

	// 更新模型到客户端的映射
	for _, model := range registration.Models {
		s.modelClients[model.ID] = append(s.modelClients[model.ID], registration.ClientID)
	}
	s.mu.Unlock()

	// 添加处理响应的 goroutine
	go s.handleClientResponses(registration.ClientID, client)
}

func (s *Server) handleClientResponses(clientID string, client *Client) {
	defer func() {
		client.conn.Close()
		s.unregisterClient(clientID)
	}()

	for {
		var msg types.ForwardResponse

		if err := client.conn.ReadJSON(&msg); err != nil {
			log.Printf("读取客户端 %s 消息失败: %v", clientID, err)
			return
		}

		switch msg.Type {
		case types.TypeNormal, types.TypeStream:
			resp := types.ForwardResponse{
				RequestID:  msg.RequestID,
				StatusCode: msg.StatusCode,
				Header:     msg.Header,
				Body:       msg.Body,
				Type:       msg.Type,
				Done:       msg.Done,
			}
			s.handleForwardResponse(resp)
		case types.TypeHeartbeat:
			// 发送pong响应
			pongReq := types.ForwardRequest{
				Type: types.TypePong,
			}
			if err := client.writeJSON(pongReq); err != nil {
				log.Printf("发送pong响应失败: %v", err)
			}
		case types.TypeModelUpdate:
			var updateReq types.ModelUpdateRequest
			if err := json.Unmarshal(msg.Body, &updateReq); err != nil {
				log.Printf("解析模型更新请求失败: %v", err)
				return
			}
			s.handleModelUpdate(updateReq)
		case types.TypeUnregister:
			var unregisterReq types.UnregisterRequest
			if err := json.Unmarshal(msg.Body, &unregisterReq); err != nil {
				log.Printf("解析取消注册请求失败: %v", err)
				return
			}
			log.Printf("收到客户端 %s 的取消注册请求", unregisterReq.ClientID)
			s.unregisterClient(unregisterReq.ClientID)
			return
		case types.TypeForceShutdown:
			var forceReq types.ForceShutdownRequest
			if err := json.Unmarshal(msg.Body, &forceReq); err != nil {
				log.Printf("解析强制关闭请求失败: %v", err)
				return
			}
			s.handleForceShutdown(forceReq)
		default:
			log.Printf("未知消息类型: %d", msg.Type)
		}
	}
}

func (s *Server) handleForwardResponse(resp types.ForwardResponse) {
	s.reqMu.RLock()
	respChan, exists := s.pendingRequests[resp.RequestID]
	s.reqMu.RUnlock()

	if exists {
		respChan <- resp
		if (resp.Type == types.TypeStream && resp.Done) || resp.Type == types.TypeNormal {
			s.reqMu.Lock()
			delete(s.pendingRequests, resp.RequestID)
			close(respChan)
			s.reqMu.Unlock()
		}
	}
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 收集所有唯一的模型并添加客户端计数
	modelMap := make(map[string]struct {
		types.ModelInfo
		ClientCount int `json:"client_count"`
	})

	// 首先收集所有模型信息
	for _, client := range s.clients {
		for _, model := range client.models {
			if existing, exists := modelMap[model.ID]; exists {
				// 如果模型已存在，只更新客户端计数
				existing.ClientCount = len(s.modelClients[model.ID])
				modelMap[model.ID] = existing
			} else {
				modelMap[model.ID] = struct {
					types.ModelInfo
					ClientCount int `json:"client_count"`
				}{
					ModelInfo:   model,
					ClientCount: len(s.modelClients[model.ID]),
				}
			}
		}
	}

	// 转换为切片
	models := make([]struct {
		types.ModelInfo
		ClientCount int `json:"client_count"`
	}, 0, len(modelMap))
	for _, model := range modelMap {
		models = append(models, model)
	}

	response := struct {
		Object string `json:"object"`
		Data   []struct {
			types.ModelInfo
			ClientCount int `json:"client_count"`
		} `json:"data"`
	}{
		Object: "list",
		Data:   models,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) handleAPIRequest(w http.ResponseWriter, r *http.Request) {
	// 检查API鉴权
	if s.apiAuthKey != "" {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer "+s.apiAuthKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	if r.URL.Path == "/v1/models" {
		s.handleModels(w, r)
		return
	}

	// 读取请求体
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	var modelReq struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &modelReq); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// 生成请求ID
	requestID := generateRequestID()

	// 创建响应通道
	respChan := make(chan types.ForwardResponse, 1)
	s.reqMu.Lock()
	s.pendingRequests[requestID] = respChan
	s.reqMu.Unlock()

	// 构建转发请求
	req := types.ForwardRequest{
		Type:      types.TypeNormal,
		RequestID: requestID,
		Model:     modelReq.Model,
		Method:    r.Method,
		Path:      r.URL.Path,
		Query:     r.URL.RawQuery,
		Header:    r.Header.Clone(),
		Body:      body,
	}

	// 选择客户端并发送请求
	s.mu.RLock()
	clients := s.modelClients[req.Model]
	if len(clients) == 0 {
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "bad_request",
				"message": fmt.Sprintf("Unsupported model: %s", req.Model),
				"type":    "invalid_request_error",
				"param":   nil,
			},
		})
		return
	}
	clientID := clients[rand.Intn(len(clients))]
	client := s.clients[clientID]
	s.mu.RUnlock()

	// 在选择客户端并发送请求后添加：
	s.reqMu.Lock()
	s.requestClient[requestID] = clientID
	s.reqMu.Unlock()

	// 等待响应
	defer func() {
		s.reqMu.Lock()
		delete(s.requestClient, requestID)
		if ch, exists := s.pendingRequests[requestID]; exists {
			delete(s.pendingRequests, requestID)
			close(ch)
		}
		s.reqMu.Unlock()
	}()

	if err := client.writeJSON(req); err != nil {
		http.Error(w, "Failed to forward request", http.StatusInternalServerError)
		return
	}

	select {
	case resp := <-respChan:
		for k, values := range resp.Header {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		if resp.Type == types.TypeNormal {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write(resp.Body)
		} else {
			// 流式响应
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			flusher, _ := w.(http.Flusher)

			// 写入第一个chunk
			fmt.Fprintf(w, "data: %s\n\n", resp.Body)
			flusher.Flush()

			// 继续处理后续chunks
			for {
				select {
				case chunk := <-respChan:
					if chunk.Done {
						fmt.Fprintf(w, "data: [DONE]\n\n")
						flusher.Flush()
						return
					} else {
						fmt.Fprintf(w, "data: %s\n\n", chunk.Body)
						flusher.Flush()
					}
				case <-r.Context().Done():
					// 发送客户端关闭请求
					closeReq := types.ForwardRequest{
						Type:      types.TypeClientClose,
						RequestID: requestID,
					}
					if err := client.writeJSON(closeReq); err != nil {
						log.Printf("发送关闭请求失败: %v", err)
					}
					return
				}
			}
		}
	case <-r.Context().Done():
		// 发送客户端关闭请求
		closeReq := types.ForwardRequest{
			Type:      types.TypeClientClose,
			RequestID: requestID,
		}
		if err := client.writeJSON(closeReq); err != nil {
			log.Printf("发送关闭请求失败: %v", err)
		}
		return
	}
}

func (s *Server) unregisterClient(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	client, exists := s.clients[clientID]
	if !exists {
		return
	}

	// 清理模型到客户端的映射
	for _, model := range client.models {
		// 查找该模型对应的客户端列表
		if clients, ok := s.modelClients[model.ID]; ok {
			// 创建新切片排除当前客户端
			newClients := make([]string, 0, len(clients))
			for _, cid := range clients {
				if cid != clientID {
					newClients = append(newClients, cid)
				}
			}

			// 更新或删除映射
			if len(newClients) > 0 {
				s.modelClients[model.ID] = newClients
			} else {
				delete(s.modelClients, model.ID)
			}
		}
	}

	// 删除客户端记录
	delete(s.clients, clientID)
	log.Printf("客户端 %s 已注销，剩余客户端数: %d", clientID, len(s.clients))
}

func (s *Server) handleModelUpdate(update types.ModelUpdateRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	client := s.clients[update.ClientID]
	for _, model := range client.models {
		if clients, ok := s.modelClients[model.ID]; ok {
			newClients := make([]string, 0, len(clients))
			for _, cid := range clients {
				if cid != update.ClientID {
					newClients = append(newClients, cid)
				}
			}
			if len(newClients) > 0 {
				s.modelClients[model.ID] = newClients
			} else {
				delete(s.modelClients, model.ID)
			}
		}
	}

	client.models = update.Models
	for _, model := range update.Models {
		s.modelClients[model.ID] = append(s.modelClients[model.ID], update.ClientID)
	}

	log.Printf("已更新客户端 %s 的模型列表，当前模型数: %d", update.ClientID, len(update.Models))
}

func (s *Server) handleForceShutdown(req types.ForceShutdownRequest) {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()

	// 查找所有属于该客户端的请求
	var requestsToCancel []string
	for requestID, clientID := range s.requestClient {
		if clientID == req.ClientID {
			requestsToCancel = append(requestsToCancel, requestID)
		}
	}

	// 发送取消响应并清理
	for _, requestID := range requestsToCancel {
		if ch, exists := s.pendingRequests[requestID]; exists {
			// 发送超时响应
			ch <- types.ForwardResponse{
				RequestID:  requestID,
				StatusCode: http.StatusGatewayTimeout,
				Body:       []byte("Upstream client force shutdown"),
				Type:       types.TypeNormal,
				Done:       true,
			}
			close(ch)
			delete(s.pendingRequests, requestID)
			delete(s.requestClient, requestID)
		}
	}
	log.Printf("已强制关闭客户端 %s 的 %d 个待处理请求", req.ClientID, len(requestsToCancel))
}

func (s *Server) handleGetClient(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, s.clientBinaryPath)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "synapse-client"))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<html>
	<head>
		<title>Synapse Server</title>
	</head>
	<body>
		<h1>Synapse Server</h1>
		<p>Version: ` + s.version + `</p>
	</body>
</html>`))
}

func (s *Server) handleGetServerVersion(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(s.version))
}

func (s *Server) oneKeyScript(w http.ResponseWriter, r *http.Request) {
	// 获取协议和主机信息
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	// 处理反向代理的情况
	if forwardedProto := r.Header.Get("X-Forwarded-Proto"); forwardedProto != "" {
		proto = forwardedProto
	}

	// 构建WebSocket协议
	wsProto := "ws"
	if proto == "https" {
		wsProto = "wss"
	}

	// 获取完整主机地址
	host := r.Host
	serverURL := fmt.Sprintf("%s://%s", proto, host)
	wsURL := fmt.Sprintf("%s://%s/ws", wsProto, host)

	// 生成Bash脚本模板
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

wsUrl="%s"
serverUrl="%s"
serverVersion="%s"
synapseClientPath="$HOME/.local/bin/synapse-client"

function installClient() {
	# 自动安装客户端脚本
	echo "下载客户端..."
	if command -v curl >/dev/null 2>&1; then
		curl -L -o "$synapseClientPath" "$serverUrl/getclient"
	elif command -v wget >/dev/null 2>&1; then
		wget -O "$synapseClientPath" "$serverUrl/getclient"
	else
		echo "错误: 需要 curl 或 wget 来下载客户端"
		exit 1
	fi
	echo "下载客户端到 $synapseClientPath"
}

if [ -f "$synapseClientPath" ]; then
	clientVersion=$("$synapseClientPath" --version)
	if [ "$clientVersion" != "$serverVersion" ]; then
		installClient
	fi
else
	installClient
fi

chmod +x "$synapseClientPath"
echo "启动客户端连接服务器: $wsUrl"
"$synapseClientPath" --server-url "$wsUrl" "$@"
`, wsURL, serverURL, s.version)

	// 设置响应头
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Header().Set("Content-Disposition", "attachment; filename=install-client.sh")

	// 发送脚本内容
	if _, err := io.WriteString(w, script); err != nil {
		log.Printf("发送一键安装脚本失败: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *Server) Start(host string, port string) error {
	http.HandleFunc("/ws", s.handleWebSocket)
	http.HandleFunc("/v1/", s.handleAPIRequest)
	http.HandleFunc("/getclient", s.handleGetClient)
	http.HandleFunc("/", s.handleIndex)
	http.HandleFunc("/version", s.handleGetServerVersion)
	http.HandleFunc("/run", s.oneKeyScript)
	return http.ListenAndServe(host+":"+port, nil)
}
