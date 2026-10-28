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

package file_test

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/eclipse/paho.golang/autopaho/queue"
	"github.com/eclipse/paho.golang/autopaho/queue/basicfile"
	"github.com/eclipse/paho.golang/autopaho/queue/file"
)

var queueImplementations = []struct {
	name string
	new  func(string) (queue.Queue, error)
}{
	{"basicfile", func(path string) (queue.Queue, error) {
		return basicfile.New(path, "bench-", ".que")
	}},
	{"file", func(path string) (queue.Queue, error) {
		return file.New(path, "bench-", ".que")
	}},
	{"file_durable", func(path string) (queue.Queue, error) {
		return file.New(path, "bench-", ".que", file.WithDurable())
	}},
}

// BenchmarkQueueEnqueue measures only Enqueue, with 0..127 existing entries.
// Draining each batch outside the timer bounds disk usage independently of b.N.
func BenchmarkQueueEnqueue(b *testing.B) {
	const batchSize = 128
	for _, impl := range queueImplementations {
		for _, size := range []int{256, 4096} {
			b.Run(fmt.Sprintf("%s/bytes=%d", impl.name, size), func(b *testing.B) {
				q, err := impl.new(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				payload := bytes.Repeat([]byte("x"), size)
				reader := bytes.NewReader(payload)
				b.ReportAllocs()
				b.SetBytes(int64(size))
				pending := 0
				for b.Loop() {
					if pending == batchSize {
						b.StopTimer()
						for range pending {
							entry, err := q.Peek()
							if err != nil {
								b.Fatal(err)
							}
							if err := entry.Remove(); err != nil {
								b.Fatal(err)
							}
						}
						pending = 0
						b.StartTimer()
					}
					reader.Reset(payload)
					if err := q.Enqueue(reader); err != nil {
						b.Fatal(err)
					}
					pending++
				}
			})
		}
	}
}

// BenchmarkQueueRoundTrip measures Enqueue, Peek, reading the entire entry, and
// Remove. The backlog stays fixed, so directory scan costs do not grow with b.N.
func BenchmarkQueueRoundTrip(b *testing.B) {
	for _, impl := range queueImplementations {
		for _, size := range []int{256, 4096} {
			for _, backlog := range []int{0, 1024} {
				b.Run(fmt.Sprintf("%s/bytes=%d/backlog=%d", impl.name, size, backlog), func(b *testing.B) {
					q, err := impl.new(b.TempDir())
					if err != nil {
						b.Fatal(err)
					}
					payload := bytes.Repeat([]byte("x"), size)
					reader := bytes.NewReader(payload)
					for range backlog {
						reader.Reset(payload)
						if err := q.Enqueue(reader); err != nil {
							b.Fatal(err)
						}
					}
					b.ReportAllocs()
					b.SetBytes(int64(size))
					for b.Loop() {
						reader.Reset(payload)
						if err := q.Enqueue(reader); err != nil {
							b.Fatal(err)
						}
						entry, err := q.Peek()
						if err != nil {
							b.Fatal(err)
						}
						r, err := entry.Reader()
						if err != nil {
							_ = entry.Leave()
							b.Fatal(err)
						}
						n, readErr := io.Copy(io.Discard, r)
						removeErr := entry.Remove() // Also closes the reader, including on read failure.
						if readErr != nil || removeErr != nil || n != int64(size) {
							b.Fatalf("read %d bytes, want %d: read=%v, remove=%v", n, size, readErr, removeErr)
						}
					}
				})
			}
		}
	}
}

// BenchmarkQueueReopen includes the constructor's probe, directory scans and,
// for durable mode, initialization syncs. Populating the directory is not timed.
func BenchmarkQueueReopen(b *testing.B) {
	for _, impl := range queueImplementations {
		for _, entries := range []int{0, 1024} {
			b.Run(fmt.Sprintf("%s/entries=%d", impl.name, entries), func(b *testing.B) {
				dir := b.TempDir()
				q, err := impl.new(dir)
				if err != nil {
					b.Fatal(err)
				}
				payload := bytes.Repeat([]byte("x"), 1024)
				reader := bytes.NewReader(payload)
				for range entries {
					reader.Reset(payload)
					if err := q.Enqueue(reader); err != nil {
						b.Fatal(err)
					}
				}
				q = nil // Queues hold no open files; only the new instance is used below.
				b.ReportAllocs()
				for b.Loop() {
					if _, err := impl.new(dir); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
