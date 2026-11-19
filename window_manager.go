package muxado

import (
	"os"
	"sync"
	"time"
)

type windowManager interface {
	Increment(int)
	Decrement(int) (int, error)
	SetError(error)
	SetDeadline(time.Time)
	Deadline() time.Time
}

type condWindow struct {
	val      int
	maxSize  int
	err      error
	deadline time.Time
	timer    *time.Timer
	sync.Cond
	sync.Mutex
}

func (w *condWindow) Init(initialSize int) {
	w.val = initialSize
	w.maxSize = initialSize
	w.L = &w.Mutex
}

func (w *condWindow) Increment(inc int) {
	w.L.Lock()
	w.val += inc
	w.Broadcast()
	w.L.Unlock()
}

func (w *condWindow) SetError(err error) {
	w.L.Lock()
	w.err = err
	if w.timer != nil {
		w.timer.Stop()
	}
	w.Broadcast()
	w.L.Unlock()
}

func (w *condWindow) SetDeadline(t time.Time) {
	w.L.Lock()
	w.deadline = t
	if timeout := time.Until(t); timeout > 0 {
		if w.timer == nil {
			w.timer = time.AfterFunc(timeout, w.notifyDeadline)
		} else {
			w.timer.Reset(timeout)
		}
	} else if w.timer != nil {
		w.timer.Stop()
	}
	w.Broadcast()
	w.L.Unlock()
}

func (w *condWindow) Deadline() time.Time {
	w.L.Lock()
	defer w.L.Unlock()
	return w.deadline
}

func (w *condWindow) notifyDeadline() {
	w.L.Lock()
	w.Broadcast()
	w.L.Unlock()
}

func (w *condWindow) Decrement(dec int) (ret int, err error) {
	if dec == 0 {
		return
	}

	w.L.Lock()
	for {
		if w.err != nil {
			err = w.err
			break
		}
		if !w.deadline.IsZero() && !time.Now().Before(w.deadline) {
			err = os.ErrDeadlineExceeded
			break
		}

		if w.val > 0 {
			if dec > w.val {
				ret = w.val
				w.val = 0
				break
			} else {
				ret = dec
				w.val -= dec
				break
			}
		} else {
			w.Wait()
		}
	}
	w.L.Unlock()
	return
}
