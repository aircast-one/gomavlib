package gomavlib

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNode_AddEndpoint(t *testing.T) {
	// Create a node with one endpoint
	node := &Node{
		Endpoints: []Endpoint{
			&EndpointUDPClient{Address: "127.0.0.1:5600"},
		},
		OutVersion:       V2,
		OutSystemID:      10,
		HeartbeatDisable: true,
	}
	err := node.Initialize()
	require.NoError(t, err)
	defer node.Close()

	// Verify initial endpoint count
	endpoints := node.GetEndpoints()
	require.Len(t, endpoints, 1)

	// Add a new endpoint dynamically
	err = node.AddEndpoint(&EndpointUDPClient{Address: "127.0.0.1:5601"})
	require.NoError(t, err)

	// Wait a bit for the endpoint to be added
	time.Sleep(100 * time.Millisecond)

	// Verify endpoint was added
	endpoints = node.GetEndpoints()
	require.Len(t, endpoints, 2)
}

func TestNode_RemoveEndpoint(t *testing.T) {
	// Create a node with two endpoints
	node := &Node{
		Endpoints: []Endpoint{
			&EndpointUDPClient{Address: "127.0.0.1:5600"},
			&EndpointUDPClient{Address: "127.0.0.1:5601"},
		},
		OutVersion:       V2,
		OutSystemID:      10,
		HeartbeatDisable: true,
	}
	err := node.Initialize()
	require.NoError(t, err)
	defer node.Close()

	// Get initial endpoints
	endpoints := node.GetEndpoints()
	require.Len(t, endpoints, 2)

	// Remove one endpoint
	err = node.RemoveEndpoint(endpoints[0])
	require.NoError(t, err)

	// Wait a bit for the endpoint to be removed
	time.Sleep(100 * time.Millisecond)

	// Verify endpoint was removed
	endpoints = node.GetEndpoints()
	require.Len(t, endpoints, 1)
}

func TestNode_RemoveEndpoint_NotFound(t *testing.T) {
	// Create a node with one endpoint
	node := &Node{
		Endpoints: []Endpoint{
			&EndpointUDPClient{Address: "127.0.0.1:5600"},
		},
		OutVersion:       V2,
		OutSystemID:      10,
		HeartbeatDisable: true,
	}
	err := node.Initialize()
	require.NoError(t, err)
	defer node.Close()

	// Try to remove a non-existent endpoint
	ep := &EndpointUDPClient{Address: "127.0.0.1:9999"}
	require.NoError(t, ep.init(node))
	err = node.RemoveEndpoint(ep)
	require.Error(t, err)
	require.Contains(t, err.Error(), "endpoint not found")
}

func TestNode_AddRemoveMultiple(t *testing.T) {
	// Create a node with one endpoint
	node := &Node{
		Endpoints: []Endpoint{
			&EndpointUDPClient{Address: "127.0.0.1:5600"},
		},
		OutVersion:       V2,
		OutSystemID:      10,
		HeartbeatDisable: true,
	}
	err := node.Initialize()
	require.NoError(t, err)
	defer node.Close()

	// Add multiple endpoints
	for range 3 {
		err = node.AddEndpoint(&EndpointUDPClient{Address: "127.0.0.1:5601"})
		require.NoError(t, err)
	}

	time.Sleep(100 * time.Millisecond)
	endpoints := node.GetEndpoints()
	require.Len(t, endpoints, 4)

	// Remove all but the first endpoint
	for _, ep := range endpoints[1:] {
		err = node.RemoveEndpoint(ep)
		require.NoError(t, err)
	}

	time.Sleep(100 * time.Millisecond)
	endpoints = node.GetEndpoints()
	require.Len(t, endpoints, 1)
}

func TestNode_AddEndpoint_AfterClose(t *testing.T) {
	// Create and close a node
	node := &Node{
		Endpoints: []Endpoint{
			&EndpointUDPClient{Address: "127.0.0.1:5600"},
		},
		OutVersion:       V2,
		OutSystemID:      10,
		HeartbeatDisable: true,
	}
	err := node.Initialize()
	require.NoError(t, err)
	node.Close()

	// Try to add endpoint after close
	err = node.AddEndpoint(&EndpointUDPClient{Address: "127.0.0.1:5601"})
	require.Error(t, err)
	require.Equal(t, errTerminated, err)
}
