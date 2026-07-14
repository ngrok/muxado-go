package muxado

import (
	"testing"
	"time"

	"golang.ngrok.com/muxado/v2/frame"
)

// TestDuplicateSynDoesNotOrphanStream verifies we don't leak a stream if a
// misbehaved client sends a duplicate streamID
func TestDuplicateSynDoesNotOrphanStream(t *testing.T) {
	t.Parallel()

	local, remote := newFakeConnPair()
	remote.Discard()

	sess := Server(local, nil)
	fr := frame.NewFramer(remote, remote)

	// Bare SYN for remote (client) stream id 1, no payload. WriteFrame runs in
	// a goroutine because the write blocks on the pipe until the server's
	// reader consumes the frame.
	syn := new(frame.Data)
	if err := syn.Pack(1, []byte{}, false, true); err != nil {
		t.Fatal(err)
	}
	go func() { _ = fr.WriteFrame(syn) }()

	str, err := sess.AcceptStream()
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}

	// A goroutine blocks reading from the stream, mirroring a server that has
	// accepted a stream and is waiting for the peer to send data (e.g. a typed
	// stream's 4-byte type header).
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 4)
		_, err := str.Read(buf)
		readDone <- err
	}()

	// Let the read park in the stream buffer's cond wait.
	time.Sleep(100 * time.Millisecond)

	// Duplicate SYN for the same id. Before the fix this overwrote the map
	// entry and orphaned the reader above; after the fix it dies the session.
	dup := new(frame.Data)
	if err := dup.Pack(1, []byte{}, false, true); err != nil {
		t.Fatal(err)
	}
	// May error if the session tears down and closes the transport mid-write;
	// we only care that the parked reader is woken.
	_ = fr.WriteFrame(dup)

	select {
	case <-readDone:
		// Reader woke (with an error) instead of leaking. Success.
	case <-time.After(2 * time.Second):
		t.Fatal("Read still blocked after a duplicate SYN + teardown: stream was orphaned")
	}
}
