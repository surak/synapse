package main

import (
	"flag"
	"log"
	"time"

	"github.com/zeyugao/synapse/internal/client"
)

func main() {
	upstream := flag.String("upstream", "http://localhost:8081", "Upstream host")
	serverHost := flag.String("server-host", "localhost", "Server host")
	serverPort := flag.String("server-port", "8080", "Server port")
	wsAuthKey := flag.String("ws-auth-key", "", "WebSocket鉴权密钥")
	flag.Parse()

	client := client.NewClient(*upstream, *serverHost, *serverPort)
	client.WSAuthKey = *wsAuthKey
	defer client.Close()

	log.Printf("Connecting to server at %s:%s", *serverHost, *serverPort)

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
