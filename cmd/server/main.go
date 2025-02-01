package main

import (
	"flag"
	"log"

	"github.com/zeyugao/synapse/internal/server"
)

func main() {
	host := flag.String("host", "localhost", "Server host")
	port := flag.String("port", "8080", "Server port")
	apiAuthKey := flag.String("api-auth-key", "", "API鉴权密钥")
	wsAuthKey := flag.String("ws-auth-key", "", "WebSocket注册鉴权密钥")
	flag.Parse()

	server := server.NewServer(*apiAuthKey, *wsAuthKey)
	log.Printf("Starting server on %s:%s", *host, *port)
	if err := server.Start(*host, *port); err != nil {
		log.Fatal(err)
	}
}
