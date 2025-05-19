package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bufio"

	"github.com/gorilla/websocket"
	"github.com/zeyugao/synapse/internal/types"
)

type Client struct {
	BaseUrl         string
	ServerURL       string
	ClientID        string
	WSAuthKey       string
	ApiKey          string
	models          []types.ModelInfo
	conn            *websocket.Conn
	mu              sync.Mutex
	reconnMu        sync.Mutex
	reconnecting    bool
	closing         bool
	heartbeatTicker *time.Ticker
	cancelMap       map[string]context.CancelFunc
	syncTicker      *time.Ticker
	shutdownSignal  chan struct{}
	shutdownOnce    sync.Once
	activeRequests  int64 // Atomic counter
	Version         string
	connClosed      chan struct{} // Channel to signal connection closed
}

func NewClient(baseUrl, serverURL string, version string) *Client {
	client := &Client{
		BaseUrl:         baseUrl,
		ServerURL:       serverURL,
		ClientID:        generateClientID(),
		Version:         version,
		cancelMap:       make(map[string]context.CancelFunc),
		heartbeatTicker: time.NewTicker(15 * time.Second),
		syncTicker:      time.NewTicker(30 * time.Second),
		shutdownSignal:  make(chan struct{}),
		connClosed:      make(chan struct{}),
	}

	// Start the background goroutines only once
	go client.heartbeatLoop()
	go client.modelSyncLoop()
	go client.WaitForShutdown()

	return client
}

func (c *Client) fetchModels(silent bool) ([]types.ModelInfo, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/models", c.BaseUrl), nil)
	if err != nil {
		if !silent {
			log.Printf("Failed to create model request: %v", err)
		}
		return nil, err
	}

	if c.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.ApiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if !silent {
			log.Printf("Failed to get model list: %v", err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	var modelsResp types.ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		if !silent {
			log.Printf("Failed to parse model response: %v", err)
		}
		return nil, err
	}

	if !silent {
		log.Printf("Got %d models", len(modelsResp.Data))
	}

	return modelsResp.Data, nil
}

func (c *Client) Connect() error {
	models, err := c.fetchModels(false)
	if err != nil {
		return err
	}
	c.models = models

	// Use the configured server URL directly
	wsURL := c.ServerURL
	if c.WSAuthKey != "" {
		wsURL += "?ws_auth_key=" + c.WSAuthKey
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Printf("Failed to connect to server: %v", err)
		return err
	}

	// Reset connClosed channel if it was closed
	select {
	case <-c.connClosed:
		c.connClosed = make(chan struct{})
	default:
	}

	c.conn = conn
	serverToPrint := wsURL
	if c.WSAuthKey != "" {
		serverToPrint = strings.Replace(wsURL, c.WSAuthKey, "...", -1)
	}
	log.Printf("Successfully connected to server %s", serverToPrint)

	registration := types.ClientRegistration{
		ClientID: c.ClientID,
		Models:   c.models,
		Version:  c.Version,
	}
	if err := conn.WriteJSON(registration); err != nil {
		log.Printf("Failed to send registration information: %v", err)
		conn.Close()
		return err
	}
	log.Printf("Sent client registration information, ID: %s", c.ClientID)

	// Start the connection handler in a separate goroutine
	go c.handleRequests()

	return nil
}

// heartbeatLoop replaces startHeartbeat and runs continuously
func (c *Client) heartbeatLoop() {
	for range c.heartbeatTicker.C {
		if c.closing {
			return
		}

		if c.isReconnecting() || c.conn == nil {
			time.Sleep(1 * time.Second)
			continue
		}

		heartbeat := types.ForwardResponse{
			Timestamp: time.Now().Unix(),
			Type:      types.TypeHeartbeat,
		}
		if err := c.writeJSON(heartbeat); err != nil {
			log.Printf("Failed to send heartbeat: %v", err)
			c.signalConnectionClosed()
			continue
		}
	}
}

// modelSyncLoop replaces startModelSync and runs continuously
func (c *Client) modelSyncLoop() {
	for range c.syncTicker.C {
		if c.closing {
			return
		}

		if c.isReconnecting() || c.conn == nil {
			time.Sleep(1 * time.Second)
			continue
		}

		newModels, _ := c.fetchModels(true)
		if !c.modelsEqual(newModels) {
			log.Printf("Detected model changes, triggering update")
			c.models = newModels
			c.notifyModelUpdate(newModels)
		}
	}
}

func (c *Client) handleRequests() {
	defer c.signalConnectionClosed()

	for {
		var req types.ForwardRequest
		if err := c.conn.ReadJSON(&req); err != nil {
			if c.closing {
				return
			}

			// Add version error handling
			if closeErr, ok := err.(*websocket.CloseError); ok {
				switch closeErr.Code {
				case 4000:
					log.Fatalf("Version error: %s", closeErr.Text)
				case 4001:
					log.Fatalf("Version mismatch: %s", closeErr.Text)
				}
			}

			log.Printf("Connection error: %v, attempting to reconnect...", err)
			return
		}

		// Handle different types of requests
		switch req.Type {
		case types.TypePong:
		case types.TypeNormal:
			go c.forwardRequest(req)
		case types.TypeClientClose: // Add handling of close requests
			c.mu.Lock()
			cancel, exists := c.cancelMap[req.RequestID]
			c.mu.Unlock()
			if exists {
				cancel() // Cancel the corresponding request
			}
		default:
			log.Printf("Unknown request type: %d", req.Type)
		}
	}
}

// signalConnectionClosed notifies that the connection has been closed
func (c *Client) signalConnectionClosed() {
	select {
	case <-c.connClosed:
		// Channel already closed
	default:
		close(c.connClosed)
		go c.reconnect() // Trigger reconnection
	}
}

func (c *Client) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("connection is nil")
	}

	return c.conn.WriteJSON(v)
}

func (c *Client) forwardRequest(req types.ForwardRequest) {
	c.trackRequest(true)
	defer c.trackRequest(false)

	// Create a cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancelMap[req.RequestID] = cancel
	c.mu.Unlock()

	// Clean up on function return
	defer func() {
		c.mu.Lock()
		delete(c.cancelMap, req.RequestID)
		c.mu.Unlock()
	}()

	var upstreamURL string
	if req.Query == "" {
		upstreamURL = fmt.Sprintf("%s%s", c.BaseUrl, req.Path)
	} else {
		upstreamURL = fmt.Sprintf("%s%s?%s", c.BaseUrl, req.Path, req.Query)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, upstreamURL, bytes.NewReader(req.Body))
	if err != nil {
		log.Printf("Failed to create upstream request: %v", err)
		errResp := types.ForwardResponse{
			RequestID:  req.RequestID,
			StatusCode: http.StatusInternalServerError,
			Body:       []byte(fmt.Sprintf("Error: %v", err)),
			Type:       types.TypeNormal,
		}
		if err := c.writeJSON(errResp); err != nil {
			log.Printf("Failed to send error response: %v", err)
		}
		return
	}

	// Set request headers
	for k, v := range req.Header {
		httpReq.Header[k] = v
	}

	if c.ApiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.ApiKey)
	}

	// Ensure Content-Type is set
	if httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		log.Printf("Upstream request execution failed: %v", err)
		errResp := types.ForwardResponse{
			RequestID:  req.RequestID,
			StatusCode: http.StatusInternalServerError,
			Body:       []byte(fmt.Sprintf("Error: %v", err)),
			Type:       types.TypeNormal,
		}
		if err := c.writeJSON(errResp); err != nil {
			log.Printf("Failed to send error response: %v", err)
		}
		return
	}
	defer resp.Body.Close()

	if mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil && mediaType == "text/event-stream" {
		c.handleStreamResponse(resp.Body, req.RequestID, ctx)
	} else {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to read response body: %v", err)
			errResp := types.ForwardResponse{
				RequestID:  req.RequestID,
				StatusCode: http.StatusInternalServerError,
				Body:       []byte(fmt.Sprintf("Error: %v", err)),
				Type:       types.TypeNormal,
			}
			if err := c.writeJSON(errResp); err != nil {
				log.Printf("Failed to send error response: %v", err)
			}
			return
		}

		// Create forward response structure
		forwardResp := types.ForwardResponse{
			RequestID:  req.RequestID,
			StatusCode: resp.StatusCode,
			Header:     resp.Header,
			Body:       body,
			Type:       types.TypeNormal,
		}

		if err := c.writeJSON(forwardResp); err != nil {
			log.Printf("Failed to send response: %v", err)
			return
		}
	}
}

func (c *Client) handleStreamResponse(reader io.Reader, requestID string, ctx context.Context) {
	scanner := bufio.NewScanner(reader)
	var buffer bytes.Buffer

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			log.Printf("Request %s has been cancelled", requestID)
			return
		default:
			line := scanner.Bytes()
			if bytes.HasPrefix(line, []byte("data: ")) {
				content := bytes.TrimSpace(line[6:])
				if bytes.Equal(content, []byte("[DONE]")) {
					// Send stream end marker
					doneResp := types.ForwardResponse{
						RequestID:  requestID,
						Type:       types.TypeStream,
						Done:       true,
						StatusCode: http.StatusOK,
					}
					if err := c.writeJSON(doneResp); err != nil {
						log.Printf("Failed to send stream end marker: %v", err)
					}
					return
				}

				buffer.Write(content)
			}
			// Send a chunk of data when an empty line is encountered
			if len(line) == 0 {
				if buffer.Len() > 0 {
					chunk := types.ForwardResponse{
						RequestID:  requestID,
						Type:       types.TypeStream,
						StatusCode: http.StatusOK,
						Body:       buffer.Bytes(),
					}
					if err := c.writeJSON(chunk); err != nil {
						log.Printf("Failed to send stream data chunk: %v", err)
						return
					}
					buffer.Reset()
				}
			}
		}
	}
}

func generateClientID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		log.Printf("Failed to generate client ID: %v", err)
		return "fallback-id"
	}
	return hex.EncodeToString(b)
}

func (c *Client) reconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closing {
		return
	}

	c.setReconnecting(true)
	defer func() {
		c.setReconnecting(false)
	}()

	// Close the current connection if it exists
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}

	retryWait := 1 * time.Second
	maxRetryWait := 30 * time.Second

	for {
		log.Printf("Attempting to reconnect...")
		if err := c.Connect(); err == nil {
			log.Printf("Reconnected successfully")
			return
		}

		if retryWait < maxRetryWait {
			retryWait *= 2
		}
		log.Printf("Reconnection failed, retrying in %v...", retryWait)
		time.Sleep(retryWait)
	}
}

func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closing = true
	if c.conn != nil {
		c.conn.Close()
	}
	c.heartbeatTicker.Stop()
	c.syncTicker.Stop()
}

func (c *Client) modelsEqual(newModels []types.ModelInfo) bool {
	if len(c.models) != len(newModels) {
		return false
	}

	oldMap := make(map[string]struct{})
	for _, m := range c.models {
		oldMap[m.ID] = struct{}{}
	}

	for _, m := range newModels {
		if _, ok := oldMap[m.ID]; !ok {
			return false
		}
		delete(oldMap, m.ID)
	}
	return len(oldMap) == 0
}

func (c *Client) notifyModelUpdate(models []types.ModelInfo) {
	body, err := json.Marshal(types.ModelUpdateRequest{
		ClientID: c.ClientID,
		Models:   models,
	})
	if err != nil {
		log.Printf("Failed to serialize model update request: %v", err)
		return
	}

	updateReq := types.ForwardRequest{
		Type: types.TypeModelUpdate,
		Body: body,
	}

	if err := c.writeJSON(updateReq); err != nil {
		log.Printf("Failed to send model update notification: %v", err)
	}
}

func (c *Client) Shutdown() {
	c.shutdownOnce.Do(func() {
		close(c.shutdownSignal)
	})
}

func (c *Client) isReconnecting() bool {
	c.reconnMu.Lock()
	defer c.reconnMu.Unlock()
	return c.reconnecting
}

func (c *Client) setReconnecting(reconnecting bool) {
	c.reconnMu.Lock()
	defer c.reconnMu.Unlock()
	c.reconnecting = reconnecting
}

func (c *Client) WaitForShutdown() {
	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sigChan:
		if c.isReconnecting() || c.conn == nil {
			break
		}
		log.Println("Notifying server to update model list")
		c.notifyModelUpdate([]types.ModelInfo{})
	case <-c.shutdownSignal:
		return
	}

	// Add force shutdown handling
	select {
	case <-sigChan:
		if c.isReconnecting() || c.conn == nil {
			break
		}
		log.Println("Force shutdown, notifying server to interrupt requests")
		// Send force shutdown notification
		body, _ := json.Marshal(types.ForceShutdownRequest{ClientID: c.ClientID})
		forceReq := types.ForwardRequest{
			Type: types.TypeForceShutdown,
			Body: body,
		}
		if err := c.writeJSON(forceReq); err != nil {
			log.Printf("Failed to send force shutdown notification: %v", err)
		}

		log.Println("Forcing shutdown...")
		os.Exit(1)
	case <-c.waitForRequests():
		log.Println("All requests processed, exiting safely")
	}
	os.Exit(0)
}

func (c *Client) trackRequest(start bool) {
	if start {
		atomic.AddInt64(&c.activeRequests, 1)
	} else {
		atomic.AddInt64(&c.activeRequests, -1)
	}
}

func (c *Client) waitForRequests() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for atomic.LoadInt64(&c.activeRequests) > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		close(ch)
	}()
	return ch
}
