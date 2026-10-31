package fcm

import (
	"context"
	"fmt"
	"sync"
)

// Fake is a [Sender] that reaches nothing. It records every message it is asked
// to send and answers from a script, so an application tests the code that
// pushes without a Firebase project, a service account or a network.
//
// It is safe for concurrent use.
type Fake struct {
	mu   sync.Mutex
	sent []Outgoing

	// Fail, when set, is asked about every message and may return the *SendError
	// to answer with, or nil to accept it.
	Fail func(Outgoing) error
}

var _ Sender = (*Fake)(nil)

// Send records the message and answers the scripted outcome.
func (f *Fake) Send(_ context.Context, out Outgoing) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Fail != nil {
		if err := f.Fail(out); err != nil {
			return "", err
		}
	}
	f.sent = append(f.sent, out)
	return fmt.Sprintf("projects/fake/messages/%d", len(f.sent)), nil
}

// Sent is the messages accepted so far, oldest first.
func (f *Fake) Sent() []Outgoing {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Outgoing(nil), f.sent...)
}
