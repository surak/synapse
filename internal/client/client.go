package client

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/zeyugao/synapse/internal/types"
)

type Client struct {
	UpstreamHost string
	UpstreamPort string
	ServerHost   string
	ServerPort   string
	ClientID     string
	models       []types.ModelInfo
	conn         *websocket.Conn
}

func NewClient(upstreamHost, upstreamPort, serverHost, serverPort string) *Client {
	return &Client{
		UpstreamHost: upstreamHost,
		UpstreamPort: upstreamPort,
		ServerHost:   serverHost,
		ServerPort:   serverPort,
		ClientID:     generateClientID(), // 实现一个生成唯一ID的函数
	}
}

func (c *Client) fetchModels() error {
	resp, err := http.Get(fmt.Sprintf("http://%s:%s/v1/models", c.UpstreamHost, c.UpstreamPort))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var modelsResp types.ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		return err
	}

	c.models = modelsResp.Data
	return nil
}

func (c *Client) Connect() error {
	// 获取模型列表
	if err := c.fetchModels(); err != nil {
		return err
	}

	// 连接到服务器
	wsURL := fmt.Sprintf("ws://%s:%s/ws", c.ServerHost, c.ServerPort)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}
	c.conn = conn

	// 发送注册信息
	registration := types.ClientRegistration{
		ClientID: c.ClientID,
		Models:   c.models,
	}
	if err := conn.WriteJSON(registration); err != nil {
		conn.Close()
		return err
	}

	// 处理请求
	go c.handleRequests()

	return nil
}

func (c *Client) handleRequests() {
	for {
		var req types.ForwardRequest
		if err := c.conn.ReadJSON(&req); err != nil {
			return
		}

		// 转发请求到上游服务器
		go c.forwardRequest(req)
	}
}

func (c *Client) forwardRequest(req types.ForwardRequest) {
	upstreamURL := fmt.Sprintf("http://%s:%s%s", c.UpstreamHost, c.UpstreamPort, req.Path)

	// 创建请求
	httpReq, err := http.NewRequest("POST", upstreamURL, bytes.NewReader(req.Body))
	if err != nil {
		c.conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Error: %v", err)))
		return
	}

	// 发送请求
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		c.conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Error: %v", err)))
		return
	}
	defer resp.Body.Close()

	// 检查是否是流式响应
	if resp.Header.Get("Content-Type") == "text/event-stream" {
		c.handleStreamResponse(resp.Body)
	} else {
		// 非流式响应
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			c.conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Error: %v", err)))
			return
		}
		c.conn.WriteMessage(websocket.TextMessage, body)
	}
}

func (c *Client) handleStreamResponse(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		// 发送每一行数据
		c.conn.WriteMessage(websocket.TextMessage, []byte(line))
	}
}

func generateClientID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
