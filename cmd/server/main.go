package main

import (
	"flag"
	"log"
	"github.com/yourusername/yourproject/internal/server"
)

func main() {
	host := flag.String("host", "localhost", "Server host")
	port := flag.String("port", "8080", "Server port")
	flag.Parse()

	server := server.NewServer()
	log.Printf("Starting server on %s:%s", *host, *port)
	if err := server.Start(*host, *port); err != nil {
		log.Fatal(err)
	}
} 