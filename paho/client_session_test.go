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

package paho

import (
	"context"
	"fmt"
	"testing"

	"github.com/eclipse/paho.golang/internal/basictestserver"
	"github.com/eclipse/paho.golang/packets"
	paholog "github.com/eclipse/paho.golang/paho/log"
	"github.com/eclipse/paho.golang/paho/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prematurePubrecSession violates the session contract by completing a publish with a successful PUBREC.
type prematurePubrecSession struct {
	session.SessionManager
	reasonCode byte
}

func (s *prematurePubrecSession) AddToSession(_ context.Context, packet session.Packet) (<-chan packets.ControlPacket, error) {
	packet.SetIdentifier(1)
	response := packets.NewControlPacket(packets.PUBREC)
	pubrec := response.Content.(*packets.Pubrec)
	pubrec.PacketID = 1
	pubrec.ReasonCode = s.reasonCode
	pubrec.Properties.ReasonString = "broker diagnostic"
	ch := make(chan packets.ControlPacket, 1)
	ch <- *response
	close(ch)
	return ch, nil
}

func TestPublishRejectsPrematurePubrec(t *testing.T) {
	for _, reasonCode := range []byte{packets.PubrecSuccess, packets.PubrecNoMatchingSubscribers} {
		t.Run(fmt.Sprintf("0x%02X", reasonCode), func(t *testing.T) {
			ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
			go ts.Run()
			defer ts.Stop()
			defer ts.ClientConn().Close()

			c := NewClient(ClientConfig{
				Conn:    ts.ClientConn(),
				Session: &prematurePubrecSession{reasonCode: reasonCode},
			})
			pr, err := c.Publish(t.Context(), &Publish{QoS: 2, Topic: "test/premature-pubrec"})
			require.ErrorContains(t, err, fmt.Sprintf("QoS 2 publish ended at PUBREC (reason code: 0x%02X):", reasonCode))
			require.NotNil(t, pr)
			assert.Equal(t, reasonCode, pr.ReasonCode)
			require.NotNil(t, pr.Properties)
			assert.Equal(t, "broker diagnostic", pr.Properties.ReasonString)
		})
	}
}
