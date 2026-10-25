package probe

import (
	"context"
	"sync"
)

// FakeICMP answers echoes with a function and records the requests.
type FakeICMP struct {
	mu       sync.Mutex
	Reply    func(req EchoRequest) (EchoReply, error)
	Requests []EchoRequest
}

func (f *FakeICMP) Echo(_ context.Context, req EchoRequest) (EchoReply, error) {
	f.mu.Lock()
	f.Requests = append(f.Requests, req)
	reply := f.Reply
	f.mu.Unlock()
	return reply(req)
}
