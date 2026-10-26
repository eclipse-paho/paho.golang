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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/packets"
)

func checkIDs(t *testing.T, s *Store, want []uint16) {
	t.Helper()
	got, err := s.List()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("List=%v, want %v, err=%v", got, want, err)
	}
}

func checkData(t *testing.T, s *Store, id uint16, want string) {
	t.Helper()
	r, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if closeErr := r.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || string(got) != want {
		t.Fatalf("Get(%d)=%q, want %q, err=%v", id, got, want, err)
	}
}

// TestPacketIdentifierFilenamePadding checks five-digit packet IDs across digit
// boundaries and verifies that the resulting filenames can be recovered on reopen.
func TestPacketIdentifierFilenamePadding(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "p", ".pkt")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		id  uint16
		key string
	}{
		{0, "00000"}, {1, "00001"}, {9, "00009"}, {10, "00010"},
		{99, "00099"}, {100, "00100"}, {999, "00999"}, {1000, "01000"},
		{9999, "09999"}, {10000, "10000"}, {65535, "65535"},
	}
	for i, tc := range tests {
		if err := s.Put(tc.id, packets.PUBLISH, strings.NewReader(tc.key)); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("ps%020d-%s.pkt", i+1, tc.key)
		if got := s.packets[tc.id].Name; got != want {
			t.Fatalf("packet %d filename=%q, want %q", tc.id, got, want)
		}
	}
	s, err = New(dir, "p", ".pkt")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range tests {
		checkData(t, s, tc.id, tc.key)
	}
}

// TestSequenceOrderAndReplacement checks that replacement and packet-ID reuse move
// an entry to the end, with the latest packet and order surviving timestamp ties,
// backwards timestamps, and reopening.
func TestSequenceOrderAndReplacement(t *testing.T) {
	for _, ext := range []string{".pkt", ""} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			s, err := New(dir, "p", ext)
			if err != nil {
				t.Fatal(err)
			}
			ids := []uint16{65535, 10, 2, 1, 42}
			for _, id := range ids {
				if err := s.Put(id, packets.PUBLISH, strings.NewReader(fmt.Sprint(id))); err != nil {
					t.Fatal(err)
				}
			}
			// Replacing the first packet moves it last, as in the memory store.
			if err := s.Put(65535, packets.PUBREL, &packets.Pubrel{PacketID: 65535}); err != nil {
				t.Fatal(err)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != len(ids) {
				t.Fatalf("replacement left extra files: %d, %v", len(files), err)
			}
			for _, e := range files {
				ts := time.Unix(1_600_000_100, 0)
				if err := os.Chtimes(filepath.Join(dir, e.Name()), ts, ts); err != nil {
					t.Fatal(err)
				}
			}
			s, err = New(dir, "p", ext)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, s, []uint16{10, 2, 1, 42, 65535})
			r, err := s.Get(65535)
			if err != nil {
				t.Fatal(err)
			}
			p, err := packets.ReadPacket(r)
			if closeErr := r.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil || p.Type != packets.PUBREL || p.PacketID() != 65535 {
				t.Fatalf("QoS2 replacement not recovered: %v, %v", p, err)
			}
			if err := s.Delete(10); err != nil {
				t.Fatal(err)
			}
			if err := s.Put(10, packets.PUBLISH, strings.NewReader("reused")); err != nil {
				t.Fatal(err)
			}
			ts := time.Unix(1_600_000_000, 0)
			if err := os.Chtimes(filepath.Join(dir, s.packets[10].Name), ts, ts); err != nil {
				t.Fatal(err)
			}
			s, err = New(dir, "p", ext)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, s, []uint16{2, 1, 42, 65535, 10})
			checkData(t, s, 10, "reused")
		})
	}
}

// TestLegacyAndInterruptedReplacement checks recovery with legacy entries and
// multiple versions of one packet, as left by interrupted replacement cleanup.
// Deleting, quarantining, or resetting must not resurrect an obsolete version on reopen.
func TestLegacyAndInterruptedReplacement(t *testing.T) {
	for _, operation := range []string{"delete", "quarantine", "reset"} {
		t.Run(operation, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{
				"p9.pkt": "9", "p10.pkt": "10", "p2.pkt": "2", "p7.pkt": "legacy seven",
				sequenceFilename("p", ".pkt", 1, "00007"): "old seven",
				sequenceFilename("p", ".pkt", 2, "00007"): "latest seven",
				".p-incomplete.tmp":                       "partial", "p5-123.pkt.tmp": "old partial",
			}
			for name, data := range files {
				fn := filepath.Join(dir, name)
				if err := os.WriteFile(fn, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				ts := time.Unix(1_600_000_100, 0)
				if name == "p9.pkt" {
					ts = time.Unix(1_600_000_050, 0)
				}
				if err := os.Chtimes(fn, ts, ts); err != nil {
					t.Fatal(err)
				}
			}
			s, err := New(dir, "p", ".pkt")
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, s, []uint16{9, 10, 2, 7})
			checkData(t, s, 7, "latest seven")
			// Replace a legacy ID and check that it joins the sequence ordering.
			if err := s.Put(10, packets.PUBLISH, strings.NewReader("new ten")); err != nil {
				t.Fatal(err)
			}
			checkIDs(t, s, []uint16{9, 2, 7, 10})
			switch operation {
			case "delete":
				err = s.Delete(7)
			case "quarantine":
				err = s.Quarantine(7)
			case "reset":
				err = s.Reset()
			}
			if err != nil {
				t.Fatal(err)
			}
			s, err = New(dir, "p", ".pkt")
			if err != nil {
				t.Fatal(err)
			}
			if r, err := s.Get(7); err == nil {
				r.Close()
				t.Fatal("obsolete packet resurrected")
			}
			if operation == "reset" {
				checkIDs(t, s, []uint16{})
			} else {
				checkIDs(t, s, []uint16{9, 2, 10})
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) WriteTo(w io.Writer) (int64, error) {
	n, _ := io.WriteString(w, "partial")
	return int64(n), io.ErrUnexpectedEOF
}

// TestFailedReplacementPreservesPacket checks that encoding failure and sequence
// exhaustion leave the previously stored packet intact, including after reopening.
func TestFailedReplacementPreservesPacket(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(1, packets.PUBLISH, strings.NewReader("original")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(1, packets.PUBREL, failingWriter{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want encoding error, got %v", err)
	}
	checkData(t, s, 1, "original")
	s, err = New(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	checkData(t, s, 1, "original")
	s.lastSequence = math.MaxUint64 - 1
	if err := s.Put(2, packets.PUBLISH, strings.NewReader("last")); err != nil {
		t.Fatal(err)
	}
	s, err = New(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(1, packets.PUBLISH, strings.NewReader("overflow")); err == nil {
		t.Fatal("sequence overflow accepted")
	}
	checkData(t, s, 1, "original")
	checkIDs(t, s, []uint16{1, 2})
}

// TestObsoleteCleanupFailurePreservesNewest checks that Delete, Quarantine, and
// Reset preserve the latest packet when obsolete versions cannot be removed.
func TestObsoleteCleanupFailurePreservesNewest(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "p", ".pkt")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(1, packets.PUBLISH, strings.NewReader("newest")); err != nil {
		t.Fatal(err)
	}
	// A nonempty directory provides a portable Remove failure, even when run as root.
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "child"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.packets[1].obsolete = []string{"blocked"}
	for _, remove := range []func() error{func() error { return s.Delete(1) }, func() error { return s.Quarantine(1) }, s.Reset} {
		if err := remove(); err == nil {
			t.Fatal("cleanup failure ignored")
		}
		checkData(t, s, 1, "newest")
	}
}

// TestConcurrentStoreWrites checks that concurrent puts retain every packet and
// recover the same sequence order and contents after reopening.
func TestConcurrentStoreWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "p", ".pkt")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for id := uint16(1); id <= 20; id++ {
		wg.Go(func() {
			if err := s.Put(id, packets.PUBLISH, strings.NewReader(fmt.Sprint(id))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	before, err := s.List()
	if err != nil || len(before) != 20 {
		t.Fatalf("concurrent writes lost IDs: %v, %v", before, err)
	}
	s, err = New(dir, "p", ".pkt")
	if err != nil {
		t.Fatal(err)
	}
	checkIDs(t, s, before)
	for _, id := range before {
		checkData(t, s, id, fmt.Sprint(id))
	}
}
