package gomavlib

import (
	"io"
)

var _ Endpoint = (*EndpointCustomPersistent)(nil)

// EndpointCustomPersistent sets up an endpoint for pre-established, persistent connections
// (e.g., WebSocket, named pipes, or any long-lived connection).
//
// Unlike EndpointCustomClient, this ensures only ONE channel is created per connection,
// preventing goroutine leaks for persistent connections.
type EndpointCustomPersistent struct {
	// struct or interface implementing Read(), Write() and Close()
	ReadWriteCloser io.ReadWriteCloser

	// whether the connection is datagram-based (e.g. UDP).
	IsDatagram bool

	provided  bool
	terminate chan struct{}
}

func (e *EndpointCustomPersistent) init(_ *Node) error {
	e.terminate = make(chan struct{})
	return nil
}

func (e *EndpointCustomPersistent) isEndpoint() {}

func (e *EndpointCustomPersistent) close() {
	close(e.terminate)
	e.ReadWriteCloser.Close()
}

func (e *EndpointCustomPersistent) oneChannelAtAtime() bool {
	return false
}

func (e *EndpointCustomPersistent) isDatagram() bool {
	return e.IsDatagram
}

func (e *EndpointCustomPersistent) provide() (string, io.ReadWriteCloser, error) {
	if e.provided {
		<-e.terminate
		return "", nil, errTerminated
	}

	e.provided = true

	return "persistent", &removeCloser{e.ReadWriteCloser}, nil
}
