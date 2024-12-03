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

package file

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/canonical/mqtt.golang/packets"
)

// TestFileStore basic tests of the file store
func TestFileStore(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir(), "foo", ".ext")
	if err != nil {
		t.Fatal(err)
	}

	ids := []uint16{65535, 2, 10, 32300, 5890}
	for _, id := range ids {
		pcp := packets.NewControlPacket(packets.PUBLISH)
		pcp.Content.(*packets.Publish).PacketID = id
		pcp.Content.(*packets.Publish).QoS = 1 // ID will only be written for QOS1+
		pcp.Content.(*packets.Publish).Payload = []byte(fmt.Sprintf("%d", id))

		if err := s.Put(id, packets.PUBLISH, pcp); err != nil {
			t.Fatalf("failed to put: %s", err)
		}
	}

	if err := s.Delete(ids[2]); err != nil {
		t.Fatalf("failed to delete: %s", err)
	}

	if _, err := s.Get(8); err == nil {
		t.Fatal("getting missing item should fail")
	}
	if _, err := s.Get(ids[2]); err == nil {
		t.Fatal("getting deleted item should fail")
	}
	ids = append(ids[:2], ids[3:]...) // keep our record in sync following delete

	if rp, err := s.Get(32300); err != nil {
		t.Fatalf("failed to get: %s", err)
	} else {
		p, err := packets.ReadPacket(rp, 5)
		if err != nil {
			t.Fatalf("error decoding packet: %s", err)
		}
		if err := rp.Close(); err != nil {
			t.Fatalf("error closing packet: %s", err)
		}

		if p.PacketID() != 32300 {
			t.Fatalf("unexpected packet id returned: %d", p.PacketID())
		}
		payload := p.Content.(*packets.Publish).Payload
		if bytes.Compare(payload, []byte(fmt.Sprintf("%d", 32300))) != 0 {
			t.Fatalf("unexpected payload returned: %s", payload)
		}
	}
	rids, err := s.List()
	if err != nil {
		t.Fatalf("failed to list: %s", err)
	}
	if len(rids) != len(ids) {
		t.Fatalf("List returned %d elements, expected %d", len(rids), len(ids))
	}
	for i, v := range rids {
		if v != ids[i] {
			t.Fatalf("List returned %v, expected %v", rids, ids)
		}
	}

	if err = s.Reset(); err != nil {
		t.Fatalf("reset failed: %s", err)
	}
	rids, err = s.List()
	if err != nil {
		t.Fatalf("failed to list: %s", err)
	}
	if len(rids) != 0 {
		t.Fatalf("reset did not clear store: %d", len(rids))
	}
}

// TestFileStoreNaming checks that file naming is as expected (and is the only thing created)
func TestFileStoreNaming(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := New(dir, "BlahXX", ".txt")
	if err != nil {
		t.Fatal(err)
	}

	msg := bytes.NewReader([]byte("random file contents"))

	if err := s.Put(1, packets.PUBLISH, msg); err != nil {
		t.Fatalf("failed to put: %s", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read dir: %s", err)
	}
	if len(entries) != 1 {
		t.Fatalf("should be one file; got %#v", entries)
	}
	if entries[0].Name() != "BlahXX1.txt" {
		t.Fatalf("filename not as expected; got %s", entries[0].Name())
	}

	if err := s.Quarantine(1); err != nil {
		t.Fatalf("failed to Quarantine: %s", err)
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read dir: %s", err)
	}
	if len(entries) != 1 {
		t.Fatalf("should be one file; got %#v", entries)
	}
	fn := entries[0].Name()
	if !strings.HasPrefix(fn, "BlahXX1") {
		t.Fatalf("quarantine filename prefix not as expected; got %s", entries[0].Name())
	}

	if !strings.HasSuffix(fn, corruptExtension) {
		t.Fatalf("quarantine filename suffix not as expected; got %s", entries[0].Name())
	}
}

// TestFileStoreBig creates a fully populated Store and checks things work
// Adding messages would make the structure bigger but should have no impact on the struct functions.
// Commenting this out as it's very slow!
// func TestFileStoreBig(t *testing.T) {
// 	t.Parallel()
// 	if testing.Short() {
// 		t.Skip("skipping TestFileStoreBig as its slow")
// 	}
//
// 	s, err := New(t.TempDir(), "foo", ".ext")
// 	if err != nil {
// 		t.Fatal(err)
// 	}
//
// 	for id := uint16(1); id != 0; id++ {
// 		pcp := packets.NewControlPacket(packets.PUBLISH)
// 		pcp.Content.(*packets.Publish).PacketID = id
// 		pcp.Content.(*packets.Publish).QoS = 1 // ID will only be written for QOS1+
// 		pcp.Content.(*packets.Publish).Payload = []byte(fmt.Sprintf("%d", id))
//
// 		if err := s.Put(id, packets.PUBLISH, pcp); err != nil {
// 			t.Fatalf("failed to put: %s", err)
// 		}
// 	}
//
// 	for id := uint16(1); id != 0; id++ {
// 		rp, err := s.Get(id)
// 		if err != nil {
// 			t.Fatal("getting missing item should fail")
// 		}
// 		pcp, err := packets.ReadPacket(rp)
// 		if err != nil {
// 			t.Fatalf("error decoding packet: %s", err)
// 		}
// 		if err := rp.Close(); err != nil {
// 			t.Fatalf("error closing packet: %s", err)
// 		}
// 		sId := pcp.Content.(*packets.Publish).PacketID
// 		if id != sId {
// 			t.Fatalf("expected %d, gor %d", id, sId)
// 		}
// 		sPayload := pcp.Content.(*packets.Publish).Payload
// 		expPayload := []byte(fmt.Sprintf("%d", id))
// 		if bytes.Compare(expPayload, sPayload) != 0 {
// 			t.Fatalf("expected %v, got %v", expPayload, sPayload)
// 		}
// 	}
// 	for id := uint16(1); id != 0; id++ {
// 		if err := s.Delete(id); err != nil {
// 			t.Fatalf("delete failed: %s", err)
// 		}
// 	}
// 	rids, err := s.List()
// 	if err != nil {
// 		t.Fatalf("failed to list: %s", err)
// 	}
// 	if len(rids) != 0 {
// 		t.Fatalf("everything should have been deleted")
// 	}
// }
