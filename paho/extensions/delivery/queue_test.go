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
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	filequeue "github.com/eclipse/paho.golang/autopaho/queue/file"
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

// TestQueuedLocalRejection verifies that an accepted queue entry rejected by local validation produces a terminal
// outcome with its original application ID, while the next valid publication still reaches the broker.
func TestQueuedLocalRejection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		publish   paho.Publish
		configure func(*packets.Connack)
	}{
		{
			name:    "maximum QoS",
			publish: paho.Publish{QoS: 2, Topic: "delivery/rejected"},
			configure: func(ca *packets.Connack) {
				maximum := byte(1)
				ca.Properties.MaximumQOS = &maximum
			},
		},
		{
			name:    "retained QoS 0",
			publish: paho.Publish{QoS: 0, Retain: true, Topic: "delivery/rejected"},
			configure: func(ca *packets.Connack) {
				available := byte(0)
				ca.Properties.RetainAvailable = &available
			},
		},
		{
			name:    "missing topic",
			publish: paho.Publish{QoS: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			q := memory.New()
			results := make(chan delivery.Result, 4)
			problems := make(chan delivery.Problem, 4)
			tracker := delivery.New(delivery.Config{
				OnResult: func(r delivery.Result) { results <- r },
				OnError:  func(p delivery.Problem) { problems <- p },
			})
			broker := testserver.New(nil)
			broker.SetConnectCallback(func(_ *packets.Connect, ca *packets.Connack) {
				if tc.configure != nil {
					tc.configure(ca)
				}
			})
			wire := make(chan string, 4)
			broker.SetPacketReceivedCallback(func(cp *packets.ControlPacket) error {
				if p, ok := cp.Content.(*packets.Publish); ok {
					wire <- p.Topic
					for _, u := range p.Properties.User {
						assert.NotEqual(t, delivery.PropertyKey, u.Key)
					}
				}
				return nil
			})
			connect := make(chan struct{})
			brokerDone := make(chan chan struct{}, 1)
			s := state.NewInMemory()
			defer s.Close()
			cm, err := autopaho.NewConnection(ctx, autopaho.ClientConfig{
				ServerUrls: []*url.URL{{Scheme: "mqtt", Host: "test.invalid:1883"}},
				Queue:      q, SessionExpiryInterval: 600,
				ClientConfig: paho.ClientConfig{ClientID: "delivery-local-rejection", Session: s, PublishEvents: tracker.Handler()},
				AttemptConnection: func(ctx context.Context, _ autopaho.ClientConfig, _ *url.URL) (net.Conn, error) {
					select {
					case <-connect:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
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

			// Queue both publications before connecting. IDs are recovered from the queue, using a different tracker.
			original := delivery.New(delivery.Config{})
			rejected := tc.publish
			require.NoError(t, original.TrackWithID(&rejected, "rejected-id"))
			require.NoError(t, cm.PublishViaQueue(ctx, &autopaho.QueuePublish{Publish: &rejected}))
			valid := &paho.Publish{QoS: 1, Topic: "delivery/valid"}
			require.NoError(t, original.TrackWithID(valid, "valid-id"))
			require.NoError(t, cm.PublishViaQueue(ctx, &autopaho.QueuePublish{Publish: valid}))
			close(connect)

			got := make(map[string]delivery.Result)
			for range 2 {
				select {
				case r := <-results:
					_, duplicate := got[r.ID]
					assert.False(t, duplicate, "one terminal outcome per publication")
					got[r.ID] = r
				case p := <-problems:
					t.Fatalf("unexpected tracking problem: %v", p.Err)
				case <-ctx.Done():
					t.Fatalf("missing queued publication outcome; received: %v", got)
				}
			}
			r, ok := got["rejected-id"]
			require.True(t, ok)
			assert.Equal(t, delivery.Unknown, r.Outcome)
			assert.Equal(t, rejected.QoS, r.QoS)
			assert.ErrorIs(t, r.Err, delivery.ErrUnconfirmed)
			assert.ErrorIs(t, r.Err, paho.ErrInvalidArguments)
			assert.Zero(t, r.PacketID)
			assert.Zero(t, r.Trigger)
			assert.Zero(t, r.ReasonCode)
			assert.Empty(t, r.ReasonString)
			r, ok = got["valid-id"]
			require.True(t, ok)
			assert.Equal(t, delivery.Acknowledged, r.Outcome)
			assert.NoError(t, r.Err)
			select {
			case <-q.WaitForEmpty():
			case <-ctx.Done():
				t.Fatal("queue did not drain")
			}
			assert.Equal(t, "delivery/valid", <-wire)
			assert.Empty(t, wire, "rejected publication must not reach the broker")
			assert.Empty(t, results)
			assert.Empty(t, problems)
		})
	}
}

// TestQueuedDiscardAfterSendAttempt covers ambiguous QoS 0 writes followed by a local rejection, both after
// reconnecting and after reopening a persistent queue with a new tracker that has never observed the earlier attempt.
func TestQueuedDiscardAfterSendAttempt(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			dir := t.TempDir()
			q, err := filequeue.New(dir, "queue-", ".msg")
			require.NoError(t, err)
			results := make(chan delivery.Result, 4)
			problems := make(chan delivery.Problem, 4)
			trackerConfig := delivery.Config{
				OnResult: func(r delivery.Result) { results <- r },
				OnError:  func(p delivery.Problem) { problems <- p },
			}
			tracker := delivery.New(trackerConfig)
			received := make(chan struct{})
			payload := []byte("delivered before the write failed")
			firstBroker := testserver.New(nil)
			firstBroker.SetPacketReceivedCallback(func(cp *packets.ControlPacket) error {
				if p, ok := cp.Content.(*packets.Publish); ok {
					assert.Equal(t, payload, p.Payload)
					assert.True(t, p.Retain)
					close(received)
				}
				return nil
			})
			secondBroker := testserver.New(nil)
			secondBroker.SetConnectCallback(func(_ *packets.Connect, ca *packets.Connack) {
				available := byte(0)
				ca.Properties.RetainAvailable = &available
			})
			secondBroker.SetPacketReceivedCallback(func(cp *packets.ControlPacket) error {
				assert.NotEqual(t, byte(packets.PUBLISH), cp.Type, "the retry must be rejected locally")
				return nil
			})
			brokerDone := make(chan chan struct{}, 4)
			// Registered before the managers' cleanup so their connection attempts finish before draining this channel.
			t.Cleanup(func() {
				close(brokerDone)
				for done := range brokerDone {
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("broker failed to shut down")
					}
				}
			})
			attempts := 0
			cfg := autopaho.ClientConfig{
				ServerUrls:       []*url.URL{{Scheme: "mqtt", Host: "test.invalid:1883"}},
				Queue:            q,
				ReconnectBackoff: func(int) time.Duration { return 0 },
				OnConnectionDown: func() bool { return !restart },
				ClientConfig:     paho.ClientConfig{ClientID: "delivery-retry", PublishEvents: tracker.Handler()},
				AttemptConnection: func(ctx context.Context, _ autopaho.ClientConfig, _ *url.URL) (net.Conn, error) {
					attempts++
					broker := secondBroker
					if attempts == 1 {
						broker = firstBroker
					}
					conn, done, err := broker.Connect(ctx)
					if err != nil {
						return nil, err
					}
					select {
					case brokerDone <- done:
					case <-ctx.Done():
						_ = conn.Close()
						return nil, ctx.Err()
					}
					if attempts == 1 {
						conn = packets.NewThreadSafeConn(&failAfterPayloadConn{
							Conn: conn, ctx: ctx, payload: payload, received: received,
						})
					}
					return conn, nil
				},
			}
			start := func(cfg autopaho.ClientConfig) *autopaho.ConnectionManager {
				cfg.Session = state.NewInMemory()
				cm, err := autopaho.NewConnection(ctx, cfg)
				require.NoError(t, err)
				t.Cleanup(func() {
					cancel()
					<-cm.Done()
					require.NoError(t, cfg.Session.Close())
				})
				return cm
			}
			nextResult := func() delivery.Result {
				t.Helper()
				select {
				case r := <-results:
					return r
				case p := <-problems:
					t.Fatalf("unexpected tracking problem: %v", p.Err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				return delivery.Result{}
			}
			cm := start(cfg)
			p := &paho.Publish{QoS: 0, Retain: true, Topic: "delivery/retry", Payload: payload}
			require.NoError(t, tracker.TrackWithID(p, "previously-transmitted"))
			require.NoError(t, cm.PublishViaQueue(ctx, &autopaho.QueuePublish{Publish: p}))
			r := nextResult()
			require.Equal(t, delivery.SendAttempted, r.Outcome)
			select {
			case <-received:
			case <-ctx.Done():
				t.Fatal("the first broker did not receive the publication")
			}
			if restart {
				select {
				case <-cm.Done():
				case <-ctx.Done():
					t.Fatal("the first manager did not stop after the write failure")
				}
				q, err = filequeue.New(dir, "queue-", ".msg")
				require.NoError(t, err)
				cfg.Queue = q
				cfg.PublishEvents = delivery.New(trackerConfig).Handler()
				cm = start(cfg)
			}
			r = nextResult()
			assert.Equal(t, delivery.Unknown, r.Outcome)
			assert.Equal(t, "previously-transmitted", r.ID)
			assert.Zero(t, r.QoS)
			assert.Zero(t, r.PacketID)
			assert.Zero(t, r.Trigger)
			assert.ErrorIs(t, r.Err, delivery.ErrUnconfirmed)
			assert.ErrorIs(t, r.Err, paho.ErrInvalidArguments)
			select {
			case <-q.WaitForEmpty():
			case <-ctx.Done():
				t.Fatal("rejected entry was not removed")
			}
			cancel()
			<-cm.Done()
			assert.Empty(t, results)
			assert.Empty(t, problems)
		})
	}
}

// failAfterPayloadConn models a write that reports failure after the peer has received the complete publication.
// MQTT packet writes pass the payload as the last buffer; waiting for received makes the delivery ambiguity explicit.
type failAfterPayloadConn struct {
	net.Conn
	ctx      context.Context
	payload  []byte
	received <-chan struct{}
}

func (c *failAfterPayloadConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil && bytes.Equal(p, c.payload) {
		select {
		case <-c.received:
			return n, errors.New("network write failed after the broker received the publication")
		case <-c.ctx.Done():
			return n, c.ctx.Err()
		}
	}
	return n, err
}
