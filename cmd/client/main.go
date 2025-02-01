package main

import (
	"flag"
	"log"

	"github.com/zeyugao/synapse/internal/client"
)

func main() {
	upstreamHost := flag.String("upstream-host", "localhost", "Upstream host")
	upstreamPort := flag.String("upstream-port", "8081", "Upstream port")
	serverHost := flag.String("server-host", "localhost", "Server host")
	serverPort := flag.String("server-port", "8080", "Server port")
	flag.Parse()

	client := client.NewClient(*upstreamHost, *upstreamPort, *serverHost, *serverPort)
	log.Printf("Connecting to server at %s:%s", *serverHost, *serverPort)
	if err := client.Connect(); err != nil {
		log.Fatal(err)
	}

	// 保持程序运行
	select {}
}
