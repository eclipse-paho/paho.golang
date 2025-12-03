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

package queue

import (
	"errors"
	"io"
)

var (
	ErrEmpty = errors.New("empty queue")
)

// Entry - permits access to a queue entry
// Users must call one of Leave, Remove, or Quarantine when done with the entry (and before calling Peek again)
// `Reader()` must not be called after calling Leave, Remove, or Quarantine (and any Reader previously requestes should be considered invalid)
type Entry interface {
	Reader() (io.Reader, error) // Provides access to the file contents, subsequent calls may return the same reader
	Leave() error               // Leave the entry in the queue (same entry will be returned on subsequent calls to Peek).
	Remove() error              // Remove this entry from the queue. Returns queue.ErrEmpty if queue is empty after operation
	Quarantine() error          // Flag that this entry has an error (remove from queue, potentially retaining data with error flagged)
}

// Queue provides the functionality needed to manage queued messages
type Queue interface {
	// Wait returns a channel that is closed when there is something in the queue (will return a closed channel if the
	// queue is empty at the time of the call)
	// Can be called multiple times.
	Wait() chan struct{}

	// Enqueue add item to the queue.
	Enqueue(p io.Reader) error

	// Peek retrieves the oldest item from the queue without removing it
	// Users must call one of Close, Remove, or Quarantine when done with the entry, and before calling Peek again.
	// Warning: Peek is not safe for concurrent use (it may return the same Entry leading to unpredictable results)
	Peek() (Entry, error)
}
