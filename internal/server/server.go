package server

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/zeyugao/synapse/internal/types"
)

type Server struct {
	clients      map[string]*Client
	modelClients map[string][]string // model -> []clientID
	mu           sync.RWMutex
	upgrader     websocket.Upgrader
}

type Client struct {
	conn   *websocket.Conn
	models []types.ModelInfo
}

func NewServer() *Server {
	return &Server{
		clients:      make(map[string]*Client),
		modelClients: make(map[string][]string),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "Could not upgrade connection", http.StatusInternalServerError)
		return
	}

	// 等待客户端注册信息
	var registration types.ClientRegistration
	if err := conn.ReadJSON(&registration); err != nil {
		conn.Close()
		return
	}

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

	// 保持连接并处理断开
	go s.handleClientDisconnect(registration.ClientID, conn)
}

func (s *Server) handleClientDisconnect(clientID string, conn *websocket.Conn) {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			s.mu.Lock()
			delete(s.clients, clientID)
			// 清理模型映射
			for model, clients := range s.modelClients {
				newClients := make([]string, 0)
				for _, cid := range clients {
					if cid != clientID {
						newClients = append(newClients, cid)
					}
				}
				if len(newClients) == 0 {
					delete(s.modelClients, model)
				} else {
					s.modelClients[model] = newClients
				}
			}
			s.mu.Unlock()
			return
		}
	}
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 收集所有唯一的模型
	modelMap := make(map[string]types.ModelInfo)
	for _, client := range s.clients {
		for _, model := range client.models {
			modelMap[model.ID] = model
		}
	}

	models := make([]types.ModelInfo, 0, len(modelMap))
	for _, model := range modelMap {
		models = append(models, model)
	}

	response := types.ModelsResponse{
		Object: "list",
		Data:   models,
	}

	json.NewEncoder(w).Encode(response)
}

func (s *Server) handleAPIRequest(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" {
		s.handleModels(w, r)
		return
	}

	// 读取并保留原始请求体
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(body)) // 重置body供后续使用

	// 从请求体解析model字段
	var modelReq struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &modelReq); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// 构建转发请求
	req := types.ForwardRequest{
		Model:  modelReq.Model,
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Header: r.Header.Clone(),
		Body:   body,
	}

	s.mu.RLock()
	clients := s.modelClients[req.Model]
	if len(clients) == 0 {
		s.mu.RUnlock()
		http.Error(w, "Model not found", http.StatusNotFound)
		return
	}

	// 随机选择一个客户端
	clientID := clients[rand.Intn(len(clients))]
	client := s.clients[clientID]
	s.mu.RUnlock()

	// 使用 WebSocket 转发请求
	if err := client.conn.WriteJSON(req); err != nil {
		http.Error(w, "Failed to forward request", http.StatusInternalServerError)
		return
	}

	// 创建用于转发响应的 WebSocket 连接
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "Could not upgrade connection", http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	// 转发响应
	for {
		messageType, message, err := client.conn.ReadMessage()
		if err != nil {
			break
		}
		if err := conn.WriteMessage(messageType, message); err != nil {
			break
		}
	}
}

func (s *Server) Start(host string, port string) error {
	http.HandleFunc("/ws", s.handleWebSocket)
	http.HandleFunc("/v1/", s.handleAPIRequest)
	return http.ListenAndServe(host+":"+port, nil)
}
