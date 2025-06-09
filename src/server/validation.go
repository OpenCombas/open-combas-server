package server

import "fmt"

// ValidationError represents a packet validation error
type ValidationError struct {
	Reason string
	Size   int
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("validation failed: %s (packet size: %d)", e.Reason, e.Size)
}
