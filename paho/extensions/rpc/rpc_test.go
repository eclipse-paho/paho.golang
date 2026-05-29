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

package rpc

import (
	"context"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/internal/basictestserver"
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	paholog "github.com/eclipse/paho.golang/paho/log"
	"github.com/stretchr/testify/require"
)

// TestRequestCleansUpCorrelDataOnTimeout is a regression test for issue #313:
// a Request whose response never arrives (timeout/cancel) must not leak its
// correlation-data entry. Previously the entry was only removed when a matching
// response was received, so every timed-out request leaked a map entry + channel.
func TestRequestCleansUpCorrelDataOnTimeout(t *testing.T) {
	ts := basictestserver.New(paholog.NOOPLogger{})
	ts.SetResponse(packets.CONNACK, &packets.Connack{ReasonCode: 0, Properties: &packets.Properties{}})
	ts.SetResponse(packets.SUBACK, &packets.Suback{Reasons: []byte{1}, Properties: &packets.Properties{}})
	ts.SetResponse(packets.PUBACK, &packets.Puback{Properties: &packets.Properties{}})
	go ts.Run()
	defer ts.Stop()

	c := paho.NewClient(paho.ClientConfig{
		Conn:     ts.ClientConn(),
		ClientID: "testRPC",
	})
	_, err := c.Connect(context.Background(), &paho.Connect{
		ClientID:   "testRPC",
		KeepAlive:  30,
		CleanStart: true,
	})
	require.NoError(t, err)

	h, err := NewHandler(context.Background(), c)
	require.NoError(t, err)

	// The server auto-acks the PUBLISH but never sends a response on the
	// response topic, so the Request must time out.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = h.Request(ctx, &paho.Publish{Topic: "test/request", QoS: 1, Payload: []byte("ping")})
	require.Error(t, err, "Request should fail when no response arrives")

	h.Lock()
	n := len(h.correlData)
	h.Unlock()
	require.Equal(t, 0, n, "correlData entry must be removed after a timed-out request (issue #313)")
}
