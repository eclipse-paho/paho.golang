/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
 *
 * This program and the accompanying materials are made available under the
 * Eclipse Public License v2.0 (https://www.eclipse.org/legal/epl-2.0/) and
 * Eclipse Distribution License v1.0 (http://www.eclipse.org/org/documents/edl-v10.php).
 * AI Disclosure: This file was largely generated with OpenAI GPT-6 via Codex.
 * AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-6
 */

package file

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho/queue"
)

func take(t *testing.T, q *Queue, want string) {
	t.Helper()
	e, err := q.Peek()
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Reader()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(); err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSequenceOrderAfterRestart(t *testing.T) {
	for _, ext := range []string{".que", ""} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			q, err := New(dir, "queue-", ext)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 30; i++ {
				if err := q.Enqueue(strings.NewReader(fmt.Sprint(i))); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				ts := time.Unix(1_600_000_100, 0) // Coarser than FAT: every timestamp is identical.
				if err := os.Chtimes(filepath.Join(dir, e.Name()), ts, ts); err != nil {
					t.Fatal(err)
				}
			}
			q, err = New(dir, "queue-", ext)
			if err != nil {
				t.Fatal(err)
			}
			if err := q.Enqueue(strings.NewReader("30")); err != nil {
				t.Fatal(err)
			}
			// Simulate a backwards clock for the new entry.
			ts := time.Unix(1_600_000_000, 0)
			if err := os.Chtimes(filepath.Join(dir, sequenceFilename("queue-", ext, 31, "")), ts, ts); err != nil {
				t.Fatal(err)
			}
			for i := 0; i <= 30; i++ {
				take(t, q, fmt.Sprint(i))
			}
			if _, err := q.Peek(); !errors.Is(err, queue.ErrEmpty) {
				t.Fatalf("expected empty: %v", err)
			}
			if err := q.Enqueue(strings.NewReader("after drain")); err != nil {
				t.Fatal(err)
			}
			if q.lastSequence != 32 {
				t.Fatalf("sequence reset after draining: %d", q.lastSequence)
			}
			take(t, q, "after drain")
		})
	}
}

func TestLegacyQueueWithSequences(t *testing.T) {
	dir := t.TempDir()
	for _, suffix := range []string{"9", "10", "2"} {
		name := filepath.Join(dir, "queue-"+suffix+".que")
		if err := os.WriteFile(name, []byte(suffix), 0600); err != nil {
			t.Fatal(err)
		}
		ts := time.Unix(1_600_000_100, 0)
		if suffix == "9" {
			ts = time.Unix(1_600_000_050, 0)
		}
		if err := os.Chtimes(name, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	q, err := New(dir, "queue-", ".que")
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	take(t, q, "9")
	q, err = New(dir, "queue-", ".que")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"10", "2", "new"} {
		take(t, q, want)
	}
}

type brokenReader struct{}

func (brokenReader) Read(p []byte) (int, error) {
	return copy(p, "partial"), io.ErrUnexpectedEOF
}

func TestFailedEnqueueAndExhaustion(t *testing.T) {
	dir := t.TempDir()
	q, err := New(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	wait := q.Wait()
	if err := q.Enqueue(brokenReader{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want read error, got %v", err)
	}
	select {
	case <-wait:
		t.Fatal("failed enqueue woke waiting consumer")
	default:
	}
	q.lastSequence = math.MaxUint64 - 1
	if err := q.Enqueue(strings.NewReader("last")); err != nil {
		t.Fatal(err)
	}
	e, err := q.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Quarantine(); err != nil {
		t.Fatal(err)
	}
	q, err = New(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(strings.NewReader("overflow")); err == nil {
		t.Fatal("sequence overflow was accepted")
	}
	if _, err := q.Peek(); !errors.Is(err, queue.ErrEmpty) {
		t.Fatalf("failed/quarantined entry became visible: %v", err)
	}
}

func TestConcurrentEnqueues(t *testing.T) {
	dir := t.TempDir()
	q, err := New(dir, "", ".que")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Go(func() {
			if err := q.Enqueue(strings.NewReader(fmt.Sprint(i))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	entries, last, err := readEntries(dir, "", ".que")
	if err != nil || len(entries) != 40 || last != 40 {
		t.Fatalf("concurrent writes lost entries: count=%d, last=%d, err=%v", len(entries), last, err)
	}
	seen := make(map[string]bool)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name))
		if err != nil || seen[string(b)] {
			t.Fatalf("bad/duplicate payload %q: %v", b, err)
		}
		seen[string(b)] = true
	}
}
