package main

import (
	"flag"
	"log"

	"github.com/zeyugao/synapse/internal/client"
)

func main() {
	upstream := flag.String("upstream", "http://localhost:8081", "Upstream host")
	serverHost := flag.String("server-host", "localhost", "Server host")
	serverPort := flag.String("server-port", "8080", "Server port")
	flag.Parse()

	client := client.NewClient(*upstream, *serverHost, *serverPort)
	log.Printf("Connecting to server at %s:%s", *serverHost, *serverPort)
	if err := client.Connect(); err != nil {
		log.Fatal(err)
	}

	// 保持程序运行
	select {}
}
