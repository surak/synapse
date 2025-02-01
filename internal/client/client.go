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
	log.Printf("开始处理请求 - 模型: %s, 路径: %s", req.Model, req.Path)
	defer log.Printf("完成处理请求 - 模型: %s, 路径: %s", req.Model, req.Path)

	var upstreamURL string
	if req.Query == "" {
		upstreamURL = fmt.Sprintf("%s%s", c.Upstream, req.Path)
	} else {
		upstreamURL = fmt.Sprintf("%s%s?%s", c.Upstream, req.Path, req.Query)
	}
	log.Printf("转发请求到上游: %s %s", req.Method, upstreamURL)
	log.Printf("请求内容: %s", string(req.Body))

	httpReq, err := http.NewRequest(req.Method, upstreamURL, bytes.NewReader(req.Body))
	if err != nil {
		log.Printf("创建上游请求失败: %v", err)
		errResp := types.ForwardResponse{
			RequestID:  req.RequestID,
			StatusCode: http.StatusInternalServerError,
			Body:       []byte(fmt.Sprintf("Error: %v", err)),
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
		}
		if err := c.conn.WriteJSON(errResp); err != nil {
			log.Printf("发送错误响应失败: %v", err)
		}
		return
	}
	defer resp.Body.Close()
	log.Printf("上游响应状态码: %d", resp.StatusCode)
	log.Printf("上游响应头: %+v", resp.Header)

	if mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil && mediaType == "text/event-stream" {
		log.Printf("处理流式响应")
		c.handleStreamResponse(resp.Body)
	} else {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("读取响应体失败: %v", err)
			errResp := types.ForwardResponse{
				RequestID:  req.RequestID,
				StatusCode: http.StatusInternalServerError,
				Body:       []byte(fmt.Sprintf("Error: %v", err)),
			}
			if err := c.conn.WriteJSON(errResp); err != nil {
				log.Printf("发送错误响应失败: %v", err)
			}
			return
		}
		log.Printf("收到非流式响应，长度: %d bytes", len(body))
		log.Printf("响应内容: %s", string(body))

		// 创建转发响应结构
		forwardResp := types.ForwardResponse{
			RequestID:  req.RequestID,
			StatusCode: resp.StatusCode,
			Header:     resp.Header,
			Body:       body,
		}

		// 在发送响应之前添加互斥锁
		var writeMu sync.Mutex
		writeMu.Lock()
		if err := c.conn.WriteJSON(forwardResp); err != nil {
			log.Printf("发送响应失败: %v", err)
			writeMu.Unlock()
			return
		}
		log.Printf("响应已发送")
		writeMu.Unlock()
	}
}

func (c *Client) handleStreamResponse(reader io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Printf("读取流数据错误: %v", err)
			}
			return
		}
		if n > 0 {
			if err := c.conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				log.Printf("发送流数据失败: %v", err)
				return
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
