package shellsession

import "sync"

type subscriber struct {
	fn       func(Chunk)
	ch       chan Chunk
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func newSubscriber(fn func(Chunk)) *subscriber {
	s := &subscriber{
		fn:   fn,
		ch:   make(chan Chunk, subscriberBuffer),
		done: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.pump()
	return s
}

func (s *subscriber) pump() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case c := <-s.ch:
			s.fn(c)
		}
	}
}

func (s *subscriber) deliver(c Chunk) {
	select {
	case <-s.done:
	case s.ch <- c:
	}
}

func (s *subscriber) stop() {
	s.stopOnce.Do(func() { close(s.done) })
}
