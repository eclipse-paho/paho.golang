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

	"github.com/canonical/mqtt.golang/packets"
)

// TestMemoryStore basic tests of the memory store
func TestMemoryStore(t *testing.T) {
	s := New()

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

	if _, err := s.Get(8); !errors.Is(err, ErrNotInStore) {
		t.Fatal("getting missing item should fail")
	}
	if _, err := s.Get(ids[2]); !errors.Is(err, ErrNotInStore) {
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

	s.Reset()
	rids, err = s.List()
	if err != nil {
		t.Fatalf("failed to list: %s", err)
	}
	if len(rids) != 0 {
		t.Fatalf("reset did not clear store: %d", len(rids))
	}

}

// TestMemoryStoreBig creates a fully populated Store and checks things work
// Adding messages would make the structure bigger but should have no impact on the struct functions.
func TestMemoryStoreBig(t *testing.T) {
	s := New()

	for id := uint16(1); id != 0; id++ {
		pcp := packets.NewControlPacket(packets.PUBLISH)
		pcp.Content.(*packets.Publish).PacketID = id
		pcp.Content.(*packets.Publish).QoS = 1 // ID will only be written for QOS1+
		pcp.Content.(*packets.Publish).Payload = []byte(fmt.Sprintf("%d", id))

		if err := s.Put(id, packets.PUBLISH, pcp); err != nil {
			t.Fatalf("failed to put: %s", err)
		}
	}

	for id := uint16(1); id != 0; id++ {
		rp, err := s.Get(id)
		if err != nil {
			t.Fatal("getting missing item should fail")
		}
		pcp, err := packets.ReadPacket(rp, 5)
		if err != nil {
			t.Fatalf("error decoding packet: %s", err)
		}
		sId := pcp.Content.(*packets.Publish).PacketID
		if id != sId {
			t.Fatalf("expected %d, gor %d", id, sId)
		}
		sPayload := pcp.Content.(*packets.Publish).Payload
		expPayload := []byte(fmt.Sprintf("%d", id))
		if bytes.Compare(expPayload, sPayload) != 0 {
			t.Fatalf("expected %v, got %v", expPayload, sPayload)
		}
	}
	for id := uint16(1); id != 0; id++ {
		if err := s.Delete(id); err != nil {
			t.Fatalf("delete failed: %s", err)
		}
	}
	rids, err := s.List()
	if err != nil {
		t.Fatalf("failed to list: %s", err)
	}
	if len(rids) != 0 {
		t.Fatalf("everything should have been deleted")
	}
}
