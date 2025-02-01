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
	clients         map[string]*Client
	modelClients    map[string][]string // model -> []clientID
	mu              sync.RWMutex
	upgrader        websocket.Upgrader
	pendingRequests map[string]chan types.ForwardResponse
	reqMu           sync.RWMutex
	apiAuthKey      string // 新增API鉴权密钥
	wsAuthKey       string // 新增WebSocket鉴权密钥
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

func NewServer(apiAuthKey, wsAuthKey string) *Server {
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
	log.Printf("新客户端连接: %s, 注册模型数: %d", registration.ClientID, len(registration.Models))

	s.mu.Lock()
	s.clients[registration.ClientID] = &Client{
		conn:   conn,
		models: registration.Models,
	}

	// 更新模型到客户端的映射
	for _, model := range registration.Models {
		s.modelClients[model.ID] = append(s.modelClients[model.ID], registration.ClientID)
	}
	s.mu.Unlock()

	// 添加处理响应的 goroutine
	go s.handleClientResponses(registration.ClientID, conn)
}

func (s *Server) handleClientResponses(clientID string, conn *websocket.Conn) {
	defer func() {
		s.unregisterClient(clientID)
		conn.Close()
	}()

	for {
		var msg types.ForwardResponse

		if err := conn.ReadJSON(&msg); err != nil {
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
			if err := conn.WriteJSON(pongReq); err != nil {
				log.Printf("发送pong响应失败: %v", err)
			}
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
		http.Error(w, "Model not found", http.StatusNotFound)
		return
	}
	clientID := clients[rand.Intn(len(clients))]
	client := s.clients[clientID]
	s.mu.RUnlock()

	if err := client.writeJSON(req); err != nil {
		s.reqMu.Lock()
		delete(s.pendingRequests, requestID)
		close(respChan)
		s.reqMu.Unlock()
		http.Error(w, "Failed to forward request", http.StatusInternalServerError)
		return
	}

	// 等待响应
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
					return
				}
			}
		}
	case <-r.Context().Done():
		s.reqMu.Lock()
		delete(s.pendingRequests, requestID)
		close(respChan)
		s.reqMu.Unlock()
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

func (s *Server) Start(host string, port string) error {
	http.HandleFunc("/ws", s.handleWebSocket)
	http.HandleFunc("/v1/", s.handleAPIRequest)
	return http.ListenAndServe(host+":"+port, nil)
}
