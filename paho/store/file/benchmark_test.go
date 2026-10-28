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

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho/store/basicfile"
	"github.com/eclipse/paho.golang/paho/store/file"
)

type benchmarkStore interface {
	Put(uint16, byte, io.WriterTo) error
	Get(uint16) (io.ReadCloser, error)
	Delete(uint16) error
	List() ([]uint16, error)
	Reset() error
}

var storeImplementations = []struct {
	name string
	new  func(string) (benchmarkStore, error)
}{
	{"basicfile", func(path string) (benchmarkStore, error) {
		return basicfile.New(path, "bench-", ".pkt")
	}},
	{"file", func(path string) (benchmarkStore, error) {
		return file.New(path, "bench-", ".pkt")
	}},
	{"file_durable", func(path string) (benchmarkStore, error) {
		return file.New(path, "bench-", ".pkt", file.WithDurable())
	}},
}

func benchmarkPublish(size int) *packets.Publish {
	return &packets.Publish{
		PacketID: 1,
		QoS:      1,
		Topic:    "benchmark/store",
		Payload:  bytes.Repeat([]byte("x"), size),
	}
}

// BenchmarkStorePut measures inserting PUBLISH packets, including serialization.
// Reset runs outside the timer every 128 packets to bound disk usage and avoid
// packet identifier wraparound or accidentally measuring replacements.
func BenchmarkStorePut(b *testing.B) {
	const batchSize = 128
	for _, impl := range storeImplementations {
		for _, size := range []int{256, 4096} {
			b.Run(fmt.Sprintf("%s/bytes=%d", impl.name, size), func(b *testing.B) {
				s, err := impl.new(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				publish := benchmarkPublish(size)
				b.ReportAllocs()
				b.SetBytes(int64(size))
				pending := 0
				for b.Loop() {
					if pending == batchSize {
						b.StopTimer()
						if err := s.Reset(); err != nil {
							b.Fatal(err)
						}
						pending = 0
						b.StartTimer()
					}
					publish.PacketID = uint16(pending + 1)
					if err := s.Put(publish.PacketID, packets.PUBLISH, publish); err != nil {
						b.Fatal(err)
					}
					pending++
				}
			})
		}
	}
}

// BenchmarkStoreRoundTrip measures Put, Get, reading the whole packet, Close and
// Delete. The store is empty at the beginning and end of each iteration.
func BenchmarkStoreRoundTrip(b *testing.B) {
	for _, impl := range storeImplementations {
		for _, size := range []int{256, 4096} {
			b.Run(fmt.Sprintf("%s/bytes=%d", impl.name, size), func(b *testing.B) {
				s, err := impl.new(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				publish := benchmarkPublish(size)
				encodedSize, err := publish.WriteTo(io.Discard)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for b.Loop() {
					if err := s.Put(publish.PacketID, packets.PUBLISH, publish); err != nil {
						b.Fatal(err)
					}
					r, err := s.Get(publish.PacketID)
					if err != nil {
						b.Fatal(err)
					}
					n, readErr := io.Copy(io.Discard, r)
					closeErr := r.Close()
					if readErr != nil || closeErr != nil || n != encodedSize {
						b.Fatalf("read %d bytes, want %d: read=%v, close=%v", n, encodedSize, readErr, closeErr)
					}
					if err := s.Delete(publish.PacketID); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkStoreQoS2 measures the storage mutations for an outbound QoS 2
// exchange: Put(PUBLISH), replace with PUBREL, then Delete. It does not include
// network I/O or session-manager work.
func BenchmarkStoreQoS2(b *testing.B) {
	for _, impl := range storeImplementations {
		for _, size := range []int{256, 4096} {
			b.Run(fmt.Sprintf("%s/bytes=%d", impl.name, size), func(b *testing.B) {
				s, err := impl.new(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				publish := benchmarkPublish(size)
				publish.QoS = 2
				pubrel := &packets.Pubrel{PacketID: publish.PacketID}
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for b.Loop() {
					if err := s.Put(publish.PacketID, packets.PUBLISH, publish); err != nil {
						b.Fatal(err)
					}
					if err := s.Put(pubrel.PacketID, packets.PUBREL, pubrel); err != nil {
						b.Fatal(err)
					}
					if err := s.Delete(pubrel.PacketID); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func populateBenchmarkStore(b *testing.B, s benchmarkStore, entries int) {
	b.Helper()
	publish := benchmarkPublish(1024)
	for i := 1; i <= entries; i++ {
		publish.PacketID = uint16(i)
		if err := s.Put(publish.PacketID, packets.PUBLISH, publish); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStoreList measures inventory/replay ordering without packet reads.
func BenchmarkStoreList(b *testing.B) {
	for _, impl := range storeImplementations {
		for _, entries := range []int{1, 1024} {
			b.Run(fmt.Sprintf("%s/entries=%d", impl.name, entries), func(b *testing.B) {
				s, err := impl.new(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				populateBenchmarkStore(b, s, entries)
				b.ReportAllocs()
				for b.Loop() {
					ids, err := s.List()
					if err != nil || len(ids) != entries {
						b.Fatalf("List returned %d IDs, want %d: %v", len(ids), entries, err)
					}
				}
			})
		}
	}
}

// BenchmarkStoreReopen includes the constructor's probe, recovery scans and,
// for durable mode, initialization syncs. Populating the directory is not timed.
func BenchmarkStoreReopen(b *testing.B) {
	for _, impl := range storeImplementations {
		for _, entries := range []int{0, 1024} {
			b.Run(fmt.Sprintf("%s/entries=%d", impl.name, entries), func(b *testing.B) {
				dir := b.TempDir()
				s, err := impl.new(dir)
				if err != nil {
					b.Fatal(err)
				}
				populateBenchmarkStore(b, s, entries)
				s = nil // Stores hold no open files; only the new instance is used below.
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
