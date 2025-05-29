package main

import (
	"ChromehoundsStatusServer/status"
	"bytes"
	"encoding/gob"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	var cfg = LoadConfig()
	var address = net.ParseIP(cfg.ListeningAddress)

	go RunEchoingServer(address, cfg.WorldPort, "WORLD", cfg.BufferSize)
	go RunEchoingServer(address, cfg.WorldOldPort, "WORLD_OLD", cfg.BufferSize)
	go RunStatusServer(address, cfg.ServerStatusPort, cfg.BufferSize)

	// Sleep forever (or until manually stopped)
	select {}
}

func RunEchoingServer(listenAddress net.IP, listenPort int, label string, bufferSize int) {
	addr := net.UDPAddr{
		Port: listenPort,
		IP:   listenAddress,
	}

	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] Failed to bind: %v\n", label, err)
		return
	}
	defer conn.Close()

	fmt.Printf("[%s] UDP Echo Server is listening on port %d\n", label, listenPort)
	buffer := make([]byte, bufferSize)
	for {
		n, clientAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] Read error: %v\n", label, err)
			continue
		}
		fmt.Printf("[%s] Received: %s\n", label, string(buffer[:n]))

		_, err = conn.WriteToUDP(buffer[:n], clientAddr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] Write error: %v\n", label, err)
		}
	}
}

func RunStatusServer(listenAddress net.IP, listenPort int, bufferSize int) {
	addr := net.UDPAddr{
		Port: listenPort,
		IP:   listenAddress,
	}

	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[OTHER] Failed to bind: %v\n", err)
		return
	}
	defer conn.Close()

	fmt.Printf("[OTHER] UDP Echo Server is listening on port %d\n", listenPort)

	buffer := make([]byte, bufferSize)
	for {
		n, clientAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[OTHER] Read error: %v\n", err)
			continue
		}
		fmt.Printf("[OTHER] Received from %s:%d -> %s\n",
			clientAddr.IP, clientAddr.Port, string(buffer[:n]))

		currentTime := time.Now()
		offset := time.Minute * 10
		responseStruct := status.CreateStatus(currentTime, currentTime.Add(-offset), currentTime.Add(offset))

		var sendBuffer bytes.Buffer
		enc := gob.NewEncoder(&sendBuffer)

		if err := enc.Encode(responseStruct); err != nil {
			panic(err)
		}

		bytesSent, err := conn.WriteToUDP(sendBuffer.Bytes(), clientAddr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[OTHER] sendto failed: %v\n", err)
			continue
		}

		fmt.Printf("[OTHER] Echoed %d bytes to %s:%d\n",
			bytesSent, clientAddr.IP, clientAddr.Port)
	}
}
