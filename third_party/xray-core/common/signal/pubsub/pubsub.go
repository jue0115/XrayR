package pubsub

import (
	"errors"
	"sync"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/signal/done"
	"github.com/xtls/xray-core/common/task"
)

type Subscriber struct {
	buffer chan interface{}
	done   *done.Instance
}

func (s *Subscriber) push(msg interface{}) {
	select {
	case s.buffer <- msg:
	default:
	}
}

func (s *Subscriber) Wait() <-chan interface{} {
	return s.buffer
}

func (s *Subscriber) Close() error {
	return s.done.Close()
}

func (s *Subscriber) IsClosed() bool {
	return s.done.Done()
}

type Service struct {
	sync.RWMutex
	subs   map[string][]*Subscriber
	ctask  *task.Periodic
	closed bool
}

// Close stops cleanup and releases all subscribers.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.Lock()
	defer s.Unlock()
	s.closed = true
	_ = s.ctask.Close()
	for _, subscribers := range s.subs {
		for _, subscriber := range subscribers {
			_ = subscriber.Close()
		}
	}
	s.subs = make(map[string][]*Subscriber)
	return nil
}

func NewService() *Service {
	s := &Service{
		subs: make(map[string][]*Subscriber),
	}
	s.ctask = &task.Periodic{
		Execute:  s.Cleanup,
		Interval: time.Second * 30,
	}
	return s
}

// Cleanup cleans up internal caches of subscribers.
// Visible for testing only.
func (s *Service) Cleanup() error {
	s.Lock()
	defer s.Unlock()

	if len(s.subs) == 0 {
		return errors.New("nothing to do")
	}

	for name, subs := range s.subs {
		newSub := make([]*Subscriber, 0, len(s.subs))
		for _, sub := range subs {
			if !sub.IsClosed() {
				newSub = append(newSub, sub)
			}
		}
		if len(newSub) == 0 {
			delete(s.subs, name)
		} else {
			s.subs[name] = newSub
		}
	}

	if len(s.subs) == 0 {
		s.subs = make(map[string][]*Subscriber)
	}
	return nil
}

func (s *Service) Subscribe(name string) *Subscriber {
	sub := &Subscriber{
		buffer: make(chan interface{}, 16),
		done:   done.New(),
	}
	s.Lock()
	if s.closed {
		_ = sub.Close()
		s.Unlock()
		return sub
	}
	s.subs[name] = append(s.subs[name], sub)
	s.Unlock()
	common.Must(s.ctask.Start())
	return sub
}

func (s *Service) Publish(name string, message interface{}) {
	s.RLock()
	defer s.RUnlock()

	for _, sub := range s.subs[name] {
		if !sub.IsClosed() {
			sub.push(message)
		}
	}
}
