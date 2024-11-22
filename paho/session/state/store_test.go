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

package state

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/canonical/mqtt.golang/internal/testserver"
	"github.com/canonical/mqtt.golang/packets"
	paholog "github.com/canonical/mqtt.golang/paho/log"
	"github.com/canonical/mqtt.golang/paho/store/memory"
)

// TestLoadExistingSession confirms that a session loaded from disk updates the state as expected
func TestLoadExistingSession(t *testing.T) {
	t.Parallel()

	// Create client store and load a PUBLISH that should be transmitted when CONNACK received
	cs := memory.New()
	pcp := packets.NewControlPacket(packets.PUBLISH)
	cliPacketID := uint16(65534)
	qos := byte(1)
	payload := []byte("This is the payload")
	pcp.Content.(*packets.Publish).PacketID = cliPacketID
	pcp.Content.(*packets.Publish).QoS = qos
	pcp.Content.(*packets.Publish).Payload = payload

	if err := cs.Put(cliPacketID, packets.PUBLISH, pcp); err != nil {
		t.Fatalf("failed to put: %s", err)
	}

	// On the server side, let's assume we have sent a PUBREC for packet 12
	ss := memory.New()
	pcp = packets.NewControlPacket(packets.PUBREC)
	srvPacketID := uint16(12)
	pcp.Content.(*packets.Pubrec).PacketID = srvPacketID

	if err := ss.Put(srvPacketID, packets.PUBREC, pcp); err != nil {
		t.Fatalf("failed to put: %s", err)
	}

	// Use a TestServer to capture packets transmitted
	ts := testserver.New(paholog.NewTestLogger(t, "TestServer:"))
	var received []*packets.ControlPacket
	ts.SetPacketReceivedCallback(func(cp *packets.ControlPacket) error {
		received = append(received, cp)
		return nil
	})
	ts.SetConnectCallback(func(cp *packets.Connect, cap *packets.Connack) {
		cap.SessionPresent = true
	})
	c, tsDone, err := ts.Connect(context.Background())
	if err != nil {
		t.Fatalf("failed to start test server: %s", err)
	}

	receiveMax := uint16(20) // Must be set or nothing will be sent
	ccp := packets.Connect{
		ProtocolName:    "MQTT",
		ProtocolVersion: 5,
		CleanStart:      false,
		Properties:      &packets.Properties{ReceiveMaximum: &receiveMax}}
	if _, err := ccp.WriteTo(c); err != nil {
		t.Fatalf("failed to send CONNECT packet: %s", err)
	}
	cca, err := packets.ReadPacket(c, 5)
	if err != nil {
		t.Fatalf("failed to receive CONNACK packet: %s", err)
	}
	ca, ok := cca.Content.(*packets.Connack)
	if !ok {
		t.Fatalf("expected CONNACK, got %s", cca.PacketType())
	}

	// Create a state and monitor what gets sent
	s := New(cs, ss)

	if err := s.ConAckReceived(c, &ccp, ca); err != nil {
		t.Fatalf("ConAckReceived falied: %s", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close failed %s", err)
	}

	// The test server should have picked up the dropped connection
	select {
	case <-tsDone:
	case <-time.After(time.Second):
		t.Fatal("test server did not shutdown within expected time")
	}

	// We should have received a connect (already tested) and a PUBLISH
	if len(received) != 2 {
		t.Fatalf("expected 2 packets, got %#v", received)
	}
	cp := received[1]
	p, ok := cp.Content.(*packets.Publish)
	if !ok {
		t.Fatalf("expected PUBLISH; got %s", cp.PacketType())
	}
	if !p.Duplicate {
		t.Errorf("resent publish should be a duplicate")
	}
	if p.QoS != qos {
		t.Errorf("resent publish has qos %d, should be %d", p.QoS, qos)
	}
	if bytes.Compare(p.Payload, payload) != 0 {
		t.Errorf("resent publish has payload %s, should be %s", p.Payload, payload)
	}

	// Check the store state
	if len(s.serverPackets) != 1 {
		t.Fatalf("expected one packet in the server side state, got %d", len(s.serverPackets))
	}
	sp, ok := s.serverPackets[srvPacketID]
	if !ok {
		t.Fatalf("packet ID %d not in server side state", srvPacketID)
	}
	if sp != packets.PUBREC {
		t.Fatalf("expected PUBREC in the server side state, got %d", sp)
	}

	if len(s.clientPackets) != 1 {
		t.Fatalf("expected one packet in the client side state, got %d", len(s.clientPackets))
	}
	cg, ok := s.clientPackets[cliPacketID]
	if !ok {
		t.Fatalf("packet ID %d not in client side state", cliPacketID)
	}
	if cg.packetType != packets.PUBLISH {
		t.Fatalf("expected PUBLISH in the client side state, got %d", sp)
	}
}
