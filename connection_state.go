package gomavlib

import "fmt"

// ConnectionState represents the current state of a durable connection endpoint
type ConnectionState int32

const (
	ConnStateDisconnected ConnectionState = iota
	ConnStateConnecting
	ConnStateConnected
	ConnStateReconnecting
)

func (s ConnectionState) String() string {
	switch s {
	case ConnStateDisconnected:
		return "disconnected"
	case ConnStateConnecting:
		return "connecting"
	case ConnStateConnected:
		return "connected"
	case ConnStateReconnecting:
		return "reconnecting"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// ConnectionStateCallback is called when the connection state changes
type ConnectionStateCallback func(oldState, newState ConnectionState, err error)
