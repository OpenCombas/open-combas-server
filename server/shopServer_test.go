package server

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/pooling"
	"ChromehoundsStatusServer/shop"
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// TestValidateShopPacket covers the size gates.
func TestValidateShopPacket(t *testing.T) {
	clientAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	label := "TEST"

	tests := []struct {
		name        string
		packet      []byte
		expectError bool
	}{
		{"empty", []byte{}, true},
		{"too small", make([]byte, constants.MinHelloMessageSize-1), true},
		{"minimum size", make([]byte, constants.MinHelloMessageSize), false},
		{"maximum size", make([]byte, constants.MaxBufferSize), false},
		{"too large", make([]byte, constants.MaxBufferSize+1), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateShopPacket(tt.packet, clientAddr, label)
			if (err != nil) != tt.expectError {
				t.Fatalf("ValidateShopPacket(%d bytes) error = %v, expectError = %v", len(tt.packet), err, tt.expectError)
			}
		})
	}
}

// TestDecodeFaction pins the header scan: the last A/B/C byte wins, so the
// faction byte at 0x1e is read regardless of the gamertag contents.
func TestDecodeFaction(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want string
	}{
		{"tarakia", []byte{0, 9, 0, 1, 'A'}, "A"},
		{"morskoj", []byte{0, 9, 0, 1, 'B'}, "B"},
		{"sal kar", []byte{0, 9, 0, 1, 'C'}, "C"},
		{"no faction byte", []byte{0, 9, 0, 1, 0}, "A"}, // fallback
		{"last byte wins", []byte{'A', 'B', 'C'}, "C"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeFaction(tc.b); got != tc.want {
				t.Fatalf("decodeFaction(%x) = %q, want %q", tc.b, got, tc.want)
			}
		})
	}
}

// TestRunShopServerServesBuyList boots the shop server on a loopback port,
// sends one faction-A request and asserts the reply is a valid shop packet of
// the right length that echoes the request header.
func TestRunShopServerServesBuyList(t *testing.T) {
	pooling.InitBufferPools(constants.MaxBufferSize)
	loggingInit := config.LoggingConfig{Verbose: false, PerformanceReportInterval: 10}

	serverAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	conn, err := net.ListenUDP("udp", serverAddr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()

	cfg := config.ServerConfig{Label: "SHOP_TEST", Port: port, Enabled: true, Type: config.Shop}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	promCfg := config.PrometheusConfig{Enabled: false}
	go RunShopServer(net.ParseIP("127.0.0.1"), &cfg, constants.MaxBufferSize, &loggingInit, ctx, &wg, promCfg, nil)

	// Wait for the listener to come up.
	time.Sleep(100 * time.Millisecond)

	client, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		cancel()
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	request := make([]byte, constants.MinHelloMessageSize)
	request[1] = 0x09
	copy(request[4:], "00091234567890ABCDEF")
	copy(request[12:], "PLAYERAAAAAAAA")
	request[0x1e] = 'A'

	if _, err := client.Write(request); err != nil {
		cancel()
		t.Fatalf("write: %v", err)
	}

	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		cancel()
		t.Fatalf("deadline: %v", err)
	}
	reply := make([]byte, ShopResponseSize+64)
	n, err := client.Read(reply)
	if err != nil {
		cancel()
		t.Fatalf("read: %v", err)
	}

	cancel()
	wg.Wait()

	if n != ShopResponseSize {
		t.Fatalf("reply size: got %d, want %d", n, ShopResponseSize)
	}
	body := reply[:n]
	if body[0] != request[0] || body[1] != request[1] {
		t.Fatalf("header not echoed: got %x, want %x", body[:2], request[:2])
	}
	// Section 0 starts after the 0x20 request header; the count byte sits at
	// section offset 0x32.
	const countOffset = shop.HeaderLength + 0x32
	if count := body[countOffset]; count == 0 {
		t.Fatalf("section 0 part count = 0; shop is empty")
	}
	// The first part row must match the faction-A lineup head.
	if want := shop.CurrentLineup("A"); len(want) > 0 {
		const recordOffset = shop.HeaderLength + 0x34
		gotID := uint32(body[recordOffset]) |
			uint32(body[recordOffset+1])<<8 |
			uint32(body[recordOffset+2])<<16 |
			uint32(body[recordOffset+3])<<24
		if gotID != want[0].PartID {
			t.Fatalf("first part id: got %#x, want %#x", gotID, want[0].PartID)
		}
	}
}
