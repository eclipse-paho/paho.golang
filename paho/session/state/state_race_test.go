/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
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
	"context"
	"io"
	"sync"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho/session"
)

// TestAddToSessionWhileReconnecting checks that publishing while the connection
// is being re-established does not race the send quota being replaced.
//
// ConAckReceived replaces the quota for the new connection, while AddToSession
// acquires from it. AddToSession cannot hold the mutex over the acquire (that
// call blocks by design), so it must take its own reference to the quota while
// it does hold it. Reading the field afterwards is a data race with any
// reconnect, which is a normal thing to happen under an automatic reconnect
// loop while the application keeps publishing.
//
// Run with -race; without it this passes whether the bug is present or not.
func TestAddToSessionWhileReconnecting(t *testing.T) {
	t.Parallel()

	s := NewInMemory()
	defer s.Close()

	connack := &packets.Connack{ReasonCode: 0, SessionPresent: false}
	connect := &packets.Connect{}

	if err := s.ConAckReceived(io.Discard, connect, connack); err != nil {
		t.Fatalf("first ConAckReceived: %s", err)
	}

	const rounds = 200
	var wg sync.WaitGroup

	// One goroutine reconnects, as an automatic reconnect loop would.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range rounds {
			if err := s.ConnectionLost(nil); err != nil {
				t.Errorf("ConnectionLost: %s", err)
				return
			}
			if err := s.ConAckReceived(io.Discard, connect, connack); err != nil {
				t.Errorf("ConAckReceived: %s", err)
				return
			}
		}
	}()

	// Another publishes throughout, as an application would. Whether any given
	// attempt succeeds depends on the timing; that a successful one does not
	// race the reconnect is the point.
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp := make(chan packets.ControlPacket, 1)
		for range rounds {
			pub := packets.NewControlPacket(packets.PUBLISH)
			pub.Content.(*packets.Publish).QoS = 1
			err := s.AddToSession(context.Background(), pub.Content.(*packets.Publish), resp)
			if err != nil && err != session.ErrNoConnection {
				// Anything else is a real failure; a lost connection is the
				// expected outcome of racing a disconnect.
				t.Errorf("AddToSession: %s", err)
				return
			}
		}
	}()

	wg.Wait()
}
