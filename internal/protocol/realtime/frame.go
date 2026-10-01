// Package realtime contains the minimal WebSocket framing needed before a
// backend is selected. The proxy buffers only the first client text frame and
// then returns the exact bytes for replay, so no application payload is lost.
package realtime

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

var ErrNeedMore = errors.New("incomplete websocket frame")

const MaxFramePayload = 16 << 20

type Frame struct {
	FIN     bool
	Opcode  byte
	Masked  bool
	Payload []byte
}

func ParseFrame(data []byte) (Frame, int, error) {
	if len(data) < 2 {
		return Frame{}, 0, ErrNeedMore
	}
	f := Frame{FIN: data[0]&0x80 != 0, Opcode: data[0] & 0x0f, Masked: data[1]&0x80 != 0}
	if data[0]&0x70 != 0 {
		return Frame{}, 0, errors.New("websocket frame has unsupported reserved bits")
	}
	if (f.Opcode >= 3 && f.Opcode <= 7) || f.Opcode >= 11 {
		return Frame{}, 0, errors.New("websocket frame has unsupported opcode")
	}
	length := int(data[1] & 0x7f)
	pos := 2
	if length == 126 {
		if len(data) < pos+2 {
			return Frame{}, 0, ErrNeedMore
		}
		length = int(binary.BigEndian.Uint16(data[pos : pos+2]))
		if length < 126 {
			return Frame{}, 0, errors.New("websocket frame uses a non-canonical length")
		}
		pos += 2
	} else if length == 127 {
		if len(data) < pos+8 {
			return Frame{}, 0, ErrNeedMore
		}
		n := binary.BigEndian.Uint64(data[pos : pos+8])
		pos += 8
		if n&(uint64(1)<<63) != 0 {
			return Frame{}, 0, errors.New("websocket frame length has the most-significant bit set")
		}
		if n < 65536 {
			return Frame{}, 0, errors.New("websocket frame uses a non-canonical length")
		}
		if n > uint64(^uint(0)>>1) {
			return Frame{}, 0, errors.New("websocket frame is too large")
		}
		length = int(n)
	}
	if f.Opcode >= 8 {
		if !f.FIN {
			return Frame{}, 0, errors.New("websocket control frame must not be fragmented")
		}
		if length > 125 {
			return Frame{}, 0, errors.New("websocket control frame payload exceeds 125 bytes")
		}
	}
	if length > MaxFramePayload {
		return Frame{}, 0, errors.New("websocket frame payload exceeds limit")
	}
	var mask [4]byte
	if f.Masked {
		if len(data) < pos+4 {
			return Frame{}, 0, ErrNeedMore
		}
		copy(mask[:], data[pos:pos+4])
		pos += 4
	}
	if length < 0 || len(data) < pos+length {
		return Frame{}, 0, ErrNeedMore
	}
	payload := append([]byte(nil), data[pos:pos+length]...)
	if f.Masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	if f.Opcode == 8 {
		if err := validateClosePayload(payload); err != nil {
			return Frame{}, 0, err
		}
	}
	f.Payload = payload
	return f, pos + length, nil
}

// ReadFrame reads one complete WebSocket frame and returns both its decoded
// payload and the exact wire bytes. The wire copy is used by the realtime
// bridge to replay the first client frame after the backend handshake.
func ReadFrame(reader io.Reader) (Frame, []byte, error) {
	if reader == nil {
		return Frame{}, nil, errors.New("websocket reader is nil")
	}
	header := make([]byte, 2)
	if err := readFrameBytes(reader, header); err != nil {
		return Frame{}, nil, err
	}
	lengthCode := int(header[1] & 0x7f)
	extra := 0
	switch lengthCode {
	case 126:
		extra = 2
	case 127:
		extra = 8
	}
	wire := bytes.NewBuffer(append([]byte(nil), header...))
	if extra > 0 {
		part := make([]byte, extra)
		if err := readFrameBytes(reader, part); err != nil {
			return Frame{}, nil, err
		}
		wire.Write(part)
	}
	length := lengthCode
	if lengthCode == 126 {
		length = int(binary.BigEndian.Uint16(wire.Bytes()[2:4]))
	} else if lengthCode == 127 {
		n := binary.BigEndian.Uint64(wire.Bytes()[2:10])
		if n > MaxFramePayload {
			return Frame{}, nil, errors.New("websocket frame payload exceeds limit")
		}
		length = int(n)
	}
	if length > MaxFramePayload {
		return Frame{}, nil, errors.New("websocket frame payload exceeds limit")
	}
	if header[1]&0x80 != 0 {
		mask := make([]byte, 4)
		if err := readFrameBytes(reader, mask); err != nil {
			return Frame{}, nil, err
		}
		wire.Write(mask)
	}
	payload := make([]byte, length)
	if err := readFrameBytes(reader, payload); err != nil {
		return Frame{}, nil, err
	}
	wire.Write(payload)
	frame, used, err := ParseFrame(wire.Bytes())
	if err != nil {
		return Frame{}, nil, err
	}
	if used != wire.Len() {
		return Frame{}, nil, errors.New("invalid websocket frame length")
	}
	return frame, wire.Bytes(), nil
}

// readFrameBytes keeps the streaming and byte-slice parsers consistent: any
// truncated header, mask, or payload is an incomplete frame rather than a raw
// EOF whose meaning depends on which portion happened to be missing.
func readFrameBytes(reader io.Reader, dst []byte) error {
	if _, err := io.ReadFull(reader, dst); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrNeedMore
		}
		return err
	}
	return nil
}

// ReadMessage reads a complete client text message, including legal
// continuation frames, and returns the exact wire bytes for replay. Control
// frames may appear between fragments and are preserved in the wire copy but
// are not included in the message payload.
func ReadMessage(reader io.Reader) (Frame, []byte, error) {
	var frame Frame
	var wireBytes []byte
	// RFC 6455 permits control frames between data frames, including before
	// the first application message. Consume and retain leading ping/pong
	// frames so a normal client heartbeat does not prevent model extraction;
	// a leading close is returned immediately because no later data is valid.
	for {
		next, wire, err := ReadFrame(reader)
		if err != nil {
			return Frame{}, nil, err
		}
		if err := requireClientFrame(next); err != nil {
			return Frame{}, nil, err
		}
		wireBytes = append(wireBytes, wire...)
		switch next.Opcode {
		case 1, 8:
			// The model selector is carried by a text data message. A leading
			// close is returned to the caller immediately so it does not block
			// waiting for a frame that can never arrive.
			frame = next
		case 9, 10:
			// Control frames may precede the first application message.
			continue
		default:
			return Frame{}, nil, errors.New("first websocket data frame must be text")
		}
		break
	}
	if frame.Opcode != 1 {
		return frame, wireBytes, nil
	}
	if frame.FIN {
		return frame, wireBytes, nil
	}
	payload := append([]byte(nil), frame.Payload...)
	for {
		next, nextWire, readErr := ReadFrame(reader)
		if readErr != nil {
			return Frame{}, nil, readErr
		}
		if err := requireClientFrame(next); err != nil {
			return Frame{}, nil, err
		}
		wireBytes = append(wireBytes, nextWire...)
		switch next.Opcode {
		case 0:
			if len(payload)+len(next.Payload) > MaxFramePayload {
				return Frame{}, nil, errors.New("websocket message payload exceeds limit")
			}
			payload = append(payload, next.Payload...)
			if next.FIN {
				frame.Payload = payload
				frame.FIN = true
				return frame, wireBytes, nil
			}
		case 8:
			// A close handshake terminates the message. Preserve the close
			// bytes for replay, but do not wait for continuations after it.
			return frame, wireBytes, nil
		case 9, 10:
			// Ping and pong frames are allowed during fragmentation; retain
			// them for replay and continue waiting for the message.
		default:
			return Frame{}, nil, errors.New("websocket continuation frame has invalid opcode")
		}
	}
}

// ParseMessage is the byte-slice counterpart to ReadMessage. It is used by
// deterministic HTTP test fallbacks where the complete client wire is already
// available in the request body.
func ParseMessage(data []byte) (Frame, int, error) {
	var frame Frame
	used := 0
	// Keep leading control frames in the consumed byte count. The caller
	// replays exactly that prefix after the backend handshake, while model
	// extraction still starts at the first data frame. A leading close is
	// returned immediately instead of waiting for a frame that can never be
	// valid after the close handshake.
	for {
		next, nextUsed, err := ParseFrame(data[used:])
		if err != nil {
			return Frame{}, 0, err
		}
		if err := requireClientFrame(next); err != nil {
			return Frame{}, 0, err
		}
		used += nextUsed
		switch next.Opcode {
		case 1, 8:
			frame = next
		case 9, 10:
			continue
		default:
			return Frame{}, 0, errors.New("first websocket data frame must be text")
		}
		break
	}
	if frame.Opcode != 1 || frame.FIN {
		return frame, used, nil
	}
	payload := append([]byte(nil), frame.Payload...)
	for {
		next, nextUsed, parseErr := ParseFrame(data[used:])
		if parseErr != nil {
			return Frame{}, 0, parseErr
		}
		if err := requireClientFrame(next); err != nil {
			return Frame{}, 0, err
		}
		used += nextUsed
		switch next.Opcode {
		case 0:
			if len(payload)+len(next.Payload) > MaxFramePayload {
				return Frame{}, 0, errors.New("websocket message payload exceeds limit")
			}
			payload = append(payload, next.Payload...)
			if next.FIN {
				frame.Payload = payload
				frame.FIN = true
				return frame, used, nil
			}
		case 8:
			// A close frame terminates the fragmented message.
			return frame, used, nil
		case 9, 10:
			// Control frame; continue to the next continuation.
		default:
			return Frame{}, 0, errors.New("websocket continuation frame has invalid opcode")
		}
	}
}

func requireClientFrame(frame Frame) error {
	if !frame.Masked {
		return errors.New("client websocket frame must be masked")
	}
	return nil
}

// validateClosePayload enforces the wire-level close-frame rules from RFC
// 6455. A close payload is either empty or a two-byte status code followed by
// valid UTF-8 text; accepting a one-byte payload or reserved status code would
// make the bridge forward malformed control frames to an upstream server.
func validateClosePayload(payload []byte) error {
	if len(payload) == 1 {
		return errors.New("websocket close payload must contain a status code")
	}
	if len(payload) == 0 {
		return nil
	}
	code := binary.BigEndian.Uint16(payload[:2])
	if !validCloseCode(code) {
		return fmt.Errorf("websocket close status code %d is not allowed", code)
	}
	if len(payload) > 2 && !utf8.Valid(payload[2:]) {
		return errors.New("websocket close reason is not valid UTF-8")
	}
	return nil
}

func validCloseCode(code uint16) bool {
	return (code >= 1000 && code <= 1003) ||
		(code >= 1007 && code <= 1011) ||
		(code >= 3000 && code <= 4999)
}

func ExtractModel(frame Frame) (string, error) {
	if frame.Opcode != 1 {
		return "", errors.New("realtime model frame must be text")
	}
	if !utf8.Valid(frame.Payload) {
		return "", errors.New("realtime session frame is not valid UTF-8")
	}
	var body map[string]any
	if err := json.Unmarshal(frame.Payload, &body); err != nil {
		return "", fmt.Errorf("decode realtime session frame: %w", err)
	}
	if typ, _ := body["type"].(string); typ != "session.update" && typ != "session.update.model" {
		return "", fmt.Errorf("first realtime frame must be session.update, got %q", typ)
	}
	if model, ok := body["model"].(string); ok && model != "" {
		return model, nil
	}
	if session, ok := body["session"].(map[string]any); ok {
		if model, ok := session["model"].(string); ok && model != "" {
			return model, nil
		}
	}
	return "", errors.New("realtime session.update.model is required")
}

func FirstModel(data []byte) (model string, frameBytes int, err error) {
	frame, used, err := ParseMessage(data)
	if err != nil {
		return "", 0, err
	}
	model, err = ExtractModel(frame)
	return model, used, err
}
