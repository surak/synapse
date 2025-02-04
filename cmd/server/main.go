package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/zeyugao/synapse/internal/server"
)

var version = "dev" // 默认值，编译时会被覆盖

func main() {
	host := flag.String("host", "localhost", "Server host")
	port := flag.String("port", "8080", "Server port")
	apiAuthKey := flag.String("api-auth-key", "", "API鉴权密钥")
	wsAuthKey := flag.String("ws-auth-key", "", "WebSocket注册鉴权密钥")
	printVersion := flag.Bool("version", false, "打印版本号")
	flag.Parse()

	if *printVersion {
		fmt.Printf("Synapse Server Version: %s\n", version)
		return
	}

	server := server.NewServer(*apiAuthKey, *wsAuthKey, version)
	log.Printf("Starting server on %s:%s", *host, *port)
	if err := server.Start(*host, *port); err != nil {
		log.Fatal(err)
	}
}
