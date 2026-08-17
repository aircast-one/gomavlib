package frame

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gomavlib/v3/pkg/dialect"
	"github.com/bluenviron/gomavlib/v3/pkg/message"
)

// Integration tests for clock functionality

func TestClockDefaultUsesRealTime(t *testing.T) {
	clock := DefaultClock()

	before := time.Now()
	clockTime := clock.Now()
	after := time.Now()

	require.True(t, !clockTime.Before(before), "clock time should not be before the call")
	require.True(t, !clockTime.After(after), "clock time should not be after the call")
}

func TestMockClockReturnsFixedTime(t *testing.T) {
	fixedTime := time.Date(2020, 6, 15, 12, 30, 0, 0, time.UTC)
	clock := MockClock{Time: fixedTime}

	// Multiple calls should return the same time
	require.Equal(t, fixedTime, clock.Now())
	require.Equal(t, fixedTime, clock.Now())
	require.Equal(t, fixedTime, clock.Now())
}

func TestSignedFrameRoundTripWithMockClock(t *testing.T) {
	// Setup dialect
	dialectDef := &dialect.Dialect{
		Version: 3,
		Messages: []message.Message{
			&MessageTestClock{},
		},
	}
	dialectRW := &dialect.ReadWriter{Dialect: dialectDef}
	err := dialectRW.Initialize()
	require.NoError(t, err)

	// Fixed time for deterministic signatures
	fixedTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := MockClock{Time: fixedTime}

	key := NewV2Key(bytes.Repeat([]byte{0x42}, 32))

	// Write a signed frame
	var buf bytes.Buffer
	writer := &Writer{
		ByteWriter:     &buf,
		DialectRW:      dialectRW,
		Clock:          clock,
		OutVersion:     V2,
		OutSystemID:    1,
		OutComponentID: 1,
		OutKey:         key,
	}
	err = writer.Initialize()
	require.NoError(t, err)

	msg := &MessageTestClock{Value: 42}
	err = writer.WriteMessage(msg)
	require.NoError(t, err)

	// Read and verify the frame
	reader := &Reader{
		ByteReader: bytes.NewReader(buf.Bytes()),
		DialectRW:  dialectRW,
		InKey:      key,
	}
	err = reader.Initialize()
	require.NoError(t, err)

	frame, err := reader.Read()
	require.NoError(t, err)

	v2Frame, ok := frame.(*V2Frame)
	require.True(t, ok, "expected V2Frame")
	require.True(t, v2Frame.IsSigned(), "frame should be signed")
	require.NotNil(t, v2Frame.Signature, "signature should be present")

	// Verify timestamp is based on the mock clock
	// Timestamp is in 10 microsecond units since 2015-01-01
	expectedTimestamp := uint64(fixedTime.Sub(signatureReferenceDate)) / 10000
	require.Equal(t, expectedTimestamp, v2Frame.SignatureTimestamp)
}

func TestSignedFrameWithRealClockHasReasonableTimestamp(t *testing.T) {
	// Setup dialect
	dialectDef := &dialect.Dialect{
		Version: 3,
		Messages: []message.Message{
			&MessageTestClock{},
		},
	}
	dialectRW := &dialect.ReadWriter{Dialect: dialectDef}
	err := dialectRW.Initialize()
	require.NoError(t, err)

	key := NewV2Key(bytes.Repeat([]byte{0x42}, 32))

	// Write a signed frame with default (real) clock
	var buf bytes.Buffer
	writer := &Writer{
		ByteWriter:     &buf,
		DialectRW:      dialectRW,
		OutVersion:     V2,
		OutSystemID:    1,
		OutComponentID: 1,
		OutKey:         key,
	}
	err = writer.Initialize()
	require.NoError(t, err)

	beforeWrite := time.Now()
	msg := &MessageTestClock{Value: 42}
	err = writer.WriteMessage(msg)
	require.NoError(t, err)
	afterWrite := time.Now()

	// Read and verify the frame
	reader := &Reader{
		ByteReader: bytes.NewReader(buf.Bytes()),
		DialectRW:  dialectRW,
		InKey:      key,
	}
	err = reader.Initialize()
	require.NoError(t, err)

	frame, err := reader.Read()
	require.NoError(t, err)

	v2Frame, ok := frame.(*V2Frame)
	require.True(t, ok, "expected V2Frame")
	require.True(t, v2Frame.IsSigned(), "frame should be signed")

	// Verify timestamp is within the expected range
	minTimestamp := uint64(beforeWrite.Sub(signatureReferenceDate)) / 10000
	maxTimestamp := uint64(afterWrite.Sub(signatureReferenceDate)) / 10000

	require.GreaterOrEqual(t, v2Frame.SignatureTimestamp, minTimestamp,
		"timestamp should be >= time before write")
	require.LessOrEqual(t, v2Frame.SignatureTimestamp, maxTimestamp,
		"timestamp should be <= time after write")
}

func TestMultipleSignedFramesHaveIncreasingTimestamps(t *testing.T) {
	// Setup dialect
	dialectDef := &dialect.Dialect{
		Version: 3,
		Messages: []message.Message{
			&MessageTestClock{},
		},
	}
	dialectRW := &dialect.ReadWriter{Dialect: dialectDef}
	err := dialectRW.Initialize()
	require.NoError(t, err)

	key := NewV2Key(bytes.Repeat([]byte{0x42}, 32))

	var buf bytes.Buffer
	writer := &Writer{
		ByteWriter:     &buf,
		DialectRW:      dialectRW,
		OutVersion:     V2,
		OutSystemID:    1,
		OutComponentID: 1,
		OutKey:         key,
	}
	err = writer.Initialize()
	require.NoError(t, err)

	// Write multiple frames
	const frameCount = 5
	for i := 0; i < frameCount; i++ {
		msg := &MessageTestClock{Value: uint32(i)}
		err = writer.WriteMessage(msg)
		require.NoError(t, err)
	}

	// Read frames and verify timestamps are non-decreasing
	reader := &Reader{
		ByteReader: bytes.NewReader(buf.Bytes()),
		DialectRW:  dialectRW,
		InKey:      key,
	}
	err = reader.Initialize()
	require.NoError(t, err)

	var lastTimestamp uint64
	for i := 0; i < frameCount; i++ {
		frame, err := reader.Read()
		require.NoError(t, err)

		v2Frame, ok := frame.(*V2Frame)
		require.True(t, ok, "expected V2Frame")

		require.GreaterOrEqual(t, v2Frame.SignatureTimestamp, lastTimestamp,
			"timestamps should be non-decreasing")
		lastTimestamp = v2Frame.SignatureTimestamp
	}
}

// MessageTestClock is a simple test message for integration tests.
type MessageTestClock struct {
	Value uint32
}

func (m *MessageTestClock) GetID() uint32 {
	return 999
}
