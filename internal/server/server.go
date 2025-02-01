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
}

type Client struct {
	conn   *websocket.Conn
	models []types.ModelInfo
}

func NewServer() *Server {
	return &Server{
		clients:         make(map[string]*Client),
		modelClients:    make(map[string][]string),
		pendingRequests: make(map[string]chan types.ForwardResponse),
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
	for {
		var resp types.ForwardResponse

		if err := conn.ReadJSON(&resp); err != nil {
			log.Printf("读取客户端 %s 响应失败: %v", clientID, err)
			return
		}

		s.reqMu.RLock()
		respChan, exists := s.pendingRequests[resp.RequestID]
		s.reqMu.RUnlock()

		if exists {
			// 处理流式结束标记
			if resp.Done {
				close(respChan)
				s.reqMu.Lock()
				delete(s.pendingRequests, resp.RequestID)
				s.reqMu.Unlock()
				continue
			}

			// 发送响应数据
			respChan <- resp

			// 普通响应需要关闭通道
			if resp.Type == types.TypeNormal {
				s.reqMu.Lock()
				delete(s.pendingRequests, resp.RequestID)
				close(respChan)
				s.reqMu.Unlock()
			}
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

	// 读取请求体
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	log.Printf("收到请求: %s", string(body))

	var modelReq struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &modelReq); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// 生成请求ID
	requestID := generateRequestID()

	log.Printf("请求ID: %s", requestID)

	// 创建响应通道
	respChan := make(chan types.ForwardResponse, 1)
	s.reqMu.Lock()
	s.pendingRequests[requestID] = respChan
	s.reqMu.Unlock()

	// 构建转发请求
	req := types.ForwardRequest{
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

	log.Printf("选择客户端: %s", clientID)

	if err := client.conn.WriteJSON(req); err != nil {
		s.reqMu.Lock()
		delete(s.pendingRequests, requestID)
		close(respChan)
		s.reqMu.Unlock()
		http.Error(w, "Failed to forward request", http.StatusInternalServerError)
		return
	}

	log.Printf("已发送请求到客户端: %s", clientID)

	// 等待响应
	select {
	case resp := <-respChan:
		log.Printf("收到响应: %+v", resp)
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
					fmt.Fprintf(w, "data: %s\n\n", chunk.Body)
					flusher.Flush()
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

func (s *Server) Start(host string, port string) error {
	http.HandleFunc("/ws", s.handleWebSocket)
	http.HandleFunc("/v1/", s.handleAPIRequest)
	return http.ListenAndServe(host+":"+port, nil)
}
