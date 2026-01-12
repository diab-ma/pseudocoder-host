// Command wsclient is a simple WebSocket test client for pseudocoder.
// Usage: go run ./cmd/wsclient ws://127.0.0.1:7070/ws
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	url := "ws://127.0.0.1:7070/ws"
	if len(os.Args) > 1 {
		url = os.Args[1]
	}

	fmt.Printf("Connecting to %s...\n", url)

	// Connect to WebSocket
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	fmt.Println("Connected! Waiting for messages...")

	// Handle interrupt signal
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	// Read messages
	done := make(chan struct{})
	messageCount := 0

	go func() {
		defer close(done)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					fmt.Printf("Read error: %v\n", err)
				}
				return
			}

			messageCount++

			// Parse and display the message
			var msg map[string]interface{}
			if err := json.Unmarshal(data, &msg); err != nil {
				fmt.Printf("[%d] Raw: %s\n", messageCount, string(data))
				continue
			}

			msgType, _ := msg["type"].(string)
			fmt.Printf("[%d] type=%s", messageCount, msgType)

			if payload, ok := msg["payload"].(map[string]interface{}); ok {
				if msgType == "terminal.append" {
					if chunk, ok := payload["chunk"].(string); ok {
						fmt.Printf(" chunk=%q", chunk)
					}
				} else if msgType == "session.status" {
					if status, ok := payload["status"].(string); ok {
						fmt.Printf(" status=%s", status)
					}
				}
			}
			fmt.Println()
		}
	}()

	// Wait for interrupt or connection close
	select {
	case <-done:
		fmt.Println("Connection closed")
	case <-interrupt:
		fmt.Println("Interrupted")
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}

	fmt.Printf("Total messages received: %d\n", messageCount)
}
