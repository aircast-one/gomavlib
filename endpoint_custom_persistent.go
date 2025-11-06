package gomavlib

import (
	"io"
)

// EndpointCustomPersistent sets up an endpoint for pre-established, persistent connections
// (e.g., WebSocket, named pipes, or any long-lived connection).
//
// Unlike EndpointCustom, this ensures only ONE channel is created per connection,
// preventing goroutine leaks for persistent connections.
type EndpointCustomPersistent struct {
	// struct or interface implementing Read(), Write() and Close()
	ReadWriteCloser io.ReadWriteCloser
}

func (conf EndpointCustomPersistent) init(node *Node) (Endpoint, error) {
	e := &endpointCustomPersistent{
		node: node,
		conf: conf,
	}
	err := e.initialize()
	return e, err
}

type endpointCustomPersistent struct {
	node *Node
	conf EndpointCustomPersistent

	rwc       io.ReadWriteCloser
	provided  bool           // Track if we've already provided the connection
	terminate chan struct{} // Signal termination
}

func (e *endpointCustomPersistent) close() {
	close(e.terminate)
	e.rwc.Close()
}

func (e *endpointCustomPersistent) initialize() error {
	e.rwc = e.conf.ReadWriteCloser
	e.terminate = make(chan struct{})
	return nil
}

func (e *endpointCustomPersistent) isEndpoint() {}

func (e *endpointCustomPersistent) Conf() EndpointConf {
	return e.conf
}

func (e *endpointCustomPersistent) oneChannelAtAtime() bool {
	// Return false to allow provide() to be called immediately.
	// We control the single-channel behavior in provide() by blocking
	// after the first successful provision.
	return false
}

func (e *endpointCustomPersistent) provide() (string, io.ReadWriteCloser, error) {
	// Only provide the connection once. After that, block until termination.
	if e.provided {
		// Block until the endpoint is terminated
		<-e.terminate
		return "", nil, errTerminated
	}

	e.provided = true

	// Use removeCloser wrapper (defined in endpoint_custom.go) to prevent gomavlib
	// from closing the connection when the channel closes. The connection lifecycle
	// is managed externally.
	return "persistent", &removeCloser{e.rwc}, nil
}
