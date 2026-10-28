/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
 *
 * This program and the accompanying materials are made available under the
 * Eclipse Public License v2.0 (https://www.eclipse.org/legal/epl-2.0/) and
 * Eclipse Distribution License v1.0 (http://www.eclipse.org/org/documents/edl-v10.php).
 * AI Disclosure: This file was largely generated with OpenAI GPT-5 via Codex.
 * AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-5
 */

package basicfile

import (
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	sequencefile "github.com/eclipse/paho.golang/paho/store/file"
)

// TestSwitchImplementations creates a basicfile store, adds an entry, and checks it can be read. Then creates a file
// queue (the new implementation), writes a packet, and then confirms that both packets can be read.
// Note: store is backwards compatible. basicfile is not forwards compatible.
func TestSwitchImplementations(t *testing.T) {
	// Create a basicfile and add a packet
	dir := t.TempDir()
	s, err := New(dir, "client-", "pkt")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(42, packets.PUBLISH, strings.NewReader("legacy")); err != nil {
		t.Fatal(err)
	}

	// Confirm the packet can be read
	s, err = New(dir, "client-", ".pkt")
	if err != nil {
		t.Fatalf("reopening legacy data: %v", err)
	}
	if ids, err := s.List(); err != nil || !reflect.DeepEqual(ids, []uint16{42}) {
		t.Fatalf("legacy List: %v, %v", ids, err)
	}

	// Now create a new file store (sequence format), write a packet
	upgraded, err := sequencefile.New(dir, "client-", ".pkt")
	if err != nil {
		t.Fatal(err)
	}
	if err := upgraded.Put(7, packets.PUBLISH, strings.NewReader("sequenced")); err != nil {
		t.Fatal(err)
	}

	// Now that a packet using the new format exists attempts to open the folder with basicfile should fail
	if _, err := New(dir, "client-", ".pkt"); err == nil {
		t.Fatal("basicfile accepted mixed legacy and sequence data")
	}

	// Confirm we can list and retrieve both packets using the file store
	if ids, err := upgraded.List(); err != nil || !reflect.DeepEqual(ids, []uint16{42, 7}) {
		t.Fatalf("upgraded List: %v, %v", ids, err)
	}
	for id, want := range map[uint16]string{42: "legacy", 7: "sequenced"} {
		r, err := upgraded.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		if err != nil || string(got) != want {
			t.Fatalf("Get(%d): %q, %v; want %q", id, got, err, want)
		}
		if err := upgraded.Delete(id); err != nil {
			t.Fatal(err)
		}
	}

	// As the files have been removed basicfile should now open the folder again
	s, err = New(dir, "client-", ".pkt")
	if err != nil {
		t.Fatalf("reopening drained namespace: %v", err)
	}
	if ids, err := s.List(); err != nil || len(ids) != 0 {
		t.Fatalf("drained List: %v, %v", ids, err)
	}
}
