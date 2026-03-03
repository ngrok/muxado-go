package muxado

import (
	"maps"
	"sync"

	"golang.ngrok.com/muxado/v2/frame"
)

const (
	initMapCapacity = 128 // not too much extra memory wasted to avoid allocations
)

// streamMap is a map of stream ids -> streams guarded by a read/write lock
type streamMap struct {
	sync.RWMutex
	table map[frame.StreamId]streamPrivate
}

func (m *streamMap) Get(id frame.StreamId) (streamPrivate, bool) {
	m.RLock()
	defer m.RUnlock()
	if m.table != nil {
		s, ok := m.table[id]
		return s, ok
	}
	return nil, false
}

// Set adds a stream to the map. It returns false if the stream could not be
// added due to the streamMap having been drained already.
func (m *streamMap) Set(id frame.StreamId, str streamPrivate) bool {
	m.Lock()
	defer m.Unlock()
	if m.table == nil {
		// already drained, let our caller know to give up
		return false
	}
	m.table[id] = str
	return true
}

func (m *streamMap) Delete(id frame.StreamId) {
	m.Lock()
	defer m.Unlock()
	if m.table != nil {
		delete(m.table, id)
	}
}

func (m *streamMap) Each(fn func(frame.StreamId, streamPrivate)) {
	m.RLock()
	streams := maps.Clone(m.table)
	m.RUnlock()
	if streams == nil {
		// already drained, do nothing
		return
	}

	for id, str := range streams {
		fn(id, str)
	}
}

// Drain nils out the table under the write lock and returns all streams that
// were in the map. After Drain, Set will return false for any new streams.
func (m *streamMap) Drain() map[frame.StreamId]streamPrivate {
	m.Lock()
	defer m.Unlock()
	streams := m.table
	m.table = nil
	return streams
}

func newStreamMap() *streamMap {
	return &streamMap{table: make(map[frame.StreamId]streamPrivate, initMapCapacity)}
}
