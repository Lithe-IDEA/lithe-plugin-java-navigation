package lithe

import (
	"context"
	"sync"
	"time"
)

type watcher struct {
	id       uint64
	callback func()
}

// Cancellation is a call's cancellation token. Lithe cancels it when the
// host cancels the call, when the call is unregistered, and when the session
// ends.
type Cancellation struct {
	mu        sync.Mutex
	cancelled bool
	done      chan struct{}
	callbacks []watcher
	nextWatch uint64
	ctxOnce   sync.Once
	ctx       context.Context
}

// NewCancellation returns a token that is not cancelled.
func NewCancellation() *Cancellation {
	return &Cancellation{done: make(chan struct{})}
}

// IsCancelled reports whether the token was cancelled.
func (c *Cancellation) IsCancelled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancelled
}

// Check returns ErrCancelled once the token was cancelled.
func (c *Cancellation) Check() error {
	if c.IsCancelled() {
		return ErrCancelled
	}
	return nil
}

// Done is closed when the token is cancelled.
func (c *Cancellation) Done() <-chan struct{} { return c.done }

// Context is a context.Context cancelled with the token, for code that takes
// one, such as exec.CommandContext or http.NewRequestWithContext. Every call
// returns the same context.
func (c *Cancellation) Context() context.Context {
	c.ctxOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		c.ctx = ctx
		c.OnCancel(cancel)
	})
	return c.ctx
}

// WaitTimeout waits up to timeout and reports whether the token was
// cancelled.
func (c *Cancellation) WaitTimeout(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.done:
		return true
	case <-timer.C:
		return c.IsCancelled()
	}
}

// OnCancel runs callback once the token is cancelled, at once when it
// already is.
func (c *Cancellation) OnCancel(callback func()) {
	c.watch(callback)
}

// Cancel cancels the token. Only the first call has an effect; callbacks run
// outside the token's lock.
func (c *Cancellation) Cancel() {
	c.mu.Lock()
	if c.cancelled {
		c.mu.Unlock()
		return
	}
	c.cancelled = true
	close(c.done)
	callbacks := c.callbacks
	c.callbacks = nil
	c.mu.Unlock()
	for _, watch := range callbacks {
		watch.callback()
	}
}

// watch registers callback and returns its ID; when the token is already
// cancelled it runs callback and returns false.
func (c *Cancellation) watch(callback func()) (uint64, bool) {
	c.mu.Lock()
	if c.cancelled {
		c.mu.Unlock()
		callback()
		return 0, false
	}
	c.nextWatch++
	id := c.nextWatch
	c.callbacks = append(c.callbacks, watcher{id, callback})
	c.mu.Unlock()
	return id, true
}

func (c *Cancellation) unwatch(id uint64) {
	c.mu.Lock()
	for i, watch := range c.callbacks {
		if watch.id == id {
			c.callbacks = append(c.callbacks[:i:i], c.callbacks[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
}

func anyCancelled(scope []*Cancellation) bool {
	for _, token := range scope {
		if token.IsCancelled() {
			return true
		}
	}
	return false
}
