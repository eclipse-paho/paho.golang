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
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/canonical/mqtt.golang/autopaho/queue"
)

// TestMemoryQueue some basic tests of the queue
func TestMemoryQueue(t *testing.T) {
	q := New()

	if _, err := q.Peek(); !errors.Is(err, queue.ErrEmpty) {
		t.Errorf("expected ErrEmpty, got %s", err)
	}

	queueNotEmpty := make(chan struct{})
	go func() {
		<-q.Wait()
		close(queueNotEmpty)
	}()
	time.Sleep(time.Nanosecond) // let go routine run
	select {
	case <-queueNotEmpty:
		t.Fatalf("Wait should not return until something is in queue")
	default:
	}
	testEntry := []byte("This is a test")
	if err := q.Enqueue(bytes.NewReader(testEntry)); err != nil {
		t.Fatalf("error adding to queue: %s", err)
	}
	select {
	case <-queueNotEmpty:
	case <-time.After(time.Second):
		t.Fatalf("Wait should return when something is in queue")
	}

	const entryFormat = "Queue entry %d for testing"
	for i := 0; i < 10; i++ {
		if err := q.Enqueue(bytes.NewReader([]byte(fmt.Sprintf(entryFormat, i)))); err != nil {
			t.Fatalf("error adding entry %d: %s", i, err)
		}
	}

	// Remove the initial "This is a test" entry
	if entry, err := q.Peek(); err != nil {
		t.Fatalf("error peeking test entry: %s", err)
	} else if err = entry.Remove(); err != nil {
		t.Fatalf("error dequeue test entry: %s", err)
	}

	for i := 0; i < 10; i++ {
		entry, err := q.Peek()
		if err != nil {
			t.Fatalf("error peeking entry %d: %s", i, err)
		}
		r, err := entry.Reader()
		if err != nil {
			t.Fatalf("error getting reader for entry %d: %s", i, err)
		}
		buf := &bytes.Buffer{}
		if _, err = buf.ReadFrom(r); err != nil {
			t.Fatalf("error reading entry %d: %s", i, err)
		}
		if err = entry.Remove(); err != nil {
			t.Fatalf("error removing queue entry %d: %s", i, err)
		}

		expected := []byte(fmt.Sprintf(entryFormat, i))
		if bytes.Compare(expected, buf.Bytes()) != 0 {
			t.Fatalf("expected \"%s\", got \"%s\"", expected, buf.Bytes())
		}
	}

	if _, err := q.Peek(); !errors.Is(err, queue.ErrEmpty) {
		t.Errorf("expected ErrEmpty, got %s", err)
	}
}

// TestLeaveAndQuarantine checks that the Leave and Quarantine functions do what is expected
func TestLeaveAndQuarantine(t *testing.T) {
	q := New()

	if _, err := q.Peek(); !errors.Is(err, queue.ErrEmpty) {
		t.Fatalf("expected ErrEmpty, got %s", err)
	}

	testEntry := []byte("This is a test")
	if err := q.Enqueue(bytes.NewReader(testEntry)); err != nil {
		t.Fatalf("error adding to queue: %s", err)
	}

	// Peek and leave the entry in the queue
	if entry, err := q.Peek(); err != nil {
		t.Fatalf("error peeking test entry: %s", err)
	} else if err = entry.Leave(); err != nil {
		t.Fatalf("error leaving test entry: %s", err)
	}

	// Quarantine entry
	if entry, err := q.Peek(); err != nil {
		t.Fatalf("error peeking test entry: %s", err)
	} else if err = entry.Quarantine(); err != nil {
		t.Fatalf("error erroring test entry: %s", err)
	}

	// As the file has been moved to quarantine it should not be part of the queue
	if _, err := q.Peek(); !errors.Is(err, queue.ErrEmpty) {
		t.Errorf("expected ErrEmpty, got %s", err)
	}
}
