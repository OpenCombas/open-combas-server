package server

import "net"

var (
	ChromeHoundsHeader = [4]byte{'C', 'H', '0', '0'}
)

// isTimeoutError checks if an error is a network timeout
func isTimeoutError(err error) bool {
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	return false
}
