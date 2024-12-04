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

package paho

import (
	"context"
	"testing"
	"time"

	"github.com/canonical/mqtt.golang/mock/basictestserver"
	"github.com/canonical/mqtt.golang/packets"
	paholog "github.com/canonical/mqtt.golang/paho/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPackedIdNoExhaustion tests interactions between Publish and the session ensuring that IDs are
// released and reused
func TestPackedIdNoExhaustion(t *testing.T) {
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.PUBACK, &packets.Puback{
		ReasonCode: packets.PubackSuccess,
		Properties: &packets.Properties{},
	})
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
	})
	require.NotNil(t, c)

	clientCtx := basicClientInitialisation(c)
	c.publishPackets = make(chan *packets.Publish)
	go c.incoming(clientCtx)
	go c.config.PingHandler.Run(clientCtx, c.config.Conn, 30)
	c.config.Session.ConAckReceived(c.config.Conn, &packets.Connect{}, &packets.Connack{})

	for i := 0; i < 70000; i++ {
		p := &Publish{
			Topic:   "test/1",
			QoS:     1,
			Payload: []byte("test payload"),
		}

		pa, err := c.Publish(context.Background(), p)
		require.Nil(t, err)
		assert.Equal(t, uint8(0), pa.ReasonCode)
	}

	time.Sleep(10 * time.Millisecond)
}

// Note: We no longer test for Packet Id Exhaustion because the way the CONNACK Receive Maximum now works makes
// this impossible (the semaphore will lock on the 65536th request and only unlock when a response is received
// which would also mean an ID is available).
