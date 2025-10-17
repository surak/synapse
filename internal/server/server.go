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
	"os"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/zeyugao/synapse/internal/types"
	pb "github.com/zeyugao/synapse/internal/types/proto"
	"google.golang.org/protobuf/proto"
)

type Server struct {
	clients                      map[string]*Client
	modelClients                 map[string][]string // model -> []clientID
	mu                           sync.RWMutex
	upgrader                     websocket.Upgrader
	pendingRequests              map[string]chan *types.ForwardResponse
	reqMu                        sync.RWMutex
	apiAuthKey                   string
	wsAuthKey                    string
	clientRequests               map[string]map[string]struct{} // clientID -> set of requestIDs
	version                      string
	clientBinaryPath             string
	abortOnClientVersionMismatch bool
}

type Client struct {
	conn   *websocket.Conn
	models []*types.ModelInfo
	mu     sync.Mutex
}

func (c *Client) writeProto(msg proto.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

func NewServer(apiAuthKey, wsAuthKey string, version string, clientBinaryPath string, abortOnClientVersionMismatch bool) *Server {
	return &Server{
		clients:         make(map[string]*Client),
		modelClients:    make(map[string][]string),
		pendingRequests: make(map[string]chan *types.ForwardResponse),
		clientRequests:  make(map[string]map[string]struct{}),
		apiAuthKey:      apiAuthKey,
		wsAuthKey:       wsAuthKey,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		version:                      version,
		clientBinaryPath:             clientBinaryPath,
		abortOnClientVersionMismatch: abortOnClientVersionMismatch,
	}
}

func generateRequestID() string {
	b := make([]byte, 16)
	cryptoRand.Read(b)
	return hex.EncodeToString(b)
}

func readClientMessage(conn *websocket.Conn) (*types.ClientMessage, error) {
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}

	msg := &types.ClientMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil, err
	}
	return msg, nil
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

	initialMsg, err := readClientMessage(conn)
	if err != nil {
		log.Printf("Failed to read registration information: %v", err)
		conn.Close()
		return
	}

	registration := initialMsg.GetRegistration()
	if registration == nil {
		log.Printf("Client did not send registration as first message, connection refused")
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "Registration required"))
		conn.Close()
		return
	}

	// Version check logic
	clientID := registration.GetClientId()
	version := registration.GetVersion()
	if version == "" {
		log.Printf("Client %s did not provide a version number, connection refused", clientID)
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "Version required"))
		conn.Close()
		return
	}

	if version != s.version {
		log.Printf("Client version mismatch (client: %s, server: %s)", version, s.version)
		if s.abortOnClientVersionMismatch {
			log.Printf("Aborting server because client version mismatch")
			conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4001, fmt.Sprintf("Client version %s does not match server version %s", version, s.version)))
			conn.Close()
			return
		}
	}

	models := registration.GetModels()
	log.Printf("New client connected: %s, registered models: %d", clientID, len(models))

	s.mu.Lock()
	client := &Client{
		conn:   conn,
		models: append([]*types.ModelInfo(nil), models...),
	}
	s.clients[clientID] = client

	// Update model to client mapping
	for _, model := range models {
		if model == nil {
			continue
		}
		s.modelClients[model.GetId()] = append(s.modelClients[model.GetId()], clientID)
	}
	s.mu.Unlock()

	// Add goroutine for handling responses
	go s.handleClientResponses(clientID, client)
}

func (s *Server) handleClientResponses(clientID string, client *Client) {
	defer func() {
		client.conn.Close()
		s.unregisterClient(clientID)
	}()

	for {
		msg, err := readClientMessage(client.conn)
		if err != nil {
			log.Printf("Failed to read client %s message: %v", clientID, err)
			return
		}

		switch payload := msg.Message.(type) {
		case *pb.ClientMessage_ForwardResponse:
			if payload.ForwardResponse == nil {
				continue
			}
			s.handleForwardResponse(payload.ForwardResponse)
		case *pb.ClientMessage_Heartbeat:
			if err := client.writeProto(&types.ServerMessage{
				Message: &pb.ServerMessage_Pong{
					Pong: &types.Pong{},
				},
			}); err != nil {
				log.Printf("Failed to send pong response: %v", err)
			}
		case *pb.ClientMessage_ModelUpdate:
			if payload.ModelUpdate == nil {
				continue
			}
			s.handleModelUpdate(payload.ModelUpdate)
		case *pb.ClientMessage_Unregister:
			if payload.Unregister == nil {
				continue
			}
			log.Printf("Received unregister request from client %s", payload.Unregister.GetClientId())
			s.unregisterClient(payload.Unregister.GetClientId())
			return
		case *pb.ClientMessage_ForceShutdown:
			if payload.ForceShutdown == nil {
				continue
			}
			s.handleForceShutdown(payload.ForceShutdown)
		default:
			log.Printf("Unknown client message type: %T", payload)
		}
	}
}

func (s *Server) handleForwardResponse(resp *types.ForwardResponse) {
	requestID := resp.GetRequestId()

	s.reqMu.RLock()
	respChan, exists := s.pendingRequests[requestID]
	s.reqMu.RUnlock()

	if !exists || respChan == nil {
		return
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				// Channel may have been closed concurrently; ignore.
			}
		}()
		respChan <- resp
	}()

	kind := resp.GetKind()
	if (kind == types.ResponseKindStream && resp.GetDone()) || kind == types.ResponseKindNormal {
		s.reqMu.Lock()
		if ch, ok := s.pendingRequests[requestID]; ok && ch == respChan {
			delete(s.pendingRequests, requestID)
			close(respChan)
		}
		s.reqMu.Unlock()
	}
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Collect all unique models and add client count
	modelMap := make(map[string]struct {
		*types.ModelInfo
		ClientCount int `json:"client_count"`
	})

	// First collect all model information
	for _, client := range s.clients {
		for _, model := range client.models {
			if model == nil {
				continue
			}
			modelID := model.GetId()
			if existing, exists := modelMap[modelID]; exists {
				// If the model already exists, only update the client count
				existing.ClientCount = len(s.modelClients[modelID])
				modelMap[modelID] = existing
			} else {
				modelMap[modelID] = struct {
					*types.ModelInfo
					ClientCount int `json:"client_count"`
				}{
					ModelInfo:   model,
					ClientCount: len(s.modelClients[modelID]),
				}
			}
		}
	}

	// Convert to slice
	models := make([]struct {
		*types.ModelInfo
		ClientCount int `json:"client_count"`
	}, 0, len(modelMap))
	for _, model := range modelMap {
		models = append(models, model)
	}

	response := struct {
		Object string `json:"object"`
		Data   []struct {
			*types.ModelInfo
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
	respChan := make(chan *types.ForwardResponse, 1)
	s.reqMu.Lock()
	s.pendingRequests[requestID] = respChan
	s.reqMu.Unlock()

	// Build forward request
	req := &types.ForwardRequest{
		RequestId: requestID,
		Model:     modelReq.Model,
		Method:    r.Method,
		Path:      path,
		Query:     r.URL.RawQuery,
		Header:    types.HTTPHeaderToProto(r.Header.Clone()),
		Body:      body,
	}

	// Select client based on model and load balancing
	s.mu.RLock()
	clientIDsForModel, modelSupported := s.modelClients[req.Model]

	var candidateClients []*Client
	var candidateClientIDs []string

	if modelSupported && len(clientIDsForModel) > 0 {
		for _, cid := range clientIDsForModel {
			if c, exists := s.clients[cid]; exists {
				candidateClients = append(candidateClients, c)
				candidateClientIDs = append(candidateClientIDs, cid)
			}
		}
	}
	s.mu.RUnlock()

	if len(candidateClients) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "bad_request",
				"message": fmt.Sprintf("Unsupported model or no available clients: %s", req.Model),
				"type":    "invalid_request_error",
				"param":   nil,
			},
		})
		return
	}

	var bestClient *Client
	var bestClientID string
	minLoad := -1
	var tiedClients []*Client
	var tiedClientIDs []string

	for i, currentClient := range candidateClients {
		currentClientID := candidateClientIDs[i]
		s.reqMu.RLock()
		load := 0
		if reqs, ok := s.clientRequests[currentClientID]; ok {
			load = len(reqs)
		}
		s.reqMu.RUnlock()

		if bestClient == nil || load < minLoad {
			minLoad = load
			bestClient = currentClient
			bestClientID = currentClientID
			tiedClients = []*Client{currentClient}
			tiedClientIDs = []string{currentClientID}
		} else if load == minLoad {
			tiedClients = append(tiedClients, currentClient)
			tiedClientIDs = append(tiedClientIDs, currentClientID)
		}
	}

	if len(tiedClients) > 1 {
		randomIndex := rand.Intn(len(tiedClients))
		bestClient = tiedClients[randomIndex]
		bestClientID = tiedClientIDs[randomIndex]
	}

	client := bestClient     // Use this client object to send the request
	clientID := bestClientID // Use this clientID for tracking and logging

	// Add request to client's active requests map
	s.reqMu.Lock()
	if s.clientRequests[clientID] == nil {
		s.clientRequests[clientID] = make(map[string]struct{})
	}
	s.clientRequests[clientID][requestID] = struct{}{}
	s.reqMu.Unlock()

	// Wait for response
	// The clientID variable from the outer scope (bestClientID) is captured by this defer.
	defer func() {
		s.reqMu.Lock()
		// Remove request from the client's active request set
		if reqs, ok := s.clientRequests[clientID]; ok {
			delete(reqs, requestID)
			if len(reqs) == 0 {
				delete(s.clientRequests, clientID)
			}
		}
		// Clean up pendingRequests
		if ch, exists := s.pendingRequests[requestID]; exists {
			delete(s.pendingRequests, requestID)
			close(ch)
		}
		s.reqMu.Unlock()
	}()

	if err := client.writeProto(&types.ServerMessage{
		Message: &pb.ServerMessage_ForwardRequest{
			ForwardRequest: req,
		},
	}); err != nil {
		http.Error(w, "Failed to forward request", http.StatusInternalServerError)
		return
	}

	select {
	case resp, ok := <-respChan:
		if !ok || resp == nil {
			return
		}
		for k, values := range types.ProtoToHTTPHeader(resp.GetHeader()) {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		if resp.GetKind() == types.ResponseKindNormal {
			if resp.GetStatusCode() != 0 {
				w.WriteHeader(int(resp.GetStatusCode()))
			}
			if len(resp.GetBody()) > 0 {
				w.Write(resp.GetBody())
			}
		} else {
			// Streaming response
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			flusher, _ := w.(http.Flusher)

			// Write the first chunk
			fmt.Fprintf(w, "data: %s\n\n", resp.GetBody())
			flusher.Flush()

			// Continue processing subsequent chunks
			for {
				select {
				case chunk, ok := <-respChan:
					if !ok || chunk == nil {
						return
					}
					if chunk.GetDone() {
						fmt.Fprintf(w, "data: [DONE]\n\n")
						flusher.Flush()
						return
					} else {
						fmt.Fprintf(w, "data: %s\n\n", chunk.GetBody())
						flusher.Flush()
					}
				case <-r.Context().Done():
					// Send client close request
					closeReq := &types.ServerMessage{
						Message: &pb.ServerMessage_ClientClose{
							ClientClose: &types.ClientClose{
								RequestId: requestID,
							},
						},
					}
					if err := client.writeProto(closeReq); err != nil {
						log.Printf("Failed to send close request: %v", err)
					}
					return
				}
			}
		}
	case <-r.Context().Done():
		// Send client close request
		closeReq := &types.ServerMessage{
			Message: &pb.ServerMessage_ClientClose{
				ClientClose: &types.ClientClose{
					RequestId: requestID,
				},
			},
		}
		if err := client.writeProto(closeReq); err != nil {
			log.Printf("Failed to send close request: %v", err)
		}
		return
	}
}

func (s *Server) unregisterClient(clientID string) {
	s.mu.Lock()
	client, exists := s.clients[clientID]
	if !exists {
		s.mu.Unlock()
		return
	}

	// Clean up model to client mapping
	for _, model := range client.models {
		if model == nil {
			continue
		}
		modelID := model.GetId()
		if clients, ok := s.modelClients[modelID]; ok {
			newClients := make([]string, 0, len(clients))
			for _, cid := range clients {
				if cid != clientID {
					newClients = append(newClients, cid)
				}
			}

			if len(newClients) > 0 {
				s.modelClients[modelID] = newClients
			} else {
				delete(s.modelClients, modelID)
			}
		}
	}

	delete(s.clients, clientID)
	currentClientCount := len(s.clients)
	s.mu.Unlock()

	// Clean up clientRequests associated with this client
	s.reqMu.Lock()
	delete(s.clientRequests, clientID)
	s.reqMu.Unlock()

	log.Printf("Client %s unregistered, remaining clients: %d", clientID, currentClientCount)
}

func (s *Server) handleModelUpdate(update *types.ModelUpdateRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	client := s.clients[update.GetClientId()]
	if client == nil {
		log.Printf("Received model update for unknown client %s", update.GetClientId())
		return
	}
	for _, model := range client.models {
		if model == nil {
			continue
		}
		modelID := model.GetId()
		if clients, ok := s.modelClients[modelID]; ok {
			newClients := make([]string, 0, len(clients))
			for _, cid := range clients {
				if cid != update.GetClientId() {
					newClients = append(newClients, cid)
				}
			}
			if len(newClients) > 0 {
				s.modelClients[modelID] = newClients
			} else {
				delete(s.modelClients, modelID)
			}
		}
	}

	client.models = append([]*types.ModelInfo(nil), update.GetModels()...)
	for _, model := range update.GetModels() {
		if model == nil {
			continue
		}
		modelID := model.GetId()
		s.modelClients[modelID] = append(s.modelClients[modelID], update.GetClientId())
	}

	log.Printf("Updated model list for client %s, current number of models: %d", update.GetClientId(), len(update.GetModels()))
}

func (s *Server) handleForceShutdown(req *types.ForceShutdownRequest) {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()

	var requestsToCancel []string
	if clientActiveRequests, clientExists := s.clientRequests[req.GetClientId()]; clientExists {
		for requestID := range clientActiveRequests {
			requestsToCancel = append(requestsToCancel, requestID)
		}
	}

	if len(requestsToCancel) == 0 {
		log.Printf("No pending requests found to forcibly close for client %s", req.GetClientId())
		return
	}

	// Send cancel response and clean up
	for _, requestID := range requestsToCancel {
		if ch, exists := s.pendingRequests[requestID]; exists {
			ch <- &types.ForwardResponse{
				RequestId:  requestID,
				StatusCode: int32(http.StatusGatewayTimeout),
				Body:       []byte("Upstream client force shutdown"),
				Kind:       types.ResponseKindNormal,
				Done:       true,
			}
			close(ch)
			delete(s.pendingRequests, requestID)
		}
		// Remove from clientRequests for this specific client (req.ClientId) and this requestID
		if reqsForClient, ok := s.clientRequests[req.GetClientId()]; ok {
			delete(reqsForClient, requestID)
			if len(reqsForClient) == 0 {
				delete(s.clientRequests, req.GetClientId())
			}
		}
	}
	log.Printf("Forcibly closed %d pending requests for client %s", len(requestsToCancel), req.GetClientId())
}

func (s *Server) handleGetClient(w http.ResponseWriter, r *http.Request) {
	// Check if client binary exists
	if _, err := os.Stat(s.clientBinaryPath); os.IsNotExist(err) {
		log.Printf("Client binary not found at %s", s.clientBinaryPath)
		http.Error(w, "Client binary not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "synapse-client"))
	http.ServeFile(w, r, s.clientBinaryPath)
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
	// Check if client binary exists
	if _, err := os.Stat(s.clientBinaryPath); os.IsNotExist(err) {
		log.Printf("Client binary not found at %s", s.clientBinaryPath)
		http.Error(w, "Client binary not available, cannot generate installation script", http.StatusServiceUnavailable)
		return
	}

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
