package main

import (
	"ChromehoundsStatusServer/status"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var wg sync.WaitGroup

func main() {

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	defer stop()

	Info.Println("App started")
	var cfg = LoadConfig()
	Info.Println("Config Loaded")
	var address = net.ParseIP(cfg.ListeningAddress)

	go RunStatusServer(address, cfg.ServerStatusPort, "STATUS", cfg.BufferSize, ctx)
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

	conn, err := buildUDPListener(listenAddress, listenPort, label)
	if err != nil {
		return
	}
	defer conn.Close()

	buffer := make([]byte, bufferSize)
	for {
		select {
		case <-ctx.Done():
			Info.Printf("[%s] Received shutdown signal\n", label)
			return

		default:
			n, clientAddr, err := readUDP(conn, &buffer, label)
			if err != nil {
				continue
			}
			var sendBuffer = buffer[:n]
			sendUDP(conn, clientAddr, &sendBuffer, label, false)

		}
	}
}

func RunStatusServer(listenAddress net.IP, listenPort int, label string, bufferSize int, ctx context.Context) {
	wg.Add(1)
	defer wg.Done()

	conn, err := buildUDPListener(listenAddress, listenPort, label)
	if err != nil {
		return
	}
	defer conn.Close()

	readBuffer := make([]byte, bufferSize)
	for {
		select {
		case <-ctx.Done():
			Info.Printf("[%s] Received shutdown signal\n", label)
			return

		default:
			_, clientAddr, err := readUDP(conn, &readBuffer, label)
			if err != nil {
				continue
			}

			sendBuffer, err := createStatusResponse(&readBuffer, label)
			if err != nil {
				continue
			}

			sendUDP(conn, clientAddr, sendBuffer, label, true)
		}
	}
}

func createStatusResponse(readBuffer *[]byte, label string) (*[]byte, error) {
	currentTime := time.Now()
	offset := time.Minute * 10
	var helloBuffer []byte = (*readBuffer)[0:31]
	var helloStruct status.UserHelloMessage

	if _, err := binary.Decode(helloBuffer, binary.LittleEndian, &helloStruct); err != nil {
		Warn.Printf("[%s] fallback to default xuid due to parsing error of hello header: %v\n", label, err)
		helloStruct.Xuid = status.XuidValueHardCoded
	}

	responseStruct := status.CreateStatus(helloStruct.Xuid, currentTime, currentTime.Add(-offset), currentTime.Add(offset))
	sendBuffer := make([]byte, 64)
	if _, err := binary.Encode(sendBuffer, binary.LittleEndian, responseStruct); err != nil {
		Warn.Printf("[%s] Error populating sendbuffer: %s", label, err)
		return nil, err
	}
	return &sendBuffer, nil
}

func buildUDPListener(listenAddress net.IP, listenPort int, label string) (*net.UDPConn, error) {
	addr := net.UDPAddr{
		Port: listenPort,
		IP:   listenAddress,
	}

	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		Error.Printf("[%s] Failed to bind: %v\n", label, err)
		return nil, nil
	}
	Info.Printf("[%s] UDP Server is listening on port %d\n", label, listenPort)
	return conn, nil
}

func readUDP(conn *net.UDPConn, buffer *[]byte, label string) (int, *net.UDPAddr, error) {
	conn.SetReadDeadline(time.Now().Add((1 * time.Second)))
	n, clientAddr, err := conn.ReadFromUDP(*buffer)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
		} else {
			Warn.Printf("[%s] Read error: %v\n", label, err)
		}
		return 0, nil, err
	} else if n == 0 {
		return 0, nil, fmt.Errorf("0 bytes recieved, but still recieved")
	}
	Info.Printf("[%s] Received from %s:%d -> %s\n",
		label, clientAddr.IP, clientAddr.Port, string((*buffer)[:n]))
	return n, clientAddr, nil
}

func sendUDP(conn *net.UDPConn, clientAddr *net.UDPAddr, buffer *[]byte, label string, logSend bool) error {
	bytesSent, err := conn.WriteToUDP(*buffer, clientAddr)
	if err != nil {
		Warn.Printf("[%s] send failed: %v\n", label, err)
		return err
	}

	if logSend {
		Info.Printf("[%s] Sent %d bytes to %s:%d\n",
			label, bytesSent, clientAddr.IP, clientAddr.Port)
	}
	return nil
}
