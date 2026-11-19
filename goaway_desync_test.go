package muxado

import (
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestGoAwayDoesNotConsumeFollowingFrame is a regression test for a GOAWAY
// bug where we didn't update our byte-counting correctly for goaway frames.
func TestGoAwayDoesNotConsumeFollowingFrame(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		// this test requires parallelism to reproduce
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	}

	clientConn, serverConn := tcpConnPair(t)
	client := Client(clientConn, nil)
	server := Server(serverConn, nil)

	// The server opens streams and writes to each, as we do for proxying real
	// connections.
	srvStreams := make([]Stream, 0, 16)
	for range 16 {
		st, err := server.OpenStream()
		if err != nil {
			t.Fatalf("server OpenStream: %v", err)
		}
		srvStreams = append(srvStreams, st)
	}
	go func() {
		for _, st := range srvStreams {
			_, _ = st.Write([]byte("hello there"))
		}
	}()

	cliStreams := make([]Stream, 0, 16)
	for range 16 {
		st, err := client.AcceptStream()
		if err != nil {
			t.Fatalf("client AcceptStream: %v", err)
		}
		cliStreams = append(cliStreams, st)
	}

	// Close the session and half-close every stream at once, with no
	// coordination, so a FIN can race onto the wire just behind the GOAWAY.
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Go(func() { ; <-start; _ = client.Close() })
	for _, st := range cliStreams {
		wg.Go(func() { ; <-start; _ = st.CloseWrite() })
	}
	close(start)

	// The client's GOAWAY carries the debug text "no error". If the decoder
	// over-reads it, the following frame's header is swallowed into the debug
	// and the transport desyncs. Wait returns the debug the server decoded.
	debugCh := make(chan []byte, 1)
	go func() {
		_, _, debug := server.Wait()
		debugCh <- debug
	}()

	select {
	case debug := <-debugCh:
		if string(debug) != "no error" {
			t.Fatalf("server decoded GOAWAY debug %q (len %d), want %q: the decoder read past the debug field into the following frame",
				debug, len(debug), "no error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server session hang: the GOAWAY over-read desynced the reader")
	}

	wg.Wait()
	_ = client.Close()
	_ = server.Close()
}

func tcpConnPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()

	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server = <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	return client, server
}
