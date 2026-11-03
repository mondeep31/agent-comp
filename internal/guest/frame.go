package guest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
)

// MaxFrameBytes matches agentd's limit.
const MaxFrameBytes = 4 << 20

var errFrameTooLarge = errors.New("guest: frame too large")

// WriteFrame writes one length-prefixed frame.
func WriteFrame(w io.Writer, f *agentdv1.Frame) error {
	b, err := proto.Marshal(f)
	if err != nil {
		return fmt.Errorf("guest: encoding frame: %w", err)
	}
	if len(b) > MaxFrameBytes {
		return errFrameTooLarge
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b))) //nolint:gosec // bounded by MaxFrameBytes
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// ReadFrame returns io.EOF only on a clean close between frames.
func ReadFrame(r io.Reader) (*agentdv1.Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameBytes {
		return nil, errFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	f := &agentdv1.Frame{}
	if err := proto.Unmarshal(buf, f); err != nil {
		return nil, fmt.Errorf("guest: decoding frame: %w", err)
	}
	return f, nil
}
