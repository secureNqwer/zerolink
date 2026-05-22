// Package core – EventBus implementation.
// Subscribers receive typed events via buffered channels.
// The bus is thread-safe and non-blocking (slow subscribers drop events).
package core

import (
	"sync"
	"time"
)

type bus struct {
	mu   sync.RWMutex
	subs map[string]*subscription
}

type subscription struct {
	id    string
	types map[EventType]struct{} // empty map = all types
	ch    chan Event
}

// NewEventBus creates a new in-process event bus.
func NewEventBus() EventBus {
	return &bus{subs: make(map[string]*subscription)}
}

// Publish broadcasts the event to all matching subscribers.
// Delivery is non-blocking: a full subscriber channel drops the event for that sub.
func (b *bus) Publish(event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sub := range b.subs {
		if len(sub.types) > 0 {
			if _, ok := sub.types[event.Type]; !ok {
				continue
			}
		}
		select {
		case sub.ch <- event:
		default:
			// subscriber is slow – event dropped for this subscriber
		}
	}
}

// Subscribe registers a subscriber with the given ID and returns its channel.
// If a subscription with the same ID already exists its old channel is closed
// before the new one is created (prevents goroutine leaks).
// Providing no types subscribes to all events.
func (b *bus) Subscribe(id string, types ...EventType) <-chan Event {
	sub := &subscription{
		id:    id,
		types: make(map[EventType]struct{}),
		ch:    make(chan Event, 256),
	}
	for _, t := range types {
		sub.types[t] = struct{}{}
	}

	b.mu.Lock()
	// Close existing channel for this ID so callers waiting on it unblock.
	if old, ok := b.subs[id]; ok {
		close(old.ch)
	}
	b.subs[id] = sub
	b.mu.Unlock()

	return sub.ch
}

// Unsubscribe removes and closes a subscriber's channel.
func (b *bus) Unsubscribe(id string) {
	b.mu.Lock()
	sub, ok := b.subs[id]
	if ok {
		delete(b.subs, id)
	}
	b.mu.Unlock()
	if ok {
		close(sub.ch)
	}
}
