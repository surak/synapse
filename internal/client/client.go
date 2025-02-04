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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bufio"

	"github.com/gorilla/websocket"
	"github.com/zeyugao/synapse/internal/types"
)

type Client struct {
	Upstream        string
	ServerURL       string
	ClientID        string
	WSAuthKey       string
	UpstreamAPIKey  string
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
	activeRequests  int64 // 原子计数器
	Version         string
}

func NewClient(upstream, serverURL string, version string) *Client {
	return &Client{
		Upstream:        upstream,
		ServerURL:       serverURL,
		ClientID:        generateClientID(),
		Version:         version,
		cancelMap:       make(map[string]context.CancelFunc),
		heartbeatTicker: time.NewTicker(15 * time.Second),
		syncTicker:      time.NewTicker(30 * time.Second),
		shutdownSignal:  make(chan struct{}),
	}
}

func (c *Client) fetchModels() error {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/v1/models", c.Upstream), nil)
	if err != nil {
		log.Printf("创建模型请求失败: %v", err)
		return err
	}

	if c.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.UpstreamAPIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("获取模型列表失败: %v", err)
		return err
	}
	defer resp.Body.Close()

	var modelsResp types.ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		log.Printf("解析模型响应失败: %v", err)
		return err
	}
	log.Printf("获取到 %d 个模型", len(modelsResp.Data))
	c.models = modelsResp.Data
	return nil
}

func (c *Client) Connect() error {
	if err := c.fetchModels(); err != nil {
		return err
	}

	// 直接使用配置的服务器URL
	wsURL := c.ServerURL
	if c.WSAuthKey != "" {
		wsURL += "?ws_auth_key=" + c.WSAuthKey
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Printf("连接服务器失败: %v", err)
		return err
	}
	c.conn = conn
	log.Printf("成功连接到服务器 %s", wsURL)

	registration := types.ClientRegistration{
		ClientID: c.ClientID,
		Models:   c.models,
		Version:  c.Version,
	}
	if err := conn.WriteJSON(registration); err != nil {
		log.Printf("发送注册信息失败: %v", err)
		conn.Close()
		return err
	}
	log.Printf("已发送客户端注册信息，ID: %s", c.ClientID)

	go c.startHeartbeat()
	go c.startModelSync()
	go c.handleRequests()
	go c.WaitForShutdown()
	return nil
}

func (c *Client) handleRequests() {
	for {
		var req types.ForwardRequest
		if err := c.conn.ReadJSON(&req); err != nil {
			if c.closing {
				return
			}

			// 添加版本错误处理
			if closeErr, ok := err.(*websocket.CloseError); ok {
				switch closeErr.Code {
				case 4000:
					log.Fatalf("版本错误: %s", closeErr.Text)
				case 4001:
					log.Fatalf("版本不匹配: %s", closeErr.Text)
				}
			}

			log.Printf("连接异常: %v，尝试重新连接...", err)
			go c.reconnect()
			return
		}

		// 处理不同类型请求
		switch req.Type {
		case types.TypePong:
		case types.TypeNormal:
			go c.forwardRequest(req)
		case types.TypeClientClose: // 新增处理关闭请求
			c.mu.Lock()
			cancel, exists := c.cancelMap[req.RequestID]
			c.mu.Unlock()
			if exists {
				cancel() // 取消对应的请求
			}
		default:
			log.Printf("未知请求类型: %d", req.Type)
		}
	}
}

func (c *Client) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(v)
}

func (c *Client) forwardRequest(req types.ForwardRequest) {
	c.trackRequest(true)
	defer c.trackRequest(false)

	// 创建可取消的context
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancelMap[req.RequestID] = cancel
	c.mu.Unlock()

	// 在函数返回时清理
	defer func() {
		c.mu.Lock()
		delete(c.cancelMap, req.RequestID)
		c.mu.Unlock()
	}()

	var upstreamURL string
	if req.Query == "" {
		upstreamURL = fmt.Sprintf("%s%s", c.Upstream, req.Path)
	} else {
		upstreamURL = fmt.Sprintf("%s%s?%s", c.Upstream, req.Path, req.Query)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, upstreamURL, bytes.NewReader(req.Body))
	// httpReq, err := http.NewRequest(req.Method, upstreamURL, bytes.NewReader(req.Body))
	if err != nil {
		log.Printf("创建上游请求失败: %v", err)
		errResp := types.ForwardResponse{
			RequestID:  req.RequestID,
			StatusCode: http.StatusInternalServerError,
			Body:       []byte(fmt.Sprintf("Error: %v", err)),
			Type:       types.TypeNormal,
		}
		if err := c.writeJSON(errResp); err != nil {
			log.Printf("发送错误响应失败: %v", err)
		}
		return
	}

	if c.UpstreamAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.UpstreamAPIKey)
	}

	// 设置请求头
	for k, v := range req.Header {
		httpReq.Header[k] = v
	}
	// 确保设置了 Content-Type
	if httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		log.Printf("上游请求执行失败: %v", err)
		errResp := types.ForwardResponse{
			RequestID:  req.RequestID,
			StatusCode: http.StatusInternalServerError,
			Body:       []byte(fmt.Sprintf("Error: %v", err)),
			Type:       types.TypeNormal,
		}
		if err := c.writeJSON(errResp); err != nil {
			log.Printf("发送错误响应失败: %v", err)
		}
		return
	}
	defer resp.Body.Close()

	if mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil && mediaType == "text/event-stream" {
		c.handleStreamResponse(resp.Body, req.RequestID, ctx)
	} else {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("读取响应体失败: %v", err)
			errResp := types.ForwardResponse{
				RequestID:  req.RequestID,
				StatusCode: http.StatusInternalServerError,
				Body:       []byte(fmt.Sprintf("Error: %v", err)),
				Type:       types.TypeNormal,
			}
			if err := c.writeJSON(errResp); err != nil {
				log.Printf("发送错误响应失败: %v", err)
			}
			return
		}

		// 创建转发响应结构
		forwardResp := types.ForwardResponse{
			RequestID:  req.RequestID,
			StatusCode: resp.StatusCode,
			Header:     resp.Header,
			Body:       body,
			Type:       types.TypeNormal,
		}

		if err := c.writeJSON(forwardResp); err != nil {
			log.Printf("发送响应失败: %v", err)
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
			log.Printf("请求 %s 已被取消", requestID)
			return
		default:
			line := scanner.Bytes()
			if bytes.HasPrefix(line, []byte("data: ")) {
				content := bytes.TrimSpace(line[6:])
				if bytes.Equal(content, []byte("[DONE]")) {
					// 发送流结束标记
					doneResp := types.ForwardResponse{
						RequestID:  requestID,
						Type:       types.TypeStream,
						Done:       true,
						StatusCode: http.StatusOK,
					}
					if err := c.writeJSON(doneResp); err != nil {
						log.Printf("发送流结束标记失败: %v", err)
					}
					return
				}

				buffer.Write(content)
			}
			// 当遇到空行时发送一个数据块
			if len(line) == 0 {
				if buffer.Len() > 0 {
					chunk := types.ForwardResponse{
						RequestID:  requestID,
						Type:       types.TypeStream,
						StatusCode: http.StatusOK,
						Body:       buffer.Bytes(),
					}
					if err := c.writeJSON(chunk); err != nil {
						log.Printf("发送流数据块失败: %v", err)
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
		log.Printf("生成客户端ID失败: %v", err)
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

	retryWait := 1 * time.Second
	maxRetryWait := 30 * time.Second

	for {
		log.Printf("尝试重新连接...")
		if err := c.Connect(); err == nil {
			log.Printf("重连成功")
			return
		}

		if retryWait < maxRetryWait {
			retryWait *= 2
		}
		log.Printf("重连失败，%v 后重试...", retryWait)
		time.Sleep(retryWait)
	}
}

func (c *Client) startHeartbeat() {
	for range c.heartbeatTicker.C {
		if c.closing || c.isReconnecting() {
			continue
		}

		heartbeat := types.ForwardResponse{
			Timestamp: time.Now().Unix(),
			Type:      types.TypeHeartbeat,
		}
		if err := c.writeJSON(heartbeat); err != nil {
			log.Printf("发送心跳失败: %v", err)
			go c.reconnect() // 添加重连触发
			return
		}
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

func (c *Client) startModelSync() {
	for range c.syncTicker.C {
		if c.closing || c.isReconnecting() {
			continue
		}

		newModels, _ := c.fetchModelsSilent()
		if !c.modelsEqual(newModels) {
			log.Printf("检测到模型变化，触发更新")
			c.models = newModels
			c.notifyModelUpdate(newModels)
		}
	}
}

func (c *Client) fetchModelsSilent() ([]types.ModelInfo, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/v1/models", c.Upstream), nil)
	if err != nil {
		log.Printf("创建模型请求失败: %v", err)
		return []types.ModelInfo{}, err
	}

	if c.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.UpstreamAPIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("获取模型列表失败: %v", err)
		return []types.ModelInfo{}, err
	}
	defer resp.Body.Close()

	var modelsResp types.ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		log.Printf("解析模型响应失败: %v", err)
		return []types.ModelInfo{}, err
	}
	return modelsResp.Data, nil
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
		log.Printf("序列化模型更新请求失败: %v", err)
		return
	}

	updateReq := types.ForwardRequest{
		Type: types.TypeModelUpdate,
		Body: body,
	}

	if err := c.writeJSON(updateReq); err != nil {
		log.Printf("发送模型更新通知失败: %v", err)
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
		if c.isReconnecting() {
			break
		}
		log.Println("通知服务器更新模型列表")
		c.notifyModelUpdate([]types.ModelInfo{})
	case <-c.shutdownSignal:
		return
	}

	// 新增强制关闭处理
	select {
	case <-sigChan:
		if c.isReconnecting() {
			break
		}
		log.Println("强制关闭，通知服务器中断请求")
		// 发送强制关闭通知
		body, _ := json.Marshal(types.ForceShutdownRequest{ClientID: c.ClientID})
		forceReq := types.ForwardRequest{
			Type: types.TypeForceShutdown,
			Body: body,
		}
		if err := c.writeJSON(forceReq); err != nil {
			log.Printf("发送强制关闭通知失败: %v", err)
		}

		log.Println("强制关闭...")
		os.Exit(1)
	case <-c.waitForRequests():
		log.Println("所有请求处理完成，安全退出")
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
