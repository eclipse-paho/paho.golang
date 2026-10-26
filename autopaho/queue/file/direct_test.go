/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
 *
 * This program and the accompanying materials are made available under the
 * Eclipse Public License v2.0 (https://www.eclipse.org/legal/epl-2.0/) and
 * Eclipse Distribution License v1.0 (http://www.eclipse.org/org/documents/edl-v10.php).
 * AI Disclosure: This file was generated with OpenAI GPT-5 via Codex.
 * AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-5
 */

package file

import (
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
)

type queueReadFunc func([]byte) (int, error)

func (f queueReadFunc) Read(p []byte) (int, error) { return f(p) }

// TestDefaultEnqueueWritesDirectlyUnderLock checks that a non-durable enqueue
// writes to its final filename while holding the queue mutex, preventing Peek
// from reading partial data, and only notifies consumers after the write succeeds.
func TestDefaultEnqueueWritesDirectlyUnderLock(t *testing.T) {
	dir := t.TempDir()
	q, err := New(dir, "q-", ".que")
	if err != nil {
		t.Fatal(err)
	}
	wait := q.Wait()
	name := sequenceFilename("q-", ".que", 1, "")
	reader := queueReadFunc(func(p []byte) (int, error) {
		// The final file is already present, but Peek cannot acquire the queue
		// mutex and consumers must not be notified until the write completes.
		if q.mu.TryLock() {
			q.mu.Unlock()
			return 0, errors.New("queue mutex not held during write")
		}
		select {
		case <-wait:
			return 0, errors.New("consumer notified before write completed")
		default:
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return 0, err
		}
		if len(files) != 1 || files[0].Name() != name {
			return 0, fmt.Errorf("expected only the final filename during write, got %v", files)
		}
		return copy(p, "complete"), io.EOF
	})
	if err := q.Enqueue(reader); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wait:
	default:
		t.Fatal("consumer not notified after successful write")
	}
	take(t, q, "complete")
}
