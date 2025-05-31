package server

import (
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/status"
	"context"
	"encoding/binary"
	"net"
	"sync"
	"time"
)

func RunStatusServer(listenAddress net.IP, listenPort int, label string, bufferSize int, ctx *context.Context, wg *sync.WaitGroup) {
	(*wg).Add(1)
	defer (*wg).Done()

	conn, err := buildUDPListener(listenAddress, listenPort, label)
	if err != nil {
		return
	}
	defer conn.Close()

	readBuffer := make([]byte, bufferSize)
	for {
		select {
		case <-(*ctx).Done():
			logging.Info.Printf("[%s] Received shutdown signal\n", label)
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
		logging.Warn.Printf("[%s] fallback to default xuid due to parsing error of hello header: %v\n", label, err)
		helloStruct.Xuid = status.XuidValueHardCoded
	}

	responseStruct := status.CreateStatus(helloStruct.Xuid, currentTime, currentTime.Add(-offset), currentTime.Add(offset))
	sendBuffer := make([]byte, 64)
	if _, err := binary.Encode(sendBuffer, binary.LittleEndian, responseStruct); err != nil {
		logging.Warn.Printf("[%s] Error populating sendbuffer: %s", label, err)
		return nil, err
	}
	return &sendBuffer, nil
}
