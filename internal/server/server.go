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
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/zeyugao/synapse/internal/types"
)

type Server struct {
	clients                      map[string]*Client
	modelClients                 map[string][]string // model -> []clientID
	mu                           sync.RWMutex
	upgrader                     websocket.Upgrader
	pendingRequests              map[string]chan types.ForwardResponse
	reqMu                        sync.RWMutex
	apiAuthKey                   string
	wsAuthKey                    string
	requestClient                map[string]string
	version                      string
	clientBinaryPath             string
	abortOnClientVersionMismatch bool
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

func NewServer(apiAuthKey, wsAuthKey string, version string, clientBinaryPath string, abortOnClientVersionMismatch bool) *Server {
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
		requestClient:                make(map[string]string),
		version:                      version,          // Set version
		clientBinaryPath:             clientBinaryPath, // Add initialization
		abortOnClientVersionMismatch: abortOnClientVersionMismatch,
	}
}

func generateRequestID() string {
	b := make([]byte, 16)
	cryptoRand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Check WebSocket authentication
	if s.wsAuthKey != "" {
		authKey := r.URL.Query().Get("ws_auth_key")
		if authKey != s.wsAuthKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		http.Error(w, "Could not upgrade connection", http.StatusInternalServerError)
		return
	}

	// Wait for client registration information
	var registration types.ClientRegistration
	if err := conn.ReadJSON(&registration); err != nil {
		log.Printf("Failed to read registration information: %v", err)
		conn.Close()
		return
	}

	// Version check logic
	if registration.Version == "" {
		log.Printf("Client %s did not provide a version number, connection refused", registration.ClientID)
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "Version required"))
		conn.Close()
		return
	}

	if registration.Version != s.version {
		log.Printf("Client version mismatch (client: %s, server: %s)", registration.Version, s.version)
		if s.abortOnClientVersionMismatch {
			log.Printf("Aborting server because client version mismatch")
			conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4001, fmt.Sprintf("Client version %s does not match server version %s", registration.Version, s.version)))
			conn.Close()
			return
		}
	}

	log.Printf("New client connected: %s, registered models: %d", registration.ClientID, len(registration.Models))

	s.mu.Lock()
	client := &Client{
		conn:   conn,
		models: registration.Models,
	}
	s.clients[registration.ClientID] = client

	// Update model to client mapping
	for _, model := range registration.Models {
		s.modelClients[model.ID] = append(s.modelClients[model.ID], registration.ClientID)
	}
	s.mu.Unlock()

	// Add goroutine for handling responses
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
			log.Printf("Failed to read client %s message: %v", clientID, err)
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
			// Send pong response
			pongReq := types.ForwardRequest{
				Type: types.TypePong,
			}
			if err := client.writeJSON(pongReq); err != nil {
				log.Printf("Failed to send pong response: %v", err)
			}
		case types.TypeModelUpdate:
			var updateReq types.ModelUpdateRequest
			if err := json.Unmarshal(msg.Body, &updateReq); err != nil {
				log.Printf("Failed to parse model update request: %v", err)
				return
			}
			s.handleModelUpdate(updateReq)
		case types.TypeUnregister:
			var unregisterReq types.UnregisterRequest
			if err := json.Unmarshal(msg.Body, &unregisterReq); err != nil {
				log.Printf("Failed to parse unregister request: %v", err)
				return
			}
			log.Printf("Received unregister request from client %s", unregisterReq.ClientID)
			s.unregisterClient(unregisterReq.ClientID)
			return
		case types.TypeForceShutdown:
			var forceReq types.ForceShutdownRequest
			if err := json.Unmarshal(msg.Body, &forceReq); err != nil {
				log.Printf("Failed to parse force shutdown request: %v", err)
				return
			}
			s.handleForceShutdown(forceReq)
		default:
			log.Printf("Unknown message type: %d", msg.Type)
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

	// Collect all unique models and add client count
	modelMap := make(map[string]struct {
		types.ModelInfo
		ClientCount int `json:"client_count"`
	})

	// First collect all model information
	for _, client := range s.clients {
		for _, model := range client.models {
			if existing, exists := modelMap[model.ID]; exists {
				// If the model already exists, only update the client count
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

	// Convert to slice
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
	// Check API authentication
	if s.apiAuthKey != "" {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer "+s.apiAuthKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	path := r.URL.Path
	if strings.HasPrefix(path, "/v1/") {
		// Remove the /v1 prefix from the path
		path = strings.TrimPrefix(path, "/v1")
	}

	if path == "/models" {
		s.handleModels(w, r)
		return
	}

	// Read request body
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

	// Generate request ID
	requestID := generateRequestID()

	// Create response channel
	respChan := make(chan types.ForwardResponse, 1)
	s.reqMu.Lock()
	s.pendingRequests[requestID] = respChan
	s.reqMu.Unlock()

	// Build forward request
	req := types.ForwardRequest{
		Type:      types.TypeNormal,
		RequestID: requestID,
		Model:     modelReq.Model,
		Method:    r.Method,
		Path:      path,
		Query:     r.URL.RawQuery,
		Header:    r.Header.Clone(),
		Body:      body,
	}

	// Select client and send request
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

	// Add after selecting client and sending request:
	s.reqMu.Lock()
	s.requestClient[requestID] = clientID
	s.reqMu.Unlock()

	// Wait for response
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
			// Streaming response
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			flusher, _ := w.(http.Flusher)

			// Write the first chunk
			fmt.Fprintf(w, "data: %s\n\n", resp.Body)
			flusher.Flush()

			// Continue processing subsequent chunks
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
					// Send client close request
					closeReq := types.ForwardRequest{
						Type:      types.TypeClientClose,
						RequestID: requestID,
					}
					if err := client.writeJSON(closeReq); err != nil {
						log.Printf("Failed to send close request: %v", err)
					}
					return
				}
			}
		}
	case <-r.Context().Done():
		// Send client close request
		closeReq := types.ForwardRequest{
			Type:      types.TypeClientClose,
			RequestID: requestID,
		}
		if err := client.writeJSON(closeReq); err != nil {
			log.Printf("Failed to send close request: %v", err)
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

	// Clean up model to client mapping
	for _, model := range client.models {
		// Find the client list corresponding to the model
		if clients, ok := s.modelClients[model.ID]; ok {
			// Create a new slice excluding the current client
			newClients := make([]string, 0, len(clients))
			for _, cid := range clients {
				if cid != clientID {
					newClients = append(newClients, cid)
				}
			}

			// Update or delete the mapping
			if len(newClients) > 0 {
				s.modelClients[model.ID] = newClients
			} else {
				delete(s.modelClients, model.ID)
			}
		}
	}

	// Delete client record
	delete(s.clients, clientID)
	log.Printf("Client %s unregistered, remaining clients: %d", clientID, len(s.clients))
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

	log.Printf("Updated model list for client %s, current number of models: %d", update.ClientID, len(update.Models))
}

func (s *Server) handleForceShutdown(req types.ForceShutdownRequest) {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()

	// Find all requests belonging to the client
	var requestsToCancel []string
	for requestID, clientID := range s.requestClient {
		if clientID == req.ClientID {
			requestsToCancel = append(requestsToCancel, requestID)
		}
	}

	// Send cancel response and clean up
	for _, requestID := range requestsToCancel {
		if ch, exists := s.pendingRequests[requestID]; exists {
			// Send timeout response
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
	log.Printf("Forcibly closed %d pending requests for client %s", len(requestsToCancel), req.ClientID)
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
	// Get protocol and host information
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	// Handle reverse proxy cases
	if forwardedProto := r.Header.Get("X-Forwarded-Proto"); forwardedProto != "" {
		proto = forwardedProto
	}

	// Build WebSocket protocol
	wsProto := "ws"
	if proto == "https" {
		wsProto = "wss"
	}

	// Get full host address
	host := r.Host
	serverURL := fmt.Sprintf("%s://%s", proto, host)
	wsURL := fmt.Sprintf("%s://%s/ws", wsProto, host)

	// Generate Bash script template
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -xeuo pipefail

wsUrl="%s"
serverUrl="%s"
serverVersion="%s"
synapseClientPath="$HOME/.local/bin/synapse-client"

function installClient() {
    # Automatically install client script
    echo "Downloading client..."
    mkdir -p "$(dirname "$synapseClientPath")"
    if command -v curl >/dev/null 2>&1; then
        curl -L -o "$synapseClientPath" "$serverUrl/getclient"
    elif command -v wget >/dev/null 2>&1; then
        wget -O "$synapseClientPath" "$serverUrl/getclient"
    else
        echo "Error: curl or wget is required to download the client"
        exit 1
    fi
    echo "Downloaded client to $synapseClientPath"
}

if [ -f "$synapseClientPath" ]; then
    clientVersion=$("$synapseClientPath" --version | awk -F': ' '{print $2}' | tr -d '\n')
    if [ "$clientVersion" != "$serverVersion" ]; then
        echo "Client version mismatch, reinstalling"
        installClient
    fi
else
    installClient
fi

chmod +x "$synapseClientPath"
echo "Starting client to connect to server: $wsUrl"
exec "$synapseClientPath" --server-url "$wsUrl" "$@"
`, wsURL, serverURL, s.version)

	// Set response headers
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Header().Set("Content-Disposition", "attachment; filename=install-client.sh")

	// Send script content
	if _, err := io.WriteString(w, script); err != nil {
		log.Printf("Failed to send one-key installation script: %v", err)
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
