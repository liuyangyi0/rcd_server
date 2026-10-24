package api

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Broker is a bounded best-effort invalidation stream. On subscriber overflow
// it replaces queued detail with one resync event, so consumers never mistake a
// dropped event for a complete state history.
type Broker struct {
	mu          sync.RWMutex
	subscribers map[uint64]brokerSubscriber
	nextID      atomic.Uint64
	dropped     atomic.Uint64
	clock       func() time.Time
	closed      bool
}

type brokerSubscriber struct {
	events chan Event
	cancel context.CancelFunc
}

func NewBroker() *Broker {
	return &Broker{subscribers: make(map[uint64]brokerSubscriber), clock: time.Now}
}

func (b *Broker) Subscribe(ctx context.Context) (Subscription, error) {
	subscriptionContext, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		cancel()
		return Subscription{}, fmt.Errorf("event broker closed")
	}
	id := b.nextID.Add(1)
	channel := make(chan Event, 64)
	b.subscribers[id] = brokerSubscriber{events: channel, cancel: cancel}
	b.mu.Unlock()
	var once sync.Once
	closeSubscription := func() {
		once.Do(func() {
			cancel()
			b.mu.Lock()
			if existing, ok := b.subscribers[id]; ok {
				delete(b.subscribers, id)
				close(existing.events)
			}
			b.mu.Unlock()
		})
	}
	go func() {
		<-subscriptionContext.Done()
		closeSubscription()
	}()
	return Subscription{Events: channel, Close: closeSubscription}, nil
}

func (b *Broker) Publish(event Event) {
	if event.Time.IsZero() {
		event.Time = b.clock().UTC()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for _, subscriber := range b.subscribers {
		select {
		case subscriber.events <- event:
		default:
			b.dropped.Add(1)
			// Remove one stale detail item and ask this consumer for a fresh query.
			select {
			case <-subscriber.events:
			default:
			}
			select {
			case subscriber.events <- Event{Type: "resync", Time: event.Time}:
			default:
			}
		}
	}
}

func (b *Broker) Stats() (subscribers int, dropped uint64) {
	b.mu.RLock()
	subscribers = len(b.subscribers)
	b.mu.RUnlock()
	return subscribers, b.dropped.Load()
}

func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, subscriber := range b.subscribers {
		delete(b.subscribers, id)
		subscriber.cancel()
		close(subscriber.events)
	}
}
