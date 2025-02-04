package main

import (
	"flag"
	"log"
	"time"

	"github.com/zeyugao/synapse/internal/client"
)

func main() {
	upstream := flag.String("upstream", "http://localhost:8081", "Upstream host")
	serverURL := flag.String("server-url", "ws://localhost:8080/ws", "WebSocket服务器URL")
	wsAuthKey := flag.String("ws-auth-key", "", "WebSocket鉴权密钥")
	upstreamAPIKey := flag.String("upstream-api-key", "", "Upstream服务的API密钥")
	flag.Parse()

	client := client.NewClient(*upstream, *serverURL)
	client.WSAuthKey = *wsAuthKey
	client.UpstreamAPIKey = *upstreamAPIKey
	defer client.Close()

	log.Printf("Connecting to server at %s", *serverURL)

	// 保持程序运行
	for {
		if err := client.Connect(); err != nil {
			log.Printf("连接失败: %v，5秒后重试...", err)
			time.Sleep(5 * time.Second)
			continue
		}

		select {} // 阻塞主线程直到连接断开
	}
}
