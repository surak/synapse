package client

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"sync"
	"time"

	"bufio"

	"github.com/gorilla/websocket"
	"github.com/zeyugao/synapse/internal/types"
)

type Client struct {
	Upstream     string
	ServerHost   string
	ServerPort   string
	ClientID     string
	models       []types.ModelInfo
	conn         *websocket.Conn
	mu           sync.Mutex // 新增互斥锁
	reconnecting bool       // 新增重连状态标识
	closing      bool       // 新增关闭状态标识
}

func NewClient(upstream, serverHost, serverPort string) *Client {
	return &Client{
		Upstream:   upstream,
		ServerHost: serverHost,
		ServerPort: serverPort,
		ClientID:   generateClientID(), // 实现一个生成唯一ID的函数
	}
}

func (c *Client) fetchModels() error {
	resp, err := http.Get(fmt.Sprintf("%s/v1/models", c.Upstream))
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
		log.Printf("初始化模型获取失败: %v", err)
		return err
	}

	wsURL := fmt.Sprintf("ws://%s:%s/ws", c.ServerHost, c.ServerPort)
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
	}
	if err := conn.WriteJSON(registration); err != nil {
		log.Printf("发送注册信息失败: %v", err)
		conn.Close()
		return err
	}
	log.Printf("已发送客户端注册信息，ID: %s", c.ClientID)

	go c.handleRequests()
	return nil
}

func (c *Client) handleRequests() {
	for {
		var req types.ForwardRequest
		if err := c.conn.ReadJSON(&req); err != nil {
			if c.closing {
				return
			}
			log.Printf("连接异常: %v，尝试重新连接...", err)
			go c.reconnect()
			return
		}
		log.Printf("收到转发请求 - 模型: %s, 路径: %s", req.Model, req.Path)
		go c.forwardRequest(req)
	}
}

func (c *Client) forwardRequest(req types.ForwardRequest) {
	var upstreamURL string
	if req.Query == "" {
		upstreamURL = fmt.Sprintf("%s%s", c.Upstream, req.Path)
	} else {
		upstreamURL = fmt.Sprintf("%s%s?%s", c.Upstream, req.Path, req.Query)
	}

	httpReq, err := http.NewRequest(req.Method, upstreamURL, bytes.NewReader(req.Body))
	if err != nil {
		log.Printf("创建上游请求失败: %v", err)
		errResp := types.ForwardResponse{
			RequestID:  req.RequestID,
			StatusCode: http.StatusInternalServerError,
			Body:       []byte(fmt.Sprintf("Error: %v", err)),
			Type:       types.TypeNormal,
		}
		c.conn.WriteJSON(errResp)
		return
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
		if err := c.conn.WriteJSON(errResp); err != nil {
			log.Printf("发送错误响应失败: %v", err)
		}
		return
	}

	if mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil && mediaType == "text/event-stream" {
		go c.handleStreamResponse(resp.Body, req.RequestID)
	} else {
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("读取响应体失败: %v", err)
			errResp := types.ForwardResponse{
				RequestID:  req.RequestID,
				StatusCode: http.StatusInternalServerError,
				Body:       []byte(fmt.Sprintf("Error: %v", err)),
				Type:       types.TypeNormal,
			}
			if err := c.conn.WriteJSON(errResp); err != nil {
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

		// 在发送响应之前添加互斥锁
		var writeMu sync.Mutex
		writeMu.Lock()
		if err := c.conn.WriteJSON(forwardResp); err != nil {
			log.Printf("发送响应失败: %v", err)
			writeMu.Unlock()
			return
		}
		writeMu.Unlock()
	}
}

func (c *Client) handleStreamResponse(reader io.ReadCloser, requestID string) {
	defer reader.Close()
	scanner := bufio.NewScanner(reader)
	var buffer bytes.Buffer

	for scanner.Scan() {
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
				if err := c.conn.WriteJSON(doneResp); err != nil {
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
				if err := c.conn.WriteJSON(chunk); err != nil {
					log.Printf("发送流数据块失败: %v", err)
					return
				}
				buffer.Reset()
			}
		}
	}
	log.Printf("流式响应处理完成")
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

	if c.reconnecting || c.closing {
		return
	}

	c.reconnecting = true
	defer func() { c.reconnecting = false }()

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

func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closing = true
	if c.conn != nil {
		c.conn.Close()
	}
}
