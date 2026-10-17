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

package paho

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/internal/basictestserver"
	"github.com/eclipse/paho.golang/packets"
	paholog "github.com/eclipse/paho.golang/paho/log"
	"github.com/eclipse/paho.golang/paho/session"
	"github.com/eclipse/paho.golang/paho/session/state"
	"github.com/eclipse/paho.golang/paho/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClientPublishEvents checks publish callbacks in blocking and asynchronous modes.
// QoS 0 reports BeforeSend; QoS 1/2 completion events contain the final acknowledgement
// and its reason code, including rejection responses. Stored sources are not loaded by default.
func TestClientPublishEvents(t *testing.T) {
	for _, method := range []PublishMethod{PublishMethod_Blocking, PublishMethod_AsyncSend} {
		for _, tc := range []struct{ qos, ack, rec, comp byte }{
			{qos: 0}, {qos: 1}, {qos: 1, ack: 0x87},
			{qos: 2}, {qos: 2, rec: 0x97}, {qos: 2, comp: 0x92},
		} {
			t.Run(fmt.Sprintf("%d/%+v", method, tc), func(t *testing.T) {
				ts := basictestserver.New(paholog.NewTestLogger(t, "broker:"))
				ts.SetResponse(packets.CONNACK, &packets.Connack{})
				ts.SetResponse(packets.PUBACK, &packets.Puback{ReasonCode: tc.ack})
				ts.SetResponse(packets.PUBREC, &packets.Pubrec{ReasonCode: tc.rec})
				ts.SetResponse(packets.PUBCOMP, &packets.Pubcomp{ReasonCode: tc.comp})
				go ts.Run()
				t.Cleanup(ts.Stop)
				type observed struct {
					beforeSend bool
					kind       session.PublishEventType
					trigger    byte
					reason     byte
				}
				events := make(chan observed, 8)
				c := NewClient(ClientConfig{
					Conn: ts.ClientConn(),
					PublishEvents: session.PublishEventHandler{
						BeforeSend: func(session.PublishSend) { events <- observed{beforeSend: true} },
						OnEvent: func(e session.PublishEvent) {
							o := observed{kind: e.Type}
							assert.Nil(t, e.Source, "default must not load a stored packet")
							if e.Trigger != nil {
								o.trigger = e.Trigger.Type
								switch p := e.Trigger.Content.(type) {
								case *packets.Puback:
									o.reason = p.ReasonCode
								case *packets.Pubrec:
									o.reason = p.ReasonCode
								case *packets.Pubcomp:
									o.reason = p.ReasonCode
								}
							}
							events <- o
						}},
				})
				_, err := c.Connect(t.Context(), &Connect{ClientID: "events"})
				require.NoError(t, err)
				t.Cleanup(func() { _ = c.Disconnect(&Disconnect{}) })
				_, err = c.PublishWithOptions(t.Context(), &Publish{QoS: tc.qos, Topic: "events"}, PublishOptions{Method: method})
				if method == PublishMethod_Blocking && (tc.ack >= 0x80 || tc.rec >= 0x80) {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				timer := time.NewTimer(time.Second)
				defer timer.Stop()
				for {
					select {
					case o := <-events:
						if (tc.qos == 0 && !o.beforeSend) || (tc.qos > 0 && (o.beforeSend || o.kind != session.PublishEventComplete)) {
							continue
						}
						if tc.qos > 0 {
							wantTrigger, wantReason := byte(packets.PUBACK), tc.ack
							if tc.qos == 2 {
								wantTrigger, wantReason = packets.PUBCOMP, tc.comp
								if tc.rec >= 0x80 {
									wantTrigger, wantReason = packets.PUBREC, tc.rec
								}
							}
							assert.Equal(t, wantTrigger, o.trigger)
							assert.Equal(t, wantReason, o.reason)
						}
						return
					case <-timer.C:
						t.Fatal("publish event not delivered")
					}
				}
			})
		}
	}
}

// TestPublishEventsContinueAfterPublishCancellation verifies that cancelling a Publish call
// does not prevent a later acknowledgement from producing a completion event with its stored source.
func TestPublishEventsContinueAfterPublishCancellation(t *testing.T) {
	ts := basictestserver.New(paholog.NewTestLogger(t, "broker:"))
	ts.SetResponse(packets.CONNACK, &packets.Connack{})
	go ts.Run()
	t.Cleanup(ts.Stop)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sent := make(chan uint16, 1)
	completed := make(chan uint16, 1)
	c := NewClient(ClientConfig{Conn: ts.ClientConn(), PublishEvents: session.PublishEventHandler{
		ReadStoredPacket: session.ReadStoredFull,
		BeforeSend: func(e session.PublishSend) {
			sent <- e.Source.PacketID()
			cancel()
		},
		OnEvent: func(e session.PublishEvent) {
			if e.Type == session.PublishEventComplete {
				assert.Equal(t, "still tracked", string(e.Source.Content.(*packets.Publish).Payload))
				completed <- e.PacketID
			}
		},
	}})
	_, err := c.Connect(t.Context(), &Connect{ClientID: "events-cancel"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(&Disconnect{}) })
	_, err = c.Publish(ctx, &Publish{QoS: 1, Topic: "events", Payload: []byte("still tracked")})
	require.ErrorIs(t, err, context.Canceled)
	id := <-sent
	require.NoError(t, ts.SendPacket(&packets.Puback{PacketID: id}))
	select {
	case got := <-completed:
		assert.Equal(t, id, got)
	case <-time.After(time.Second):
		t.Fatal("completion lost when Publish returned")
	}
}

// Wrappers embedding SessionManager also forward publish callback registration.
type eventSessionWrapper struct{ session.SessionManager }

type publishEventReadCountingStore struct {
	*memory.Store
	reads atomic.Int32
}

// Get counts the read attempt, then retrieves the packet from the underlying memory store.
func (s *publishEventReadCountingStore) Get(id uint16) (io.ReadCloser, error) {
	s.reads.Add(1)
	return s.Store.Get(id)
}

// TestPublishEventsDisabledOnReusedSession verifies that a new client with no publish callbacks
// clears the previous client's callbacks on a shared session. Only reads needed for retransmission
// should occur, even if ReadStoredPacket is enabled on the new client.
func TestPublishEventsDisabledOnReusedSession(t *testing.T) {
	for _, present := range []bool{false, true} {
		for _, readStored := range []session.StoredPacketReadMode{session.ReadStoredNone, session.ReadStoredHeader, session.ReadStoredFull} {
			t.Run(fmt.Sprintf("sessionPresent=%t/readStored=%d", present, readStored), func(t *testing.T) {
				store := &publishEventReadCountingStore{Store: memory.New()}
				s := state.New(store, memory.New())
				t.Cleanup(func() { _ = s.Close() })
				var callbacks atomic.Int32
				// connect creates a client using the shared session and supplied callbacks.
				// The test broker reports whether the previous session is still present.
				connect := func(handler session.PublishEventHandler, sessionPresent bool) *Client {
					ts := basictestserver.New(paholog.NOOPLogger{})
					ts.SetResponse(packets.CONNACK, &packets.Connack{SessionPresent: sessionPresent})
					ts.SetResponse(packets.PUBACK, &packets.Puback{})
					ts.SetResponse(packets.PUBREC, &packets.Pubrec{})
					ts.SetResponse(packets.PUBCOMP, &packets.Pubcomp{})
					go ts.Run()
					t.Cleanup(ts.Stop)
					c := NewClient(ClientConfig{Conn: ts.ClientConn(), Session: s, PublishEvents: handler})
					expiry := uint32(600)
					_, err := c.Connect(t.Context(), &Connect{
						ClientID: "events-reused", Properties: &ConnectProperties{SessionExpiryInterval: &expiry},
					})
					require.NoError(t, err)
					t.Cleanup(func() { _ = c.Disconnect(&Disconnect{}) })
					return c
				}
				first := connect(session.PublishEventHandler{
					ReadStoredPacket: session.ReadStoredFull,
					OnEvent:          func(session.PublishEvent) { callbacks.Add(1) },
					BeforeSend:       func(session.PublishSend) { callbacks.Add(1) },
					PreparePubrel: func(session.PubrelPreparation) {
						callbacks.Add(1)
					},
				}, false)
				_, err := first.Publish(t.Context(), &Publish{QoS: 1, Topic: "events"})
				require.NoError(t, err)
				// Leave one publication for the replacement connection to resume or discard.
				pending, err := s.AddToSession(t.Context(), &packets.Publish{QoS: 1, Topic: "pending"})
				require.NoError(t, err)
				require.NoError(t, first.Disconnect(&Disconnect{}))
				oldCallbacks, oldReads := callbacks.Load(), store.reads.Load()
				require.Positive(t, oldCallbacks)
				require.Positive(t, oldReads)

				second := connect(session.PublishEventHandler{ReadStoredPacket: readStored}, present)
				select {
				case response := <-pending:
					if present {
						assert.Equal(t, byte(packets.PUBACK), response.Type)
					} else {
						assert.Zero(t, response.Type)
					}
				case <-time.After(time.Second):
					t.Fatal("pending publication was not completed or discarded")
				}
				_, err = second.Publish(t.Context(), &Publish{QoS: 2, Topic: "events"})
				require.NoError(t, err)
				require.NoError(t, second.Disconnect(&Disconnect{})) // Wait for all callbacks to finish.
				assert.Equal(t, oldCallbacks, callbacks.Load(), "the previous handler must be cleared")
				wantReads := oldReads
				if present {
					wantReads++ // Retransmission still needs to load the pending PUBLISH.
				}
				assert.Equal(t, wantReads, store.reads.Load(), "disabled events must not load stored sources")
			})
		}
	}
}

// TestPublishEventsOnSessionWrapper verifies that a wrapper embedding SessionManager forwards
// callback registration, allowing BeforeSend, PreparePubrel, and completion callbacks to run.
func TestPublishEventsOnSessionWrapper(t *testing.T) {
	ts := basictestserver.New(paholog.NOOPLogger{})
	ts.SetResponse(packets.CONNACK, &packets.Connack{})
	ts.SetResponse(packets.PUBREC, &packets.Pubrec{})
	ts.SetResponse(packets.PUBCOMP, &packets.Pubcomp{})
	go ts.Run()
	t.Cleanup(ts.Stop)
	s := state.NewInMemory()
	t.Cleanup(func() { _ = s.Close() })
	var sent, prepared atomic.Int32
	completed := make(chan uint16, 1)
	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(), Session: &eventSessionWrapper{s},
		PublishEvents: session.PublishEventHandler{
			ReadStoredPacket: session.ReadStoredFull,
			BeforeSend:       func(session.PublishSend) { sent.Add(1) },
			PreparePubrel: func(e session.PubrelPreparation) {
				assert.NotNil(t, e.Source)
				prepared.Add(1)
			},
			OnEvent: func(e session.PublishEvent) {
				assert.NoError(t, e.Err)
				if e.Type == session.PublishEventComplete {
					assert.NotNil(t, e.Source)
					completed <- e.PacketID
				}
			},
		},
	})
	_, err := c.Connect(t.Context(), &Connect{ClientID: "events-wrapper"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(&Disconnect{}) })
	_, err = c.Publish(t.Context(), &Publish{QoS: 2, Topic: "events"})
	require.NoError(t, err)
	select {
	case id := <-completed:
		assert.NotZero(t, id)
	case <-time.After(time.Second):
		t.Fatal("completion not delivered through session wrapper")
	}
	assert.Equal(t, int32(2), sent.Load())
	assert.Equal(t, int32(1), prepared.Load())
}
