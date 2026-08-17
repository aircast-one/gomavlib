package streamwriter

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gomavlib/v3/pkg/dialect"
	"github.com/bluenviron/gomavlib/v3/pkg/frame"
	"github.com/bluenviron/gomavlib/v3/pkg/message"
)

// Integration tests for clock functionality in streamwriter

func TestStreamWriterSignedFrameWithMockClock(t *testing.T) {
	// Setup dialect
	dialectDef := &dialect.Dialect{
		Version: 3,
		Messages: []message.Message{
			&MessageTestClockIntegration{},
		},
	}
	dialectRW := &dialect.ReadWriter{Dialect: dialectDef}
	err := dialectRW.Initialize()
	require.NoError(t, err)

	// Fixed time for deterministic signatures
	fixedTime := time.Date(2021, 6, 15, 10, 30, 0, 0, time.UTC)
	clock := frame.MockClock{Time: fixedTime}

	key := frame.NewV2Key(bytes.Repeat([]byte{0x55}, 32))

	var buf bytes.Buffer
	frameWriter := &frame.Writer{
		ByteWriter: &buf,
		DialectRW:  dialectRW,
	}
	err = frameWriter.Initialize()
	require.NoError(t, err)

	writer := &Writer{
		FrameWriter: frameWriter,
		Version:     V2,
		SystemID:    1,
		Key:         key,
		Clock:       clock,
	}
	err = writer.Initialize()
	require.NoError(t, err)

	msg := &MessageTestClockIntegration{Data: 123}
	err = writer.Write(msg)
	require.NoError(t, err)

	// Read and verify
	reader := &frame.Reader{
		ByteReader: bytes.NewReader(buf.Bytes()),
		DialectRW:  dialectRW,
		InKey:      key,
	}
	err = reader.Initialize()
	require.NoError(t, err)

	fr, err := reader.Read()
	require.NoError(t, err)

	v2Frame, ok := fr.(*frame.V2Frame)
	require.True(t, ok, "expected V2Frame")
	require.True(t, v2Frame.IsSigned(), "frame should be signed")

	// Verify timestamp matches mock clock
	expectedTimestamp := uint64(fixedTime.Sub(signatureReferenceDate)) / 10000
	require.Equal(t, expectedTimestamp, v2Frame.SignatureTimestamp)
}

func TestStreamWriterWithRealClockProducesValidSignatures(t *testing.T) {
	// Setup dialect
	dialectDef := &dialect.Dialect{
		Version: 3,
		Messages: []message.Message{
			&MessageTestClockIntegration{},
		},
	}
	dialectRW := &dialect.ReadWriter{Dialect: dialectDef}
	err := dialectRW.Initialize()
	require.NoError(t, err)

	key := frame.NewV2Key(bytes.Repeat([]byte{0x55}, 32))

	var buf bytes.Buffer
	frameWriter := &frame.Writer{
		ByteWriter: &buf,
		DialectRW:  dialectRW,
	}
	err = frameWriter.Initialize()
	require.NoError(t, err)

	// Use default clock (real time)
	writer := &Writer{
		FrameWriter: frameWriter,
		Version:     V2,
		SystemID:    1,
		Key:         key,
	}
	err = writer.Initialize()
	require.NoError(t, err)

	msg := &MessageTestClockIntegration{Data: 456}
	err = writer.Write(msg)
	require.NoError(t, err)

	// Read and verify signature is valid
	reader := &frame.Reader{
		ByteReader: bytes.NewReader(buf.Bytes()),
		DialectRW:  dialectRW,
		InKey:      key,
	}
	err = reader.Initialize()
	require.NoError(t, err)

	fr, err := reader.Read()
	require.NoError(t, err)

	v2Frame, ok := fr.(*frame.V2Frame)
	require.True(t, ok, "expected V2Frame")
	require.True(t, v2Frame.IsSigned(), "frame should be signed")
	require.NotNil(t, v2Frame.Signature, "signature should be present")

	// Verify timestamp is reasonable (after 2015 reference date)
	require.Greater(t, v2Frame.SignatureTimestamp, uint64(0), "timestamp should be positive")
}

func TestStreamWriterClockDefaultsToRealClock(t *testing.T) {
	dialectDef := &dialect.Dialect{
		Version: 3,
		Messages: []message.Message{
			&MessageTestClockIntegration{},
		},
	}
	dialectRW := &dialect.ReadWriter{Dialect: dialectDef}
	err := dialectRW.Initialize()
	require.NoError(t, err)

	var buf bytes.Buffer
	frameWriter := &frame.Writer{
		ByteWriter: &buf,
		DialectRW:  dialectRW,
	}
	err = frameWriter.Initialize()
	require.NoError(t, err)

	writer := &Writer{
		FrameWriter: frameWriter,
		Version:     V2,
		SystemID:    1,
		// Clock not set - should default to real clock
	}
	err = writer.Initialize()
	require.NoError(t, err)

	// Clock should be set after Initialize
	require.NotNil(t, writer.Clock, "Clock should be set after Initialize")

	// Verify it's using real time
	before := time.Now()
	clockTime := writer.Clock.Now()
	after := time.Now()

	require.True(t, !clockTime.Before(before), "clock should return current time")
	require.True(t, !clockTime.After(after), "clock should return current time")
}

// MessageTestClockIntegration is a simple test message for integration tests.
type MessageTestClockIntegration struct {
	Data uint32
}

func (m *MessageTestClockIntegration) GetID() uint32 {
	return 998
}
