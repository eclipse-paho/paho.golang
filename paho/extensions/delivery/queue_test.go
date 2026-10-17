/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
 *
 * All rights reserved. This program and the accompanying materials are made
 * available under the Eclipse Public License v2.0 and Eclipse Distribution
 * License v1.0 which accompany this distribution.
 * https://www.eclipse.org/legal/epl-2.0/
 * http://www.eclipse.org/org/documents/edl-v10.php
 *
 * AI Disclosure: This file was largely generated with OpenAI Codex (GPT-6).
 * The AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-6
 */

package delivery_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/autopaho/queue/memory"
	"github.com/eclipse/paho.golang/internal/testserver"
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/extensions/delivery"
	"github.com/eclipse/paho.golang/paho/session/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueuedQoS012 sends serialized queue entries using a fresh tracker, checks the actual wire metadata, and observes
// the different completion points for all QoS levels.
func TestQueuedQoS012(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	q := memory.New()
	original := delivery.New(delivery.Config{})
	for qos := byte(0); qos <= 2; qos++ {
		p := &paho.Publish{QoS: qos, Topic: "delivery/test", Properties: &paho.PublishProperties{
			User: paho.UserProperties{{Key: "public", Value: "keep"}},
		}}
		require.NoError(t, original.TrackWithID(p, fmt.Sprintf("qos%d", qos)))
		var data bytes.Buffer
		_, err := p.Packet().WriteTo(&data)
		require.NoError(t, err)
		require.NoError(t, q.Enqueue(&data))
	}
	results := make(chan delivery.Result, 3)
	problems := make(chan delivery.Problem, 3)
	fresh := delivery.New(delivery.Config{
		OnResult: func(r delivery.Result) { results <- r },
		OnError:  func(p delivery.Problem) { problems <- p },
	})
	wire := make(chan byte, 4)
	broker := testserver.New(nil)
	broker.SetPacketReceivedCallback(func(cp *packets.ControlPacket) error {
		var props *packets.Properties
		switch p := cp.Content.(type) {
		case *packets.Publish:
			props = p.Properties
			assert.Contains(t, props.User, packets.User{Key: "public", Value: "keep"})
		case *packets.Pubrel:
			props = p.Properties
		default:
			return nil
		}
		for _, u := range props.User {
			assert.NotEqual(t, delivery.PropertyKey, u.Key)
		}
		wire <- cp.Type
		return nil
	})
	brokerDone := make(chan chan struct{}, 1)
	s := state.NewInMemory()
	defer s.Close()
	cm, err := autopaho.NewConnection(ctx, autopaho.ClientConfig{
		ServerUrls: []*url.URL{{Scheme: "mqtt", Host: "test.invalid:1883"}},
		Queue:      q, SessionExpiryInterval: 600,
		ClientConfig: paho.ClientConfig{ClientID: "delivery-qos012", Session: s, PublishEvents: fresh.Handler()},
		AttemptConnection: func(ctx context.Context, _ autopaho.ClientConfig, _ *url.URL) (net.Conn, error) {
			conn, done, err := broker.Connect(ctx)
			if err == nil {
				select {
				case brokerDone <- done:
				case <-ctx.Done():
					_ = conn.Close()
					return nil, ctx.Err()
				}
			}
			return conn, err
		},
	})
	require.NoError(t, err)
	defer func() {
		cancel()
		<-cm.Done()
		select {
		case done := <-brokerDone:
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("broker failed to shut down")
			}
		default:
		}
	}()
	seen := make(map[string]bool)
	for range 3 {
		select {
		case r := <-results:
			assert.False(t, seen[r.ID], "one result per queued publication")
			seen[r.ID] = true
			assert.Equal(t, fmt.Sprintf("qos%d", r.QoS), r.ID)
			if r.QoS == 0 {
				assert.Equal(t, delivery.SendAttempted, r.Outcome)
				assert.Zero(t, r.Trigger)
			} else {
				assert.Equal(t, delivery.Acknowledged, r.Outcome)
				if r.QoS == 1 {
					assert.Equal(t, byte(packets.PUBACK), r.Trigger)
				}
				if r.QoS == 2 {
					assert.Equal(t, byte(packets.PUBCOMP), r.Trigger)
				}
			}
		case p := <-problems:
			t.Fatalf("tracking problem: %v", p.Err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// In particular, do not exit on QoS 0's callback: that write may still be pending.
	for range 4 {
		select {
		case <-wire:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	assert.Empty(t, problems)
}
