package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/zeyugao/synapse/internal/client"
)

var version = "dev" // Default value, will be overwritten at compile time

func main() {
	upstream := flag.String("upstream", "http://localhost:8081", "Upstream host")
	serverURL := flag.String("server-url", "ws://localhost:8080/ws", "WebSocket server URL")
	wsAuthKey := flag.String("ws-auth-key", "", "WebSocket authentication key")
	upstreamAPIKey := flag.String("upstream-api-key", "", "Upstream service API key")
	printVersion := flag.Bool("version", false, "Print version number")
	flag.Parse()

	if *printVersion {
		fmt.Printf("Synapse Client Version: %s\n", version)
		return
	}

	client := client.NewClient(*upstream, *serverURL, version)
	client.WSAuthKey = *wsAuthKey
	client.UpstreamAPIKey = *upstreamAPIKey
	defer client.Close()

	log.Printf("Connecting to server at %s", *serverURL)

	// Keep the program running
	for {
		if err := client.Connect(); err != nil {
			log.Printf("Connection failed: %v, retrying in 5 seconds...", err)
			time.Sleep(5 * time.Second)
			continue
		}

		select {} // Block the main thread until the connection is broken
	}
}
