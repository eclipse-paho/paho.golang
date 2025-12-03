package lockfree

import (
	"bytes"
	"io"
	"sync/atomic"
	"unsafe"

	"github.com/eclipse/paho.golang/autopaho/queue"
)

type Queue struct {
	head     unsafe.Pointer // *node
	tail     unsafe.Pointer // *node
	waitChan unsafe.Pointer // *chan struct{}
}

type node struct {
	value []byte
	next  unsafe.Pointer // *node
}

// NewLockFree creates a queue with a dummy node.
func New() *Queue {
	dummy := &node{}
	return &Queue{
		head: unsafe.Pointer(dummy),
		tail: unsafe.Pointer(dummy),
	}
}

// Enqueue adds an item to the queue.
func (q *Queue) Enqueue(p io.Reader) error {
	data, err := io.ReadAll(p)
	if err != nil {
		return err
	}

	n := &node{value: data}
	for {
		tail := (*node)(atomic.LoadPointer(&q.tail))
		next := (*node)(atomic.LoadPointer(&tail.next))
		if tail == (*node)(atomic.LoadPointer(&q.tail)) { // Still the tail?
			if next == nil {
				if atomic.CompareAndSwapPointer(&tail.next, nil, unsafe.Pointer(n)) {
					atomic.CompareAndSwapPointer(&q.tail, unsafe.Pointer(tail), unsafe.Pointer(n))
					// Signal that the queue is not empty if needed
					q.signalNotEmpty()
					return nil
				}
			} else {
				atomic.CompareAndSwapPointer(&q.tail, unsafe.Pointer(tail), unsafe.Pointer(next))
			}
		}
	}
}

// Dequeue removes the oldest item from the queue.
func (q *Queue) Dequeue() error {
	for {
		head := (*node)(atomic.LoadPointer(&q.head))
		tail := (*node)(atomic.LoadPointer(&q.tail))
		next := (*node)(atomic.LoadPointer(&head.next))
		if head == (*node)(atomic.LoadPointer(&q.head)) { // Still the head?
			if head == tail {
				if next == nil {
					return queue.ErrEmpty // Queue is empty
				}
				// Tail falling behind, advance it
				atomic.CompareAndSwapPointer(&q.tail, unsafe.Pointer(tail), unsafe.Pointer(next))
			} else {
				// Read value before CAS, otherwise another dequeue might free the next node
				if atomic.CompareAndSwapPointer(&q.head, unsafe.Pointer(head), unsafe.Pointer(next)) {
					return nil
				}
			}
		}
	}
}

// Peek retrieves the oldest item from the queue without removing it.
func (q *Queue) Peek() (io.ReadCloser, error) {
	for {
		head := (*node)(atomic.LoadPointer(&q.head))
		next := (*node)(atomic.LoadPointer(&head.next))
		if next != nil { // There is an item in the queue
			return io.NopCloser(bytes.NewReader(next.value)), nil
		}

		if atomic.LoadPointer(&q.waitChan) != nil {
			// The wait channel is set, meaning the queue may not be empty
			continue // Retry the loop since the queue state may have changed
		}
		// The queue is empty
		return nil, queue.ErrEmpty
	}
}

// Wait returns a channel that is closed when there is something in the queue.
func (q *Queue) Wait() chan struct{} {
	for {
		if !q.isEmpty() {
			// If the queue is not empty, return a closed channel
			c := make(chan struct{})
			close(c)
			return c
		}

		// Attempt to create a wait channel if it doesn't exist
		if atomic.LoadPointer(&q.waitChan) == nil {
			newCh := make(chan struct{})
			if atomic.CompareAndSwapPointer(&q.waitChan, nil, unsafe.Pointer(&newCh)) {
				return newCh
			}
		}
	}
}

// isEmpty checks if the queue is empty.
func (q *Queue) isEmpty() bool {
	head := (*node)(atomic.LoadPointer(&q.head))
	tail := (*node)(atomic.LoadPointer(&q.tail))
	next := (*node)(atomic.LoadPointer(&head.next))
	return head == tail && next == nil
}

// signalNotEmpty signals that the queue is not empty.
func (q *Queue) signalNotEmpty() {
	chPtr := atomic.LoadPointer(&q.waitChan)
	if chPtr != nil {
		ch := *(*chan struct{})(chPtr)
		close(ch)                             // Close the channel to signal that the queue is not empty
		atomic.StorePointer(&q.waitChan, nil) // Reset the wait channel pointer
	}
}
