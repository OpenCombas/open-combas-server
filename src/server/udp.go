package server

import (
	"ChromehoundsStatusServer/logging"
	"fmt"
	"net"
	"time"
)

func buildUDPListener(listenAddress net.IP, listenPort int, label string) (*net.UDPConn, error) {
	addr := net.UDPAddr{
		Port: listenPort,
		IP:   listenAddress,
	}

	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		logging.Error.Printf("[%s] Failed to bind: %v\n", label, err)
		return nil, nil
	}
	logging.Info.Printf("[%s] UDP Server is listening on port %d\n", label, listenPort)
	return conn, nil
}

func readUDP(conn *net.UDPConn, buffer *[]byte, label string) (int, *net.UDPAddr, error) {
	conn.SetReadDeadline(time.Now().Add((1 * time.Second)))
	n, clientAddr, err := conn.ReadFromUDP(*buffer)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
		} else {
			logging.Warn.Printf("[%s] Read error: %v\n", label, err)
		}
		return 0, nil, err
	} else if n == 0 {
		return 0, nil, fmt.Errorf("0 bytes recieved, but still recieved")
	}
	logging.Info.Printf("[%s] Received from %s:%d -> %s\n",
		label, clientAddr.IP, clientAddr.Port, string((*buffer)[:n]))
	return n, clientAddr, nil
}

func sendUDP(conn *net.UDPConn, clientAddr *net.UDPAddr, buffer *[]byte, label string, logSend bool) error {
	bytesSent, err := conn.WriteToUDP(*buffer, clientAddr)
	if err != nil {
		logging.Warn.Printf("[%s] send failed: %v\n", label, err)
		return err
	}

	if logSend {
		logging.Info.Printf("[%s] Sent %d bytes to %s:%d\n",
			label, bytesSent, clientAddr.IP, clientAddr.Port)
	}
	return nil
}
