package main

import (
	"ChromehoundsStatusServer/shop"
	"ChromehoundsStatusServer/status"
	"encoding/binary"
	"log"
	"net"
	"os"
	"time"
)

// Define loggers for each level
var (
	Info  = log.New(os.Stdout, "INFO: ", log.LstdFlags|log.Lshortfile)
	Warn  = log.New(os.Stdout, "WARN: ", log.LstdFlags|log.Lshortfile)
	Error = log.New(os.Stderr, "ERROR: ", log.LstdFlags|log.Lshortfile)
)

func main() {
	log.Println("App started")
	var cfg = LoadConfig()
	log.Println("Config Loaded")
	var address = net.ParseIP(cfg.ListeningAddress)

	go RunEchoingServer(address, cfg.WorldPort, "WORLD", cfg.BufferSize)
	go RunEchoingServer(address, cfg.WorldOldPort, "WORLD_OLD", cfg.BufferSize)
	go RunShopServer(address, cfg.ShopPort, cfg.ShopBufferSize)
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
		Error.Printf("[%s] Failed to bind: %v\n", label, err)
		return
	}
	defer conn.Close()

	Info.Printf("[%s] UDP Echo Server is listening on port %d\n", label, listenPort)
	buffer := make([]byte, bufferSize)
	for {
		n, clientAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			Warn.Printf("[%s] Read error: %v\n", label, err)
			continue
		}
		Info.Printf("[%s] Received: %s\n", label, string(buffer[:n]))

		_, err = conn.WriteToUDP(buffer[:n], clientAddr)
		if err != nil {
			Warn.Printf("[%s] Write error: %v\n", label, err)
		}
	}
}

func RunShopServer(listenAddress net.IP, listenPort int, bufferSize int) {
	addr := net.UDPAddr{
		Port: listenPort,
		IP:   listenAddress,
	}

	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		Error.Printf("[SHOP] Failed to bind: %v\n", err)
		return
	}
	defer conn.Close()

	Info.Printf("[SHOP] UDP Echo Server is listening on port %d\n", listenPort)

	buffer := make([]byte, bufferSize)
	for {
		n, clientAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			Warn.Printf("[SHOP] Read error: %v\n", err)
			continue
		}
		Info.Printf("[SHOP] Received from %s:%d -> %s\n",
			clientAddr.IP, clientAddr.Port, string(buffer[:n]))

		responseStruct := shop.CreateShop()

		sendBuffer := make([]byte, 5031)
		if _, err := binary.Encode(sendBuffer, binary.LittleEndian, responseStruct); err != nil {
			panic(err)
		}

		bytesSent, err := conn.WriteToUDP(sendBuffer, clientAddr)
		if err != nil {
			Warn.Printf("[SHOP] sendto failed: %v\n", err)
			continue
		}

		Info.Printf("[SHOP] Echoed %d bytes to %s:%d\n",
			bytesSent, clientAddr.IP, clientAddr.Port)
	}
}

func RunStatusServer(listenAddress net.IP, listenPort int, bufferSize int) {
	addr := net.UDPAddr{
		Port: listenPort,
		IP:   listenAddress,
	}

	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		Error.Printf("[OTHER] Failed to bind: %v\n", err)
		return
	}
	defer conn.Close()

	Info.Printf("[OTHER] UDP Echo Server is listening on port %d\n", listenPort)

	readBuffer := make([]byte, bufferSize)
	for {
		n, clientAddr, err := conn.ReadFromUDP(readBuffer)
		if err != nil {
			Warn.Printf("[OTHER] Read error: %v\n", err)
			continue
		}
		Info.Printf("[OTHER] Received from %s:%d -> %s\n",
			clientAddr.IP, clientAddr.Port, string(readBuffer[:n]))

		currentTime := time.Now()
		offset := time.Hour * 24
		var helloBuffer []byte = readBuffer[0:31]
		var helloStruct status.UserHelloMessage

		if _, err := binary.Decode(helloBuffer, binary.LittleEndian, &helloStruct); err != nil {
			Warn.Printf("[OTHER] fallback to default xuid due to parsing error of hello header: %v\n", err)
			helloStruct.Xuid = status.XuidValueHardCoded
		}

		responseStruct := status.CreateStatus(helloStruct.Xuid, currentTime, currentTime.Add(-offset), currentTime.Add(offset))

		sendBuffer := make([]byte, 64)
		if _, err := binary.Encode(sendBuffer, binary.LittleEndian, responseStruct); err != nil {
			continue
		}

		bytesSent, err := conn.WriteToUDP(sendBuffer, clientAddr)
		if err != nil {
			Warn.Printf("[OTHER] sendto failed: %v\n", err)
			continue
		}

		Info.Printf("[OTHER] Echoed %d bytes to %s:%d\n",
			bytesSent, clientAddr.IP, clientAddr.Port)
	}
}
