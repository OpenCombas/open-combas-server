package main

import (
	"ChromehoundsStatusServer/status"
	"context"
	"encoding/binary"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Define loggers for each level
var (
	Info  = log.New(os.Stdout, "INFO: ", log.LstdFlags)
	Warn  = log.New(os.Stdout, "WARN: ", log.LstdFlags)
	Error = log.New(os.Stderr, "ERROR: ", log.LstdFlags)
)

var wg sync.WaitGroup

func main() {

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	defer stop()

	Info.Println("App started")
	var cfg = LoadConfig()
	Info.Println("Config Loaded")
	var address = net.ParseIP(cfg.ListeningAddress)

	go RunStatusServer(address, cfg.ServerStatusPort, cfg.BufferSize, ctx)
	for _, echoCfg := range cfg.EchoingServers {
		go RunEchoingServer(address, echoCfg.Port, echoCfg.Label, cfg.BufferSize, ctx)
	}

	// Sleep forever (or until manually stopped)
	<-ctx.Done()
	Info.Println("Shuting down")
	wg.Wait()
	Info.Println("Shut down")
}

func RunEchoingServer(listenAddress net.IP, listenPort int, label string, bufferSize int, ctx context.Context) {
	wg.Add(1)
	defer wg.Done()

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
		select {
		case <-ctx.Done():
			Info.Printf("[%s] Received shutdown signal\n", label)
			return

		default:
			conn.SetReadDeadline(time.Now().Add((1 * time.Second)))
			n, clientAddr, err := conn.ReadFromUDP(buffer)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
				} else {
					Warn.Printf("[%s] Read error: %v\n", label, err)
				}
				continue
			}
			Info.Printf("[%s] Received: %s\n", label, string(buffer[:n]))

			_, err = conn.WriteToUDP(buffer[:n], clientAddr)
			if err != nil {
				Warn.Printf("[%s] Write error: %v\n", label, err)
			}
		}
	}
}

func RunStatusServer(listenAddress net.IP, listenPort int, bufferSize int, ctx context.Context) {
	wg.Add(1)
	defer wg.Done()
	addr := net.UDPAddr{
		Port: listenPort,
		IP:   listenAddress,
	}

	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		Error.Printf("[STATUS] Failed to bind: %v\n", err)
		return
	}
	defer conn.Close()

	Info.Printf("[STATUS] UDP Status Server is listening on port %d\n", listenPort)

	readBuffer := make([]byte, bufferSize)
	for {
		select {
		case <-ctx.Done():
			Info.Printf("[%s] Received shutdown signal\n", "STATUS")
			return

		default:
			conn.SetReadDeadline(time.Now().Add((1 * time.Second)))
			n, clientAddr, err := conn.ReadFromUDP(readBuffer)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
				} else {
					Warn.Printf("[STATUS] Read error: %v\n", err)
				}
				continue
			}
			Info.Printf("[STATUS] Received from %s:%d -> %s\n",
				clientAddr.IP, clientAddr.Port, string(readBuffer[:n]))

			currentTime := time.Now()
			offset := time.Minute * 10
			var helloBuffer []byte = readBuffer[0:31]
			var helloStruct status.UserHelloMessage

			if _, err := binary.Decode(helloBuffer, binary.LittleEndian, &helloStruct); err != nil {
				Warn.Printf("[STATUS] fallback to default xuid due to parsing error of hello header: %v\n", err)
				helloStruct.Xuid = status.XuidValueHardCoded
			}

			responseStruct := status.CreateStatus(helloStruct.Xuid, currentTime, currentTime.Add(-offset), currentTime.Add(offset))

			sendBuffer := make([]byte, 64)
			if _, err := binary.Encode(sendBuffer, binary.LittleEndian, responseStruct); err != nil {
				continue
			}

			bytesSent, err := conn.WriteToUDP(sendBuffer, clientAddr)
			if err != nil {
				Warn.Printf("[STATUS] sendto failed: %v\n", err)
				continue
			}

			Info.Printf("[STATUS] Echoed %d bytes to %s:%d\n",
				bytesSent, clientAddr.IP, clientAddr.Port)
		}
	}
}
