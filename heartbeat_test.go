package muxado

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// TestHeartbeatAfterTimeout verifies that a late heartbeat rearms the watchdog
// after the previous timer expiration has already been consumed.
func TestHeartbeatAfterTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		events := make(chan bool, 1)
		hb := NewHeartbeat(nil, func(_ time.Duration, timeout bool) {
			events <- timeout
		}, &HeartbeatConfig{Interval: time.Second})
		defer close(hb.closed)

		mark := make(chan time.Duration)
		go hb.check(mark)

		if !<-events {
			t.Fatal("expected initial heartbeat timeout")
		}

		mark <- time.Millisecond
		if <-events {
			t.Fatal("expected successful late heartbeat")
		}

		if !<-events {
			t.Fatal("expected heartbeat timeout after timer reset")
		}
	})
}

// TestHeartbeatFast is a regression test for a 0ms
// timeout being detectable
func TestHeartbeatFast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, serv := net.Pipe()
	stopServerHBs := make(chan struct{})
	go func() {
		sess := Server(serv, nil)
		typed := NewTypedStreamSession(sess)
		hb := NewHeartbeat(typed, func(d time.Duration, timeout bool) {
			if timeout {
				panic("timeout")
			}
		}, &HeartbeatConfig{
			Interval:  5 * time.Millisecond,
			Tolerance: 100 * time.Millisecond,
			Type:      defaultStreamType,
		})
		str, err := hb.AcceptTypedStream()
		if err != nil {
			panic(err)
		}
		<-stopServerHBs
		str.Close()

		<-ctx.Done()
		sess.Close()
	}()

	clientSess := Client(client, nil)
	clientTyped := NewTypedStreamSession(clientSess)
	hb := NewHeartbeat(clientTyped, func(d time.Duration, timeout bool) {
		if timeout {
			panic("timeout")
		}
	}, &HeartbeatConfig{
		Interval:  5 * time.Millisecond,
		Tolerance: 500 * time.Millisecond,
		Type:      defaultStreamType,
	})
	hb.Start()

	for i := 0; i < 10; i++ {
		_, ok := hb.Beat()
		if !ok {
			t.Fatal("beat failed")
		}
	}

}
