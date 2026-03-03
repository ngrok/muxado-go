package muxado

import (
	"testing"
	"time"

	"golang.ngrok.com/muxado/v2/frame"
)

// TestDieRaceMissesOrphanedStream is a regression test for a race condition in
// session.die() where a stream created concurrently with session shutdown is
// never cleaned up.
//
// The race:
//  1. Reader goroutine enters handleSyn, calls newStream (custom factory blocks)
//  2. Another goroutine calls sess.Close() -> die() -> streams.Each snapshots
//     an empty map
//  3. Factory unblocks, handleSyn calls streams.Set (stream now in map but
//     missed by die's snapshot)
//  4. The stream's buffer never gets an error set, so any Read blocks forever
func TestDieRaceMissesOrphanedStream(t *testing.T) {
	t.Parallel()

	local, remote := newFakeConnPair()
	remote.Discard()

	factoryCalled := make(chan struct{})
	factoryProceed := make(chan struct{})

	var createdStream streamPrivate

	customFactory := func(sess sessionPrivate, id frame.StreamId, windowSize uint32, fin bool, init bool) streamPrivate {
		str := newStream(sess, id, windowSize, fin, init)
		createdStream = str
		close(factoryCalled)
		<-factoryProceed
		return str
	}

	sess := Server(local, &Config{newStream: customFactory})

	// Send a zero-length SYN. WriteFrame is launched in a goroutine because
	// the zero-length body write blocks on the pipe until the reader side
	// returns from ReadFrame — but the reader is about to block in our
	// factory. The goroutine unblocks when the transport is closed by die().
	f := new(frame.Data)
	f.Pack(1, []byte{}, false, true)
	fr := frame.NewFramer(remote, remote)
	go fr.WriteFrame(f)

	// Wait for the factory to be called. At this point the reader goroutine
	// is inside handleSyn, between newStream and streams.Set.
	<-factoryCalled

	// Close the session while the factory is blocked.
	// die() snapshots an EMPTY stream map because Set() hasn't been called yet.
	sess.Close()

	// Unblock the factory. handleSyn continues:
	//   streams.Set (adds to map — but die() already snapshotted)
	//   accept <- str (channel has room, reader hasn't returned yet)
	//   handleStreamData (no-op for zero-length frame)
	// Then the reader checks s.dead, sees it closed, and returns.
	close(factoryProceed)

	// Give handleSyn time to finish.
	time.Sleep(200 * time.Millisecond)

	// The stream was NOT cleaned up by die() because it was added after
	// the snapshot. Verify by reading — it should block forever because
	// neither data nor error was ever set on the buffer.
	readDone := make(chan error, 1)
	go func() {
		readBuf := make([]byte, 1)
		_, err := createdStream.Read(readBuf)
		readDone <- err
	}()

	select {
	case <-time.After(500 * time.Millisecond):
		// Expected: Read blocks because the stream was orphaned by die().
		panic("test test failed, read never finished")
	case <-readDone:
		t.Log("read finsihed, success")
	}
}
