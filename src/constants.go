package main

// Protocol constants
const (
	MinHelloMessageSize = 31
	StatusResponseSize  = 64

	MaxBufferSize = 65535
)

var (
	ChromeHoundsHeader = [4]byte{'C', 'H', '0', '0'}
)
