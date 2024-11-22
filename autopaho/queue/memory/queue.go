/*
 * Copyright (c) 2024 Contributors to the Eclipse Foundation
 *
 *  All rights reserved. This program and the accompanying materials
 *  are made available under the terms of the Eclipse Public License v2.0
 *  and Eclipse Distribution License v1.0 which accompany this distribution.
 *
 * The Eclipse Public License is available at
 *    https://www.eclipse.org/legal/epl-2.0/
 *  and the Eclipse Distribution License is available at
 *    http://www.eclipse.org/org/documents/edl-v10.php.
 *
 *  SPDX-License-Identifier: EPL-2.0 OR BSD-3-Clause
 */

package memory

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/canonical/mqtt.golang/autopaho/queue"
)

// A queue implementation that stores all data in RAM

// Queue - basic memory based queue
type Queue struct {
	mu              sync.Mutex
	messages        [][]byte
	waiting         []chan<- struct{} // closed when something arrives in the queue
	waitingForEmpty []chan<- struct{} // closed when queue is empty
}

// New creates a new memory-based queue
func New() *Queue {
	return &Queue{}
}

// Wait returns a channel that is closed when there is something in the queue
func (q *Queue) Wait() chan struct{} {
	c := make(chan struct{})
	q.mu.Lock()
	if len(q.messages) > 0 {
		q.mu.Unlock()
		close(c)
		return c
	}
	q.waiting = append(q.waiting, c)
	q.mu.Unlock()
	return c
}

// WaitForEmpty returns a channel which will be closed when the queue is empty
func (q *Queue) WaitForEmpty() chan struct{} {
	c := make(chan struct{})
	q.mu.Lock()
	if len(q.messages) == 0 {
		q.mu.Unlock()
		close(c)
		return c
	}
	q.waitingForEmpty = append(q.waitingForEmpty, c)
	q.mu.Unlock()
	return c
}

// Enqueue add item to the queue.
func (q *Queue) Enqueue(p io.Reader) error {
	var b bytes.Buffer
	_, err := b.ReadFrom(p)
	if err != nil {
		return fmt.Errorf("Queue.Push failed to read into buffer: %w", err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.messages = append(q.messages, b.Bytes())
	for _, c := range q.waiting {
		close(c)
	}
	q.waiting = q.waiting[:0]
	return nil
}

// Peek retrieves the oldest item from the queue (without removing it)
func (q *Queue) Peek() (queue.Entry, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.messages) == 0 {
		return nil, queue.ErrEmpty
	}
	// Queue implements Entry directly (as this always references q.messages[0]
	return q, nil
}

// Reader implements Entry.Reader - As the entry will always be the first item in the queue this is implemented
// against Queue rather than as a separate struct.
func (q *Queue) Reader() (io.Reader, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.messages) == 0 {
		return nil, queue.ErrEmpty
	}
	return bytes.NewReader(q.messages[0]), nil
}

// Leave implements Entry.Leave - the entry (will be returned on subsequent calls to Peek)
func (q *Queue) Leave() error {
	return nil // No action (item is already in the queue and there is nothing to close)
}

// Remove implements Entry.Remove this entry from the queue
func (q *Queue) Remove() error {
	return q.remove()
}

// Quarantine implements Entry.Quarantine - Flag that this entry has an error (remove from queue, potentially retaining data with error flagged)
func (q *Queue) Quarantine() error {
	return q.remove() // No way for us to actually quarantine this, so we just remove the item from the queue
}

// remove removes the first item in the queue.
func (q *Queue) remove() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	initialLen := len(q.messages)
	if initialLen > 0 {
		q.messages = q.messages[1:]
	}
	if initialLen <= 1 { // Queue is now, or was already, empty
		for _, c := range q.waitingForEmpty {
			close(c)
		}
		q.waitingForEmpty = q.waitingForEmpty[:0]
		if initialLen == 0 {
			return queue.ErrEmpty
		}
	}
	return nil
}
