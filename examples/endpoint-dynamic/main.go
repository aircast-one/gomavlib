// This example demonstrates dynamic endpoint management in gomavlib.
// You can add and remove endpoints at runtime without restarting the node.

package main

import (
	"fmt"
	"log"
	"time"

	"github.com/bluenviron/gomavlib/v3"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/common"
)

func main() {
	// Create a node with an initial endpoint
	node := &gomavlib.Node{
		Endpoints: []gomavlib.EndpointConf{
			gomavlib.EndpointUDPServer{Address: "127.0.0.1:5600"},
		},
		Dialect:     common.Dialect,
		OutVersion:  gomavlib.V2,
		OutSystemID: 10,
	}

	err := node.Initialize()
	if err != nil {
		log.Fatal(err)
	}
	defer node.Close()

	log.Printf("Node initialized with %d endpoint(s)", len(node.GetEndpoints()))

	// Start handling events in a goroutine
	go func() {
		for evt := range node.Events() {
			switch e := evt.(type) {
			case *gomavlib.EventChannelOpen:
				log.Printf("Channel opened: %s", e.Channel)

			case *gomavlib.EventChannelClose:
				log.Printf("Channel closed: %s", e.Channel)

			case *gomavlib.EventFrame:
				log.Printf("Received frame from %s: %T", e.Channel, e.Message)
			}
		}
	}()

	// After 2 seconds, add a new WebSocket endpoint
	time.Sleep(2 * time.Second)
	log.Println("Adding WebSocket endpoint...")
	err = node.AddEndpoint(gomavlib.EndpointWebSocket{
		URL:   "ws://localhost:8080/mavlink",
		Label: "dynamic-websocket",
	})
	if err != nil {
		log.Printf("Failed to add WebSocket endpoint: %v", err)
	} else {
		log.Printf("WebSocket endpoint added. Total endpoints: %d", len(node.GetEndpoints()))
	}

	// After another 3 seconds, add a TCP client endpoint
	time.Sleep(3 * time.Second)
	log.Println("Adding TCP client endpoint...")
	err = node.AddEndpoint(gomavlib.EndpointTCPClient{Address: "127.0.0.1:5601"})
	if err != nil {
		log.Printf("Failed to add TCP endpoint: %v", err)
	} else {
		log.Printf("TCP endpoint added. Total endpoints: %d", len(node.GetEndpoints()))
	}

	// List all current endpoints
	time.Sleep(1 * time.Second)
	endpoints := node.GetEndpoints()
	log.Println("Current endpoints:")
	for i, ep := range endpoints {
		log.Printf("  %d. %T", i+1, ep)
	}

	// Remove the first endpoint (UDP server)
	time.Sleep(2 * time.Second)
	if len(endpoints) > 0 {
		log.Printf("Removing endpoint: %T", endpoints[0])
		err = node.RemoveEndpoint(endpoints[0])
		if err != nil {
			log.Printf("Failed to remove endpoint: %v", err)
		} else {
			log.Printf("Endpoint removed. Total endpoints: %d", len(node.GetEndpoints()))
		}
	}

	// Keep running for a bit
	time.Sleep(5 * time.Second)
	fmt.Println("Shutting down...")
}
