package realtime

import (
	"bufio"
	"bytes"
	"errors"
	"testing"
)

func maskedFrame(fin bool, opcode byte, payload []byte, mask [4]byte) []byte {
	first := opcode
	if fin {
		first |= 0x80
	}
	frame := []byte{first, byte(0x80 | len(payload)), mask[0], mask[1], mask[2], mask[3]}
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	return frame
}

func TestFirstModelMaskedFrame(t *testing.T) {
	payload := []byte(`{"type":"session.update","session":{"model":"gpt-realtime"}}`)
	mask := [4]byte{1, 2, 3, 4}
	frame := []byte{0x81, byte(0x80 | len(payload)), mask[0], mask[1], mask[2], mask[3]}
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	model, used, err := FirstModel(frame)
	if err != nil || model != "gpt-realtime" || used != len(frame) {
		t.Fatalf("model=%q used=%d err=%v", model, used, err)
	}
}

func TestFirstModelRejectsNonSession(t *testing.T) {
	payload := []byte(`{"type":"input.audio","audio":"x"}`)
	frame := append([]byte{0x81, byte(len(payload))}, payload...)
	if _, _, err := FirstModel(frame); err == nil {
		t.Fatal("expected rejection")
	}
}

func TestReadFrameReturnsWireBytesForReplay(t *testing.T) {
	payload := []byte(`{"type":"session.update","model":"m"}`)
	mask := [4]byte{9, 8, 7, 6}
	wire := []byte{0x81, byte(0x80 | len(payload)), mask[0], mask[1], mask[2], mask[3]}
	for i, b := range payload {
		wire = append(wire, b^mask[i%4])
	}
	frame, got, err := ReadFrame(bufio.NewReader(bytes.NewReader(wire)))
	if err != nil || !bytes.Equal(got, wire) || string(frame.Payload) != string(payload) {
		t.Fatalf("frame=%+v wire=%x err=%v", frame, got, err)
	}
}

func TestReadFrameReportsTruncatedWireAsNeedMore(t *testing.T) {
	for _, wire := range [][]byte{
		{0x81, 126, 0},                   // incomplete extended length
		{0x81, 0x80 | 1, 1, 2},           // incomplete mask
		{0x81, 0x80 | 1, 1, 2, 3, 4},     // incomplete payload
		{0x81, 127, 0, 0, 0, 0, 0, 0, 0}, // incomplete 64-bit length
	} {
		if _, _, err := ReadFrame(bytes.NewReader(wire)); !errors.Is(err, ErrNeedMore) {
			t.Fatalf("wire %x returned %v, want ErrNeedMore", wire, err)
		}
	}
}

func TestReadMessageDoesNotDiscardBytesBufferedAfterFirstFrame(t *testing.T) {
	firstPayload := []byte(`{"type":"session.update","model":"m"}`)
	secondPayload := []byte(`{"type":"input.text","text":"hi"}`)
	first := maskedFrame(true, 1, firstPayload, [4]byte{1, 2, 3, 4})
	secondWire := maskedFrame(true, 1, secondPayload, [4]byte{5, 6, 7, 8})
	wire := append(append([]byte(nil), first...), secondWire...)
	reader := bytes.NewReader(wire)
	frame, replay, err := ReadMessage(reader)
	if err != nil || string(frame.Payload) != string(firstPayload) || !bytes.Equal(replay, first) {
		t.Fatalf("first frame=%+v replay=%x err=%v", frame, replay, err)
	}
	secondFrame, gotWire, err := ReadFrame(reader)
	if err != nil || string(secondFrame.Payload) != string(secondPayload) || !bytes.Equal(gotWire, wire[len(first):]) {
		t.Fatalf("second frame=%+v wire=%x err=%v", secondFrame, gotWire, err)
	}
}

func TestParseFrameRejectsInvalidControlAndReservedBits(t *testing.T) {
	if _, _, err := ParseFrame([]byte{0x09, 0x00}); err == nil {
		t.Fatal("fragmented ping should be rejected")
	}
	if _, _, err := ParseFrame([]byte{0x88, 126, 0, 126}); err == nil {
		t.Fatal("oversized close control frame should be rejected")
	}
	if _, _, err := ParseFrame([]byte{0xc1, 0x00}); err == nil {
		t.Fatal("reserved websocket bits should be rejected")
	}
}

func TestParseFrameRejectsNonCanonicalExtendedLengths(t *testing.T) {
	shortExtended := append([]byte{0x81, 126, 0, 1}, 'x')
	if _, _, err := ParseFrame(shortExtended); err == nil {
		t.Fatal("16-bit length encoding for a one-byte payload should be rejected")
	}
	tooShort64 := append([]byte{0x81, 127, 0, 0, 0, 0, 0, 0, 0, 1}, 'x')
	if _, _, err := ParseFrame(tooShort64); err == nil {
		t.Fatal("64-bit length encoding below 65536 bytes should be rejected")
	}
	highBit := []byte{0x81, 127, 0x80, 0, 0, 0, 0, 0, 0, 0}
	if _, _, err := ParseFrame(highBit); err == nil {
		t.Fatal("64-bit length with the most-significant bit set should be rejected")
	}
}

func TestParseFrameValidatesClosePayload(t *testing.T) {
	for _, payload := range [][]byte{
		{0x03},             // one-byte status code
		{0x03, 0xec},       // reserved status code 1004
		{0x03, 0xe8, 0xff}, // invalid UTF-8 reason
		{0x03, 0xf7},       // reserved status code 1015
		{0x13, 0x88, 'x'},  // outside the private-use range
	} {
		wire := append([]byte{0x88, byte(len(payload))}, payload...)
		if _, _, err := ParseFrame(wire); err == nil {
			t.Fatalf("invalid close payload %x was accepted", payload)
		}
	}
	for _, payload := range [][]byte{
		nil,
		{0x03, 0xe8},           // normal closure
		{0x0f, 0xa0, 'o', 'k'}, // private-use code 4000
	} {
		wire := append([]byte{0x88, byte(len(payload))}, payload...)
		if _, _, err := ParseFrame(wire); err != nil {
			t.Fatalf("valid close payload %x was rejected: %v", payload, err)
		}
	}
}

func TestExtractModelRejectsInvalidUTF8(t *testing.T) {
	frame := Frame{Opcode: 1, Payload: []byte{'{', 0xff, '}'}}
	if _, err := ExtractModel(frame); err == nil {
		t.Fatal("invalid UTF-8 text frame was accepted")
	}
}

func TestReadMessageJoinsFragmentedClientText(t *testing.T) {
	firstPayload := []byte(`{"type":"session.update","session":{"model":"gpt-`)
	secondPayload := []byte(`realtime"}}`)
	first := maskedFrame(false, 1, firstPayload, [4]byte{1, 2, 3, 4})
	second := maskedFrame(true, 0, secondPayload, [4]byte{5, 6, 7, 8})
	wire := append(append([]byte(nil), first...), second...)
	frame, replay, err := ReadMessage(bufio.NewReader(bytes.NewReader(wire)))
	if err != nil {
		t.Fatal(err)
	}
	if !frame.FIN || frame.Opcode != 1 || !bytes.Equal(frame.Payload, append(firstPayload, secondPayload...)) {
		t.Fatalf("joined frame = %+v", frame)
	}
	if !bytes.Equal(replay, wire) {
		t.Fatalf("replay wire = %x, want %x", replay, wire)
	}
	model, used, err := FirstModel(wire)
	if err != nil || model != "gpt-realtime" || used != len(wire) {
		t.Fatalf("first model = %q used=%d err=%v", model, used, err)
	}
}

func TestParseMessagePreservesControlFrameBetweenFragments(t *testing.T) {
	firstPayload := []byte(`{"type":"session.update","model":"m"`)
	secondPayload := []byte(`}`)
	first := maskedFrame(false, 1, firstPayload, [4]byte{1, 2, 3, 4})
	pong := maskedFrame(true, 10, nil, [4]byte{5, 6, 7, 8})
	second := maskedFrame(true, 0, secondPayload, [4]byte{9, 10, 11, 12})
	wire := append(append(append([]byte(nil), first...), pong...), second...)
	frame, used, err := ParseMessage(wire)
	if err != nil {
		t.Fatal(err)
	}
	if used != len(wire) || string(frame.Payload) != string(append(firstPayload, secondPayload...)) {
		t.Fatalf("frame=%+v used=%d", frame, used)
	}
}

func TestFirstModelAllowsLeadingMaskedControlFrame(t *testing.T) {
	ping := maskedFrame(true, 9, []byte("heartbeat"), [4]byte{1, 2, 3, 4})
	update := maskedFrame(true, 1, []byte(`{"type":"session.update","session":{"model":"gpt-realtime"}}`), [4]byte{5, 6, 7, 8})
	wire := append(append([]byte(nil), ping...), update...)
	model, used, err := FirstModel(wire)
	if err != nil || model != "gpt-realtime" || used != len(wire) {
		t.Fatalf("model=%q used=%d err=%v", model, used, err)
	}
}

func TestReadMessageReplaysLeadingControlFrame(t *testing.T) {
	ping := maskedFrame(true, 9, nil, [4]byte{1, 2, 3, 4})
	update := maskedFrame(true, 1, []byte(`{"type":"session.update","model":"m"}`), [4]byte{5, 6, 7, 8})
	wire := append(append([]byte(nil), ping...), update...)
	frame, replay, err := ReadMessage(bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	if frame.Opcode != 1 || string(frame.Payload) != `{"type":"session.update","model":"m"}` {
		t.Fatalf("frame=%+v", frame)
	}
	if !bytes.Equal(replay, wire) {
		t.Fatalf("replay=%x want=%x", replay, wire)
	}
}

func TestFirstModelReturnsLeadingCloseWithoutWaitingForData(t *testing.T) {
	closeFrame := maskedFrame(true, 8, nil, [4]byte{1, 2, 3, 4})
	_, used, err := FirstModel(closeFrame)
	if err == nil || used != len(closeFrame) {
		t.Fatalf("leading close should fail model extraction without waiting for data: used=%d err=%v", used, err)
	}
}

func TestReadMessageRejectsUnmaskedClientFrame(t *testing.T) {
	payload := []byte(`{"type":"session.update","model":"m"}`)
	wire := append([]byte{0x81, byte(len(payload))}, payload...)
	if _, _, err := ReadMessage(bytes.NewReader(wire)); err == nil {
		t.Fatal("expected unmasked client frame to be rejected")
	}
	if _, _, err := ParseMessage(wire); err == nil {
		t.Fatal("expected unmasked client frame to be rejected by ParseMessage")
	}
}

func TestFirstModelRejectsLeadingContinuationOrBinaryFrame(t *testing.T) {
	for _, opcode := range []byte{0, 2} {
		wire := maskedFrame(true, opcode, []byte(`{"type":"session.update","model":"m"}`), [4]byte{1, 2, 3, 4})
		if _, _, err := FirstModel(wire); err == nil {
			t.Fatalf("opcode %d was accepted as the first data frame", opcode)
		}
		if _, _, err := ReadMessage(bytes.NewReader(wire)); err == nil {
			t.Fatalf("ReadMessage accepted opcode %d as the first data frame", opcode)
		}
	}
}

func TestParseMessageStopsAtCloseDuringFragmentation(t *testing.T) {
	first := maskedFrame(false, 1, []byte(`{"type":"session.update","model":"m"`), [4]byte{1, 2, 3, 4})
	close := maskedFrame(true, 8, nil, [4]byte{5, 6, 7, 8})
	wire := append(append([]byte(nil), first...), close...)
	frame, used, err := ParseMessage(wire)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Opcode != 1 || frame.FIN || used != len(wire) {
		t.Fatalf("close should terminate fragmented read: frame=%+v used=%d", frame, used)
	}
}
