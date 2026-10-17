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

package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho/session"
	filestore "github.com/eclipse/paho.golang/paho/store/file"
	"github.com/eclipse/paho.golang/paho/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventTestStore wraps a packet store to count read/write attempts and inject
// read, decode, or write failures during publication lifecycle tests.
type eventTestStore struct {
	storer
	gets    int
	puts    int
	readErr error
	putErr  error
	corrupt bool
}

// Get counts the read attempt, then returns an injected error, corrupt packet
// data, or the underlying stored packet.
func (s *eventTestStore) Get(id uint16) (io.ReadCloser, error) {
	s.gets++
	if s.readErr != nil {
		return nil, s.readErr
	}
	if s.corrupt {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	return s.storer.Get(id)
}

// Put counts the write attempt and either returns an injected error or stores
// the packet through the wrapped store.
func (s *eventTestStore) Put(id uint16, kind byte, p io.WriterTo) error {
	s.puts++
	if s.putErr != nil {
		return s.putErr
	}
	return s.storer.Put(id, kind, p)
}

// eventConnect establishes a connection with a persistent session, optionally
// resuming existing state and writing retransmissions to w.
func eventConnect(t *testing.T, s *State, w io.Writer, present bool) {
	t.Helper()
	expiry := uint32(600)
	require.NoError(t, s.ConAckReceived(w,
		&packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}},
		&packets.Connack{SessionPresent: present}))
}

// eventPublish builds a publication with an application ID to retain in storage
// and a public User Property to preserve during wire filtering.
func eventPublish(qos byte) *packets.Publish {
	return &packets.Publish{
		QoS: qos, Topic: "events/test", Payload: []byte("payload"),
		Properties: &packets.Properties{User: []packets.User{
			{Key: "app-id", Value: "order/42"},
			{Key: "public", Value: "keep"},
		}},
	}
}

// eventAck constructs a PUBACK, PUBREC, or PUBCOMP with the supplied packet
// identifier and reason code.
func eventAck(kind byte, id uint16, reason byte) *packets.ControlPacket {
	p := packets.NewControlPacket(kind)
	switch v := p.Content.(type) {
	case *packets.Puback:
		v.PacketID, v.ReasonCode = id, reason
	case *packets.Pubrec:
		v.PacketID, v.ReasonCode = id, reason
	case *packets.Pubcomp:
		v.PacketID, v.ReasonCode = id, reason
	}
	return p
}

// eventUsers returns the User Properties of a PUBLISH or PUBREL whose Properties
// is non-nil, or nil for another packet type.
func eventUsers(p *packets.ControlPacket) []packets.User {
	switch v := p.Content.(type) {
	case *packets.Publish:
		return v.Properties.User
	case *packets.Pubrel:
		return v.Properties.User
	}
	return nil
}

// eventRead loads and decodes a stored packet, closes its reader, and fails the
// test if any of those operations fails.
func eventRead(t *testing.T, store storer, id uint16) *packets.ControlPacket {
	t.Helper()
	r, err := store.Get(id)
	require.NoError(t, err)
	p, err := packets.ReadPacket(r)
	require.NoError(t, r.Close())
	require.NoError(t, err)
	return p
}

// assertEventUnlocked checks that a callback can acquire the state mutex,
// confirming it was not invoked while that mutex was held.
func assertEventUnlocked(t *testing.T, s *State) {
	t.Helper()
	locked := s.mu.TryLock()
	assert.True(t, locked, "callback must run outside the state mutex")
	if locked {
		s.mu.Unlock()
	}
}

// TestUnknownPubrecBeforeSend verifies that unknown-ID responses use the wire hook without creating a transaction
// or invoking callbacks that require a known publication.
func TestUnknownPubrecBeforeSend(t *testing.T) {
	store := &eventTestStore{storer: memory.New()}
	s := New(store, memory.New())
	defer s.Close()
	sent := 0
	s.SetPublishEventHandler(session.PublishEventHandler{
		ReadStoredPacket: true,
		BeforeSend: func(e session.PublishSend) {
			assertEventUnlocked(t, s)
			sent++
			source := e.Source.Content.(*packets.Pubrel)
			assert.Equal(t, uint16(42), source.PacketID)
			assert.Equal(t, byte(0x92), source.ReasonCode)
			assert.Nil(t, source.Properties)
			outgoing := e.Outgoing.Content.(*packets.Pubrel)
			outgoing.Properties = &packets.Properties{User: []packets.User{{Key: "wire", Value: "unknown"}}}
			assert.Nil(t, source.Properties, "wire edits must leave the source unchanged")
		},
		PreparePubrel: func(session.PubrelPreparation) { t.Error("unknown PUBREC must not prepare a stored PUBREL") },
		OnEvent:       func(session.PublishEvent) { t.Error("unknown PUBREC must not report a publication event") },
	})
	var wire bytes.Buffer
	eventConnect(t, s, &wire, false)
	rec := eventAck(packets.PUBREC, 42, 0)
	require.NoError(t, s.PacketReceived(rec, nil))
	got, err := packets.ReadPacket(&wire)
	require.NoError(t, err)
	assert.Equal(t, byte(packets.PUBREL), got.Type)
	assert.Equal(t, byte(2), got.Flags)
	assert.Equal(t, uint16(42), got.PacketID())
	assert.Equal(t, byte(0x92), got.Content.(*packets.Pubrel).ReasonCode)
	assert.Equal(t, []packets.User{{Key: "wire", Value: "unknown"}}, eventUsers(got))
	assert.Equal(t, 1, sent)
	assert.Zero(t, store.gets)
	assert.Zero(t, store.puts)
	assert.Empty(t, s.clientPackets)

	require.NoError(t, s.ConnectionLost(nil))
	require.ErrorIs(t, s.PacketReceived(rec, nil), session.ErrNoConnection)
	assert.Equal(t, 1, sent, "no wire hook when there is no connection to write to")
	assert.Zero(t, wire.Len())
}

// TestPublishEventOptionalReads verifies that callback source reads are optional, while a repeated PUBREC always
// reads the stored PUBREL for retransmission.
func TestPublishEventOptionalReads(t *testing.T) {
	for _, qos := range []byte{1, 2} {
		for _, enabled := range []bool{false, true} {
			for _, load := range []bool{false, true} {
				t.Run(fmt.Sprintf("qos%d/handler=%t/read=%t", qos, enabled, load), func(t *testing.T) {
					store := &eventTestStore{storer: memory.New()}
					s := New(store, memory.New())
					defer s.Close()
					var kinds []session.PublishEventType
					handler := session.PublishEventHandler{ReadStoredPacket: load}
					if enabled {
						handler.OnEvent = func(e session.PublishEvent) {
							assertEventUnlocked(t, s)
							kinds = append(kinds, e.Type)
							assert.Equal(t, load, e.Source != nil)
							assert.NotNil(t, e.Trigger)
							assert.NoError(t, e.Err)
						}
					}
					s.SetPublishEventHandler(handler)
					eventConnect(t, s, io.Discard, false)
					pub := eventPublish(qos)
					response, err := s.AddToSession(t.Context(), pub)
					require.NoError(t, err)
					if qos == 2 {
						rec := eventAck(packets.PUBREC, pub.PacketID, 0)
						require.NoError(t, s.PacketReceived(rec, nil))
						require.NoError(t, s.PacketReceived(rec, nil))
						assert.Equal(t, 2, store.puts, "duplicate PUBREC must not rewrite PUBREL")
					}
					final := byte(packets.PUBACK)
					if qos == 2 {
						final = packets.PUBCOMP
					}
					require.NoError(t, s.PacketReceived(eventAck(final, pub.PacketID, 0), nil))
					assert.Equal(t, final, (<-response).Type)
					_, open := <-response
					assert.False(t, open)
					wantReads := 0
					if qos == 2 {
						wantReads = 1 // Repeated PUBREC must resend the stored PUBREL.
					}
					if enabled && load {
						wantReads = 1
						if qos == 2 {
							wantReads = 3
						}
					}
					assert.Equal(t, wantReads, store.gets)
					if enabled {
						assert.Equal(t, session.PublishEventComplete, kinds[len(kinds)-1])
					}
				})
			}
		}
	}
}

// TestPublishEventsSurviveRestart verifies that application IDs survive disk-store recovery at each QoS stage, remain
// absent from wire packets, and reach completion callbacks even after repeated PUBRECs.
func TestPublishEventsSurviveRestart(t *testing.T) {
	for _, stage := range []string{"qos1-publish", "qos2-publish", "qos2-pubrel"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			// openStore reopens the same directory to simulate persistence across a process restart.
			openStore := func() *filestore.Store {
				store, err := filestore.New(dir, "client-", ".pkt")
				require.NoError(t, err)
				return store
			}
			store := openStore()
			s := New(store, memory.New())
			var completed []string
			var losses int
			handler := session.PublishEventHandler{ReadStoredPacket: true}
			handler.BeforeSend = func(e session.PublishSend) {
				assertEventUnlocked(t, s)
				switch p := e.Outgoing.Content.(type) {
				case *packets.Publish:
					props := *p.Properties
					props.User = slices.Clone(props.User)
					p.Properties = &props
					p.Properties.User = slices.DeleteFunc(p.Properties.User, func(u packets.User) bool { return u.Key == "app-id" })
				case *packets.Pubrel:
					props := *p.Properties
					props.User = slices.Clone(props.User)
					p.Properties = &props
					p.Properties.User = slices.DeleteFunc(p.Properties.User, func(u packets.User) bool { return u.Key == "app-id" })
				}
				assert.Contains(t, eventUsers(e.Source), packets.User{Key: "app-id", Value: "order/42"})
			}
			handler.PreparePubrel = func(e session.PubrelPreparation) {
				assertEventUnlocked(t, s)
				for _, u := range e.Source.Properties.User {
					if u.Key == "app-id" {
						e.Outgoing.Properties.User = append(e.Outgoing.Properties.User, u)
					}
				}
			}
			handler.OnEvent = func(e session.PublishEvent) {
				assertEventUnlocked(t, s)
				require.NoError(t, e.Err)
				switch e.Type {
				case session.PublishEventComplete:
					for _, u := range eventUsers(e.Source) {
						if u.Key == "app-id" {
							completed = append(completed, u.Value)
						}
					}
				case session.PublishEventSessionLost:
					losses++
				}
			}
			s.SetPublishEventHandler(handler)
			eventConnect(t, s, io.Discard, false)
			qos := byte(2)
			if stage == "qos1-publish" {
				qos = 1
			}
			pub := eventPublish(qos)
			_, err := s.AddToSession(t.Context(), pub)
			require.NoError(t, err)
			if stage == "qos2-pubrel" {
				require.NoError(t, s.PacketReceived(eventAck(packets.PUBREC, pub.PacketID, 0), nil))
			}
			require.NoError(t, s.Close())
			assert.Zero(t, losses, "closing a persistent session is not session loss")
			assert.Empty(t, completed)

			// New store and state objects deliberately discard all in-memory tracking.
			store = openStore()
			s = New(store, memory.New())
			defer s.Close()
			s.SetPublishEventHandler(handler)
			var wire bytes.Buffer
			eventConnect(t, s, &wire, true)
			p, err := packets.ReadPacket(&wire)
			require.NoError(t, err)
			assert.NotContains(t, eventUsers(p), packets.User{Key: "app-id", Value: "order/42"})
			if stage == "qos2-pubrel" {
				assert.Equal(t, byte(packets.PUBREL), p.Type)
			} else {
				assert.True(t, p.Content.(*packets.Publish).Duplicate)
				assert.Contains(t, eventUsers(p), packets.User{Key: "public", Value: "keep"})
			}
			assert.Contains(t, eventUsers(eventRead(t, store, pub.PacketID)), packets.User{Key: "app-id", Value: "order/42"})
			final := byte(packets.PUBACK)
			if qos == 2 {
				final = packets.PUBCOMP
				// Also exercises duplicate PUBREC after restarting with PUBREL.
				require.NoError(t, s.PacketReceived(eventAck(packets.PUBREC, pub.PacketID, 0), nil))
				require.NoError(t, s.PacketReceived(eventAck(packets.PUBREC, pub.PacketID, 0), nil))
				assert.Contains(t, eventUsers(eventRead(t, store, pub.PacketID)), packets.User{Key: "app-id", Value: "order/42"})
			}
			require.NoError(t, s.PacketReceived(eventAck(final, pub.PacketID, 0), nil))
			assert.Equal(t, []string{"order/42"}, completed)
			ids, err := store.List()
			require.NoError(t, err)
			assert.Empty(t, ids)
		})
	}
}

// TestPublishEventCompletionTriggers verifies that each terminal acknowledgement, including rejection, produces one
// completion notification with the original trigger and the appropriate stored PUBLISH or PUBREL.
func TestPublishEventCompletionTriggers(t *testing.T) {
	for _, tc := range []struct{ kind, reason byte }{
		{packets.PUBACK, 0}, {packets.PUBACK, 0x10}, {packets.PUBACK, 0x87},
		{packets.PUBREC, 0x80}, {packets.PUBREC, 0x97},
		{packets.PUBCOMP, 0}, {packets.PUBCOMP, 0x92},
	} {
		t.Run(fmt.Sprintf("%d/%x", tc.kind, tc.reason), func(t *testing.T) {
			s := NewInMemory()
			defer s.Close()
			eventConnect(t, s, io.Discard, false)
			qos := byte(2)
			if tc.kind == packets.PUBACK {
				qos = 1
			}
			pub := eventPublish(qos)
			response, err := s.AddToSession(t.Context(), pub)
			require.NoError(t, err)
			if tc.kind == packets.PUBCOMP {
				require.NoError(t, s.PacketReceived(eventAck(packets.PUBREC, pub.PacketID, 0), nil))
			}
			trigger := eventAck(tc.kind, pub.PacketID, tc.reason)
			count := 0
			s.SetPublishEventHandler(session.PublishEventHandler{ReadStoredPacket: true, OnEvent: func(e session.PublishEvent) {
				assertEventUnlocked(t, s)
				count++
				assert.Equal(t, session.PublishEventComplete, e.Type)
				assert.Same(t, trigger, e.Trigger)
				assert.Equal(t, pub.PacketID, e.PacketID)
				wantSource := byte(packets.PUBLISH)
				if tc.kind == packets.PUBCOMP {
					wantSource = packets.PUBREL
				}
				assert.Equal(t, wantSource, e.Source.Type)
			}})
			require.NoError(t, s.PacketReceived(trigger, nil))
			require.NoError(t, s.PacketReceived(trigger, nil))
			assert.Equal(t, 1, count)
			assert.Equal(t, tc.kind, (<-response).Type)
		})
	}
}

// TestPublishEventSessionLostBeforeLoadingState verifies that a missing broker session reports and removes persisted
// publications before they have been loaded into memory, with source reads remaining optional.
func TestPublishEventSessionLostBeforeLoadingState(t *testing.T) {
	for _, load := range []bool{false, true} {
		t.Run(fmt.Sprint(load), func(t *testing.T) {
			store := &eventTestStore{storer: memory.New()}
			pub := eventPublish(1)
			pub.PacketID = 7
			require.NoError(t, store.Put(7, packets.PUBLISH, pub))
			rel := &packets.Pubrel{PacketID: 9, Properties: &packets.Properties{User: pub.Properties.User}}
			require.NoError(t, store.Put(9, packets.PUBREL, rel))
			s := New(store, memory.New())
			defer s.Close()
			var ids []uint16
			s.SetPublishEventHandler(session.PublishEventHandler{ReadStoredPacket: load, OnEvent: func(e session.PublishEvent) {
				assertEventUnlocked(t, s)
				assert.Equal(t, session.PublishEventSessionLost, e.Type)
				assert.Equal(t, byte(packets.CONNACK), e.Trigger.Type)
				assert.NoError(t, e.Err)
				assert.Equal(t, load, e.Source != nil)
				ids = append(ids, e.PacketID)
			}})
			eventConnect(t, s, io.Discard, false)
			assert.Equal(t, []uint16{7, 9}, ids)
			wantReads := 0
			if load {
				wantReads = 2
			}
			assert.Equal(t, wantReads, store.gets)
			left, err := store.List()
			require.NoError(t, err)
			assert.Empty(t, left)
		})
	}
}

// TestPublishEventSessionLostStreamsStoredPackets verifies that session loss reads and reports one stored payload at a
// time, releases waiting publishers, and runs callbacks outside the state and store mutexes.
func TestPublishEventSessionLostStreamsStoredPackets(t *testing.T) {
	for _, transition := range []string{"connack", "disconnect", "close"} {
		t.Run(transition, func(t *testing.T) {
			disk, err := filestore.New(t.TempDir(), "client-", ".pkt")
			require.NoError(t, err)
			store := &eventTestStore{storer: disk}
			s := New(store, memory.New())
			defer s.Close()
			if transition != "connack" {
				require.NoError(t, s.ConAckReceived(io.Discard, &packets.Connect{}, &packets.Connack{}))
			}
			const count = 32
			payload := bytes.Repeat([]byte("payload"), 32*1024)
			responses := make(map[uint16]<-chan packets.ControlPacket)
			for id := uint16(1); id <= count; id++ {
				pub := eventPublish(1)
				pub.Payload = payload
				if transition == "connack" {
					pub.PacketID = id
					require.NoError(t, store.Put(id, packets.PUBLISH, pub))
				} else {
					response, err := s.AddToSession(t.Context(), pub)
					require.NoError(t, err)
					responses[pub.PacketID] = response
				}
			}
			seen := make(map[uint16]bool)
			maxReadAhead := 0
			s.SetPublishEventHandler(session.PublishEventHandler{ReadStoredPacket: true, OnEvent: func(e session.PublishEvent) {
				assertEventUnlocked(t, s)
				locked := disk.TryLock()
				assert.True(t, locked, "callback must run outside the store mutex")
				if locked {
					disk.Unlock()
				}
				maxReadAhead = max(maxReadAhead, store.gets-len(seen))
				assert.Equal(t, session.PublishEventSessionLost, e.Type)
				assert.NoError(t, e.Err)
				assert.False(t, seen[e.PacketID], "duplicate session-loss notification")
				seen[e.PacketID] = true
				if response := responses[e.PacketID]; response != nil {
					select {
					case packet, open := <-response:
						assert.True(t, open)
						assert.Zero(t, packet.Type)
					default:
						t.Error("discarded publish must be released before its callback")
					}
				}
				if assert.NotNil(t, e.Source) {
					assert.Equal(t, e.PacketID, e.Source.PacketID())
					assert.Equal(t, payload, e.Source.Content.(*packets.Publish).Payload)
					assert.Contains(t, eventUsers(e.Source), packets.User{Key: "app-id", Value: "order/42"})
				}
			}})
			switch transition {
			case "connack":
				err = s.ConAckReceived(io.Discard, &packets.Connect{}, &packets.Connack{})
			case "disconnect":
				err = s.ConnectionLost(nil)
			case "close":
				err = s.Close()
			}
			require.NoError(t, err)
			assert.Equal(t, count, len(seen))
			assert.Equal(t, count, store.gets)
			assert.LessOrEqual(t, maxReadAhead, 1, "deliver each source before loading the next payload")
			ids, err := store.List()
			require.NoError(t, err)
			assert.Empty(t, ids)
		})
	}
}

// TestPublishEventSessionLostSerializesStoreCleanup verifies that reconnect, disconnect, and close wait for ongoing
// session-loss notifications, while late acknowledgements and new publications cannot disturb the remaining sources.
func TestPublishEventSessionLostSerializesStoreCleanup(t *testing.T) {
	for _, tc := range []struct{ loss, next string }{
		{"disconnect", "reconnect"}, {"disconnect", "disconnect"}, {"disconnect", "close"},
		{"connack", "reconnect"}, {"connack", "disconnect"}, {"connack", "close"},
	} {
		t.Run(tc.loss+"/"+tc.next, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := NewInMemory()
				defer s.Close()
				connect := &packets.Connect{}
				if tc.loss == "connack" {
					expiry := uint32(600)
					connect.Properties = &packets.Properties{SessionExpiryInterval: &expiry}
				}
				require.NoError(t, s.ConAckReceived(io.Discard, connect, &packets.Connack{}))
				first, second := eventPublish(1), eventPublish(1)
				_, err := s.AddToSession(t.Context(), first)
				require.NoError(t, err)
				_, err = s.AddToSession(t.Context(), second)
				require.NoError(t, err)
				if tc.loss == "connack" {
					require.NoError(t, s.ConnectionLost(nil))
				}
				entered, resume := make(chan struct{}), make(chan struct{})
				var ids []uint16
				s.SetPublishEventHandler(session.PublishEventHandler{ReadStoredPacket: true, OnEvent: func(e session.PublishEvent) {
					assertEventUnlocked(t, s)
					assert.Equal(t, session.PublishEventSessionLost, e.Type)
					assert.NoError(t, e.Err)
					assert.NotNil(t, e.Source)
					ids = append(ids, e.PacketID)
					if len(ids) == 1 {
						close(entered)
						<-resume
					}
				}})
				lost := make(chan error, 1)
				go func() {
					if tc.loss == "connack" {
						lost <- s.ConAckReceived(io.Discard, connect, &packets.Connack{})
					} else {
						lost <- s.ConnectionLost(nil)
					}
				}()
				<-entered
				// Late acknowledgements must not delete sources which have yet to be read.
				require.NoError(t, s.PacketReceived(eventAck(packets.PUBACK, second.PacketID, 0), nil))
				_, err = s.AddToSession(t.Context(), eventPublish(1))
				assert.ErrorIs(t, err, session.ErrNoConnection)
				var wire bytes.Buffer
				next := make(chan error, 1)
				go func() {
					switch tc.next {
					case "reconnect":
						next <- s.ConAckReceived(&wire, &packets.Connect{}, &packets.Connack{SessionPresent: true})
					case "disconnect":
						next <- s.ConnectionLost(nil)
					case "close":
						next <- s.Close()
					}
				}()
				synctest.Wait()
				assert.Empty(t, next, "store cleanup must wait until source notifications finish")
				close(resume)
				require.NoError(t, <-lost)
				require.NoError(t, <-next)
				assert.Equal(t, []uint16{first.PacketID, second.PacketID}, ids)
				assert.Zero(t, wire.Len(), "discarded publications must not be retransmitted")
			})
		})
	}
}

// TestPublishEventSessionLostContinuesAfterReadFailure verifies that a corrupt source is reported without preventing
// later notifications or store cleanup.
func TestPublishEventSessionLostContinuesAfterReadFailure(t *testing.T) {
	store := &eventTestStore{storer: memory.New()}
	s := New(store, memory.New())
	defer s.Close()
	require.NoError(t, s.ConAckReceived(io.Discard, &packets.Connect{}, &packets.Connack{}))
	first, second := eventPublish(1), eventPublish(1)
	_, err := s.AddToSession(t.Context(), first)
	require.NoError(t, err)
	_, err = s.AddToSession(t.Context(), second)
	require.NoError(t, err)
	store.corrupt = true
	var ids []uint16
	s.SetPublishEventHandler(session.PublishEventHandler{ReadStoredPacket: true, OnEvent: func(e session.PublishEvent) {
		ids = append(ids, e.PacketID)
		if e.PacketID == first.PacketID {
			assert.Error(t, e.Err)
			assert.Nil(t, e.Source)
			store.corrupt = false
		} else {
			assert.NoError(t, e.Err)
			assert.NotNil(t, e.Source)
		}
	}})
	require.NoError(t, s.ConnectionLost(nil))
	assert.Equal(t, []uint16{first.PacketID, second.PacketID}, ids)
	left, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, left)
}

// TestPublishEventSessionLostReportsKnownIDsAfterListFailure verifies that a store listing error produces a separate
// notification while publications already known to the state are still reported and removed.
func TestPublishEventSessionLostReportsKnownIDsAfterListFailure(t *testing.T) {
	store := &controlledListStore{Store: memory.New()}
	s := New(store, memory.New())
	defer s.Close()
	require.NoError(t, s.ConAckReceived(io.Discard, &packets.Connect{}, &packets.Connack{}))
	pub := eventPublish(1)
	_, err := s.AddToSession(t.Context(), pub)
	require.NoError(t, err)
	store.fail = true
	var ids []uint16
	s.SetPublishEventHandler(session.PublishEventHandler{ReadStoredPacket: true, OnEvent: func(e session.PublishEvent) {
		ids = append(ids, e.PacketID)
		if e.PacketID == 0 {
			assert.ErrorContains(t, e.Err, "listing discarded publications")
			assert.Nil(t, e.Source)
		} else {
			assert.NoError(t, e.Err)
			assert.NotNil(t, e.Source)
		}
	}})
	require.NoError(t, s.ConnectionLost(nil))
	assert.Equal(t, []uint16{0, pub.PacketID}, ids)
	left, err := store.Store.List()
	require.NoError(t, err)
	assert.Empty(t, left)
}

// TestPublishEventSessionLostReportsResetFailureSeparately verifies that a failed store reset is reported separately
// from discarded publications and that cleanup retries neither retransmit those publications nor repeat their
// notifications.
func TestPublishEventSessionLostReportsResetFailureSeparately(t *testing.T) {
	for _, load := range []bool{false, true} {
		t.Run(fmt.Sprint(load), func(t *testing.T) {
			store := &resetFailStore{Store: memory.New()}
			s := New(store, memory.New())
			defer s.Close()
			require.NoError(t, s.ConAckReceived(io.Discard, &packets.Connect{}, &packets.Connack{}))
			pub := eventPublish(1)
			_, err := s.AddToSession(t.Context(), pub)
			require.NoError(t, err)
			store.resetFailures = 2
			var ids []uint16
			s.SetPublishEventHandler(session.PublishEventHandler{ReadStoredPacket: load, OnEvent: func(e session.PublishEvent) {
				assertEventUnlocked(t, s)
				ids = append(ids, e.PacketID)
				assert.Equal(t, session.PublishEventSessionLost, e.Type)
				if e.PacketID == 0 {
					assert.ErrorContains(t, e.Err, "failed to reset client store")
					assert.Nil(t, e.Source)
				} else {
					assert.NoError(t, e.Err)
					assert.Equal(t, load, e.Source != nil)
				}
			}})
			require.ErrorContains(t, s.ConnectionLost(nil), "failed to reset client store")
			assert.Equal(t, []uint16{pub.PacketID, 0}, ids)
			var wire bytes.Buffer
			require.Error(t, s.ConAckReceived(&wire, &packets.Connect{}, &packets.Connack{SessionPresent: true}))
			require.NoError(t, s.ConAckReceived(&wire, &packets.Connect{}, &packets.Connack{SessionPresent: true}))
			assert.Zero(t, wire.Len(), "failed cleanup must not resurrect discarded publications")
			assert.Equal(t, []uint16{pub.PacketID, 0}, ids, "cleanup retries must not repeat notifications")
			left, err := store.List()
			require.NoError(t, err)
			assert.Empty(t, left)
		})
	}
}

// TestPublishEventSessionExpiresOnDisconnect verifies that DISCONNECT with zero session expiry reports each discarded
// publication once, retaining the triggering packet and stored application metadata.
func TestPublishEventSessionExpiresOnDisconnect(t *testing.T) {
	s := NewInMemory()
	defer s.Close()
	eventConnect(t, s, io.Discard, false)
	pub := eventPublish(1)
	_, err := s.AddToSession(context.Background(), pub)
	require.NoError(t, err)
	var count int
	s.SetPublishEventHandler(session.PublishEventHandler{ReadStoredPacket: true, OnEvent: func(e session.PublishEvent) {
		assertEventUnlocked(t, s)
		count++
		assert.Equal(t, session.PublishEventSessionLost, e.Type)
		assert.Equal(t, byte(packets.DISCONNECT), e.Trigger.Type)
		assert.Equal(t, pub.PacketID, e.PacketID)
		assert.Contains(t, eventUsers(e.Source), packets.User{Key: "app-id", Value: "order/42"})
	}})
	zero := uint32(0)
	require.NoError(t, s.ConnectionLost(&packets.Disconnect{Properties: &packets.Properties{SessionExpiryInterval: &zero}}))
	require.NoError(t, s.ConnectionLost(nil))
	assert.Equal(t, 1, count)
}

// TestPublishEventReadFailure verifies that source read and decode failures are reported without a Source: PUBREC
// processing preserves the stored PUBLISH for retry, while a final PUBACK still completes the transaction.
func TestPublishEventReadFailure(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		for _, kind := range []byte{packets.PUBACK, packets.PUBREC} {
			t.Run(fmt.Sprintf("corrupt=%t/kind=%d", corrupt, kind), func(t *testing.T) {
				store := &eventTestStore{storer: memory.New()}
				s := New(store, memory.New())
				defer s.Close()
				eventConnect(t, s, io.Discard, false)
				pub := eventPublish(1)
				if kind == packets.PUBREC {
					pub.QoS = 2
				}
				response, err := s.AddToSession(t.Context(), pub)
				require.NoError(t, err)
				store.corrupt = corrupt
				if !corrupt {
					store.readErr = errors.New("store unavailable")
				}
				count := 0
				s.SetPublishEventHandler(session.PublishEventHandler{ReadStoredPacket: true, OnEvent: func(e session.PublishEvent) {
					count++
					assert.Error(t, e.Err)
					assert.Nil(t, e.Source)
				}})
				err = s.PacketReceived(eventAck(kind, pub.PacketID, 0), nil)
				if kind == packets.PUBREC {
					require.Error(t, err)
					assert.Equal(t, 1, store.puts, "failed source read must not erase PUBLISH")
					assert.Equal(t, byte(packets.PUBLISH), eventRead(t, store.storer, pub.PacketID).Type)
					select {
					case <-response:
						t.Fatal("PUBREC read error completed publication")
					default:
					}
				} else {
					require.NoError(t, err)
					assert.Equal(t, byte(packets.PUBACK), (<-response).Type)
				}
				assert.Equal(t, 1, count)
			})
		}
	}
}

// TestPublishEventTransitionDoesNotRestoreDiscardedSession verifies that a PUBREC transition paused in either
// preparation or notification cannot restore a stored packet after concurrent session loss has discarded it.
func TestPublishEventTransitionDoesNotRestoreDiscardedSession(t *testing.T) {
	for _, prepare := range []bool{false, true} {
		t.Run(fmt.Sprintf("prepare=%t", prepare), func(t *testing.T) {
			store := &eventTestStore{storer: memory.New()}
			s := New(store, memory.New())
			defer s.Close()
			eventConnect(t, s, io.Discard, false)
			pub := eventPublish(2)
			_, err := s.AddToSession(t.Context(), pub)
			require.NoError(t, err)
			entered, resume := make(chan struct{}), make(chan struct{})
			handler := session.PublishEventHandler{ReadStoredPacket: true}
			// pause lets the test discard the session while its PUBREC callback is running.
			pause := func() { close(entered); <-resume }
			if prepare {
				handler.PreparePubrel = func(session.PubrelPreparation) { pause() }
			} else {
				handler.OnEvent = func(e session.PublishEvent) {
					if e.Type == session.PublishEventPubrec {
						pause()
					}
				}
			}
			s.SetPublishEventHandler(handler)
			done := make(chan error, 1)
			go func() { done <- s.PacketReceived(eventAck(packets.PUBREC, pub.PacketID, 0), nil) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				close(resume)
				t.Fatal("transition callback did not start")
			}
			zero := uint32(0)
			require.NoError(t, s.ConnectionLost(&packets.Disconnect{Properties: &packets.Properties{SessionExpiryInterval: &zero}}))
			close(resume)
			require.ErrorIs(t, <-done, session.ErrNoConnection)
			ids, err := store.List()
			require.NoError(t, err)
			assert.Empty(t, ids, "callback must not restore a packet after the session was discarded")
			assert.Equal(t, 1, store.puts)

		})
	}
}

// TestPublishEventPubrelStoreFailure verifies that a failed PUBREL write preserves the stored PUBLISH and prevents
// PUBREL transmission.
func TestPublishEventPubrelStoreFailure(t *testing.T) {
	store := &eventTestStore{storer: memory.New()}
	s := New(store, memory.New())
	defer s.Close()
	var wire bytes.Buffer
	eventConnect(t, s, &wire, false)
	pub := eventPublish(2)
	_, err := s.AddToSession(t.Context(), pub)
	require.NoError(t, err)
	s.SetPublishEventHandler(session.PublishEventHandler{ReadStoredPacket: true, PreparePubrel: func(e session.PubrelPreparation) {
		e.Outgoing.Properties.User = slices.Clone(e.Source.Properties.User)
	}})
	store.putErr = errors.New("disk full")
	require.ErrorIs(t, s.PacketReceived(eventAck(packets.PUBREC, pub.PacketID, 0), nil), store.putErr)
	assert.Zero(t, wire.Len(), "must not transmit PUBREL before it is persisted")
	assert.Equal(t, byte(packets.PUBLISH), eventRead(t, store, pub.PacketID).Type)
}

// TestPublishCallbacksIndependent verifies independent callback registration, preparation before notification, shared
// source reads, and one-time PUBREL preparation despite repeated PUBRECs.
func TestPublishCallbacksIndependent(t *testing.T) {
	for _, prepare := range []bool{false, true} {
		for _, observe := range []bool{false, true} {
			for _, read := range []bool{false, true} {
				t.Run(fmt.Sprintf("prepare=%t/observe=%t/read=%t", prepare, observe, read), func(t *testing.T) {
					store := &eventTestStore{storer: memory.New()}
					s := New(store, memory.New())
					defer s.Close()
					eventConnect(t, s, io.Discard, false)
					prepared, sent, observed := 0, 0, 0
					var preparedSource *packets.Publish
					handler := session.PublishEventHandler{
						ReadStoredPacket: read,
						BeforeSend: func(e session.PublishSend) {
							assertEventUnlocked(t, s)
							sent++
							assert.Equal(t, byte(packets.PUBREL), e.Source.Type)
						},
					}
					if prepare {
						handler.PreparePubrel = func(e session.PubrelPreparation) {
							assertEventUnlocked(t, s)
							prepared++
							assert.Equal(t, read, e.Source != nil)
							assert.Equal(t, e.Trigger.PacketID, e.Outgoing.PacketID)
							preparedSource = e.Source
							if e.Source != nil {
								e.Outgoing.Properties.User = slices.Clone(e.Source.Properties.User)
							}
						}
					}
					if observe {
						handler.OnEvent = func(e session.PublishEvent) {
							assertEventUnlocked(t, s)
							observed++
							assert.NoError(t, e.Err)
							assert.Equal(t, read, e.Source != nil)
							if observed == 1 && prepare {
								assert.Equal(t, 1, prepared, "preparation precedes notification")
								if read {
									assert.Same(t, preparedSource, e.Source.Content, "both callbacks share one read")
								}
							}
						}
					}
					s.SetPublishEventHandler(handler)
					pub := eventPublish(2)
					_, err := s.AddToSession(t.Context(), pub)
					require.NoError(t, err)
					rec := eventAck(packets.PUBREC, pub.PacketID, 0)
					require.NoError(t, s.PacketReceived(rec, nil))
					require.NoError(t, s.PacketReceived(rec, nil))
					assert.Equal(t, 2, sent)
					assert.Equal(t, 2, store.puts, "repeat PUBREC must preserve the stored PUBREL")
					if prepare && read {
						assert.Equal(t, pub.Properties.User, eventUsers(eventRead(t, store.storer, pub.PacketID)))
					}
					require.NoError(t, s.PacketReceived(eventAck(packets.PUBCOMP, pub.PacketID, 0), nil))
					wantPrepared, wantObserved, wantReads := 0, 0, 1 // Repeated PUBREC reads the stored PUBREL.
					if prepare {
						wantPrepared = 1
						if read {
							wantReads++
						}
					}
					if observe {
						wantObserved = 3
						if read {
							wantReads = 3
						}
					}
					assert.Equal(t, wantPrepared, prepared)
					assert.Equal(t, wantObserved, observed)
					assert.Equal(t, wantReads, store.gets)
				})
			}
		}
	}
}

// TestPreparePubrelSkippedOnReadFailureOrRejection verifies that preparation alone never runs after a source read
// failure or rejection, and rejection completes without requiring a source read.
func TestPreparePubrelSkippedOnReadFailureOrRejection(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprintf("reject=%t", reject), func(t *testing.T) {
			store := &eventTestStore{storer: memory.New()}
			s := New(store, memory.New())
			defer s.Close()
			eventConnect(t, s, io.Discard, false)
			s.SetPublishEventHandler(session.PublishEventHandler{
				ReadStoredPacket: true,
				PreparePubrel:    func(session.PubrelPreparation) { t.Error("must not prepare PUBREL") },
			})
			pub := eventPublish(2)
			response, err := s.AddToSession(t.Context(), pub)
			require.NoError(t, err)
			store.readErr = errors.New("store unavailable")
			reason := byte(0)
			if reject {
				reason = 0x80
			}
			err = s.PacketReceived(eventAck(packets.PUBREC, pub.PacketID, reason), nil)
			if reject {
				require.NoError(t, err)
				assert.Equal(t, byte(packets.PUBREC), (<-response).Type)
				assert.Zero(t, store.gets, "preparation alone does not read rejected packets")
			} else {
				require.ErrorIs(t, err, store.readErr)
				assert.Equal(t, byte(packets.PUBLISH), eventRead(t, store.storer, pub.PacketID).Type)
			}
			assert.Equal(t, 1, store.puts)
		})
	}
}

// TestPublishSendCopiesOnlyForBeforeSend verifies that wire copies are created only for BeforeSend and share their
// properties until the callback explicitly copies them before editing User Properties.
func TestPublishSendCopiesOnlyForBeforeSend(t *testing.T) {
	for _, source := range []*packets.ControlPacket{
		eventPublish(1).ToControlPacket(),
		{FixedHeader: packets.FixedHeader{Type: packets.PUBREL, Flags: 2}, Content: &packets.Pubrel{
			PacketID: 7, Properties: &packets.Properties{User: []packets.User{{Key: "app-id", Value: "order/42"}}},
		}},
	} {
		t.Run(source.PacketType(), func(t *testing.T) {
			handler := session.PublishEventHandler{
				OnEvent:       func(session.PublishEvent) { t.Error("send must not notify OnEvent") },
				PreparePubrel: func(session.PubrelPreparation) { t.Error("send must not prepare PUBREL") },
			}
			assert.Same(t, source, handler.PrepareSend(source), "no wire copy without BeforeSend")
			handler.BeforeSend = func(e session.PublishSend) {
				assert.Same(t, source, e.Source)
				assert.NotSame(t, source, e.Outgoing)
				// Copy both levels before editing User Properties in place.
				var props **packets.Properties
				switch p := e.Outgoing.Content.(type) {
				case *packets.Publish:
					assert.Same(t, e.Source.Content.(*packets.Publish).Properties, p.Properties)
					props = &p.Properties
				case *packets.Pubrel:
					assert.Same(t, e.Source.Content.(*packets.Pubrel).Properties, p.Properties)
					props = &p.Properties
				}
				copyProps := **props
				assert.Same(t, &eventUsers(e.Source)[0], &copyProps.User[0])
				copyProps.User = slices.Clone(copyProps.User)
				*props = &copyProps
				eventUsers(e.Outgoing)[0].Value = "wire-only"
			}
			wire := handler.PrepareSend(source)
			assert.Equal(t, "wire-only", eventUsers(wire)[0].Value)
			assert.Equal(t, "order/42", eventUsers(source)[0].Value)
		})
	}
}

// TestPublishSendReplacementDataSurvivesReconnect verifies that callback-owned payload and property replacements reach
// the wire on initial sends and reconnects without changing the caller's packet or persisted publication.
func TestPublishSendReplacementDataSurvivesReconnect(t *testing.T) {
	for _, qos := range []byte{1, 2} {
		t.Run(fmt.Sprintf("qos%d", qos), func(t *testing.T) {
			store := memory.New()
			s := New(store, memory.New())
			defer s.Close()
			pub := eventPublish(qos)
			format := byte(0)
			pub.Properties.PayloadFormat = &format
			pub.Properties.CorrelationData = []byte("correlation")
			sends := 0
			handler := session.PublishEventHandler{BeforeSend: func(e session.PublishSend) {
				assertEventUnlocked(t, s)
				sends++
				src := e.Source.Content.(*packets.Publish)
				out := e.Outgoing.Content.(*packets.Publish)
				assert.Equal(t, "events/test", src.Topic)
				assert.Equal(t, "payload", string(src.Payload))
				assert.Nil(t, src.Properties.TopicAlias)
				assert.Zero(t, *src.Properties.PayloadFormat)
				assert.Equal(t, "correlation", string(src.Properties.CorrelationData))

				// All referenced data remains shared until explicitly copied.
				assert.Same(t, src.Properties, out.Properties)
				assert.Same(t, &src.Payload[0], &out.Payload[0])
				assert.Same(t, src.Properties.PayloadFormat, out.Properties.PayloadFormat)
				assert.Same(t, &src.Properties.CorrelationData[0], &out.Properties.CorrelationData[0])
				props := *out.Properties
				out.Properties = &props
				out.Payload = slices.Clone(out.Payload)
				out.Payload[0] = 'P'
				out.Properties.CorrelationData = slices.Clone(out.Properties.CorrelationData)
				out.Properties.CorrelationData[0] = 'C'
				wireFormat := byte(1)
				out.Properties.PayloadFormat = &wireFormat
				// Establish a fresh alias with the full topic on each connection.
				alias := uint16(sends)
				out.Properties.TopicAlias = &alias
			}}
			s.SetPublishEventHandler(handler)
			var wire bytes.Buffer
			eventConnect(t, s, &wire, false)
			_, err := s.AddToSession(t.Context(), pub)
			require.NoError(t, err)
			_, err = handler.PrepareSend(pub.ToControlPacket()).WriteTo(&wire)
			require.NoError(t, err)
			for attempt := 1; attempt <= 2; attempt++ {
				if attempt == 2 {
					require.NoError(t, s.ConnectionLost(nil))
					eventConnect(t, s, &wire, true)
				}
				cp, err := packets.ReadPacket(&wire)
				require.NoError(t, err)
				got := cp.Content.(*packets.Publish)
				assert.Equal(t, "events/test", got.Topic)
				assert.Equal(t, uint16(attempt), *got.Properties.TopicAlias)
				assert.Equal(t, "Payload", string(got.Payload))
				assert.Equal(t, "Correlation", string(got.Properties.CorrelationData))
				assert.Equal(t, byte(1), *got.Properties.PayloadFormat)
				assert.Equal(t, pub.PacketID, got.PacketID)
				assert.Equal(t, qos, got.QoS)
				assert.Equal(t, attempt == 2, got.Duplicate)
				assert.Equal(t, pub, eventRead(t, store, pub.PacketID).Content,
					"wire edits must not change the stored publication")
			}
			assert.Equal(t, 2, sends)
			assert.Equal(t, "payload", string(pub.Payload))
			assert.Equal(t, "correlation", string(pub.Properties.CorrelationData))
			assert.Zero(t, format)
		})
	}
}

// TestPublishSendReplaceTopicAndProperties verifies that assigning a wire-only topic representation and a new
// properties struct leaves the source intact.
func TestPublishSendReplaceTopicAndProperties(t *testing.T) {
	pub := eventPublish(1)
	originalProperties := pub.Properties
	handler := session.PublishEventHandler{BeforeSend: func(e session.PublishSend) {
		out := e.Outgoing.Content.(*packets.Publish)
		// For a topic alias already established on this connection, the wire
		// packet can omit the topic while the source retains it for recovery.
		out.Topic = ""
		alias := uint16(7)
		out.Properties = &packets.Properties{TopicAlias: &alias}
	}}
	out := handler.PrepareSend(pub.ToControlPacket()).Content.(*packets.Publish)
	assert.Empty(t, out.Topic)
	assert.Equal(t, uint16(7), *out.Properties.TopicAlias)
	assert.Equal(t, "events/test", pub.Topic)
	assert.Same(t, originalProperties, pub.Properties)
	assert.Nil(t, pub.Properties.TopicAlias)
	assert.Contains(t, pub.Properties.User, packets.User{Key: "app-id", Value: "order/42"})
}

// TestPubrelPropertiesPersistAndAllowWireReplacement verifies that repeated PUBRECs preserve prepared PUBREL properties
// before and after state recreation, independently of callback source reads and without an OnEvent observer.
func TestPubrelPropertiesPersistAndAllowWireReplacement(t *testing.T) {
	for _, read := range []bool{false, true} {
		t.Run(fmt.Sprintf("read=%t", read), func(t *testing.T) {
			store := &eventTestStore{storer: memory.New()}
			s := New(store, memory.New())
			defer func() { _ = s.Close() }()
			prepared, sent := 0, 0
			handler := session.PublishEventHandler{
				ReadStoredPacket: read,
				PreparePubrel: func(e session.PubrelPreparation) {
					assertEventUnlocked(t, s)
					prepared++
					assert.Equal(t, read, e.Source != nil)
					e.Outgoing.Properties = &packets.Properties{
						ReasonString: "stored reason",
						User:         []packets.User{{Key: "app-id", Value: "pubrel metadata"}},
					}
					if e.Source != nil {
						e.Outgoing.Properties.User = slices.Clone(e.Source.Properties.User)
						e.Outgoing.Properties.User[0].Value = "pubrel metadata"
						assert.Equal(t, "order/42", e.Source.Properties.User[0].Value)
					}
				},
				BeforeSend: func(e session.PublishSend) {
					assertEventUnlocked(t, s)
					sent++
					src := e.Source.Content.(*packets.Pubrel)
					assert.Equal(t, "stored reason", src.Properties.ReasonString)
					out := e.Outgoing.Content.(*packets.Pubrel)
					props := *out.Properties
					props.ReasonString = "wire reason"
					out.Properties = &props
				},
			}
			s.SetPublishEventHandler(handler)
			var wire bytes.Buffer
			eventConnect(t, s, &wire, false)
			pub := eventPublish(2)
			_, err := s.AddToSession(t.Context(), pub)
			require.NoError(t, err)
			for attempt := 1; attempt <= 4; attempt++ {
				if attempt == 3 {
					// Recreate State so recovery cannot rely on the original preparation or an in-memory PUBREL.
					require.NoError(t, s.Close())
					s = New(store, memory.New())
					handler.PreparePubrel = nil
					s.SetPublishEventHandler(handler)
					eventConnect(t, s, &wire, true)
				} else {
					require.NoError(t, s.PacketReceived(eventAck(packets.PUBREC, pub.PacketID, 0), nil))
				}
				cp, err := packets.ReadPacket(&wire)
				require.NoError(t, err)
				got := cp.Content.(*packets.Pubrel)
				assert.Equal(t, pub.PacketID, got.PacketID)
				assert.Zero(t, got.ReasonCode)
				assert.Equal(t, "wire reason", got.Properties.ReasonString)
				require.NotEmpty(t, got.Properties.User)
				assert.Equal(t, "pubrel metadata", got.Properties.User[0].Value)
				stored := eventRead(t, store, pub.PacketID).Content.(*packets.Pubrel)
				assert.Equal(t, "stored reason", stored.Properties.ReasonString)
				assert.Equal(t, "pubrel metadata", stored.Properties.User[0].Value)
			}
			assert.Equal(t, 1, prepared)
			assert.Equal(t, 4, sent)
			assert.Equal(t, 2, store.puts, "repeated PUBREC must not overwrite the stored PUBREL")
		})
	}
}

// TestRepeatedPubrecReadFailure verifies that a stored PUBREL cannot be replaced by an empty response when its read
// fails, even when callback source reads are disabled. The stored transaction must remain available for retry.
func TestRepeatedPubrecReadFailure(t *testing.T) {
	store := &eventTestStore{storer: memory.New()}
	s := New(store, memory.New())
	defer s.Close()
	var wire bytes.Buffer
	eventConnect(t, s, &wire, false)
	pub := eventPublish(2)
	response, err := s.AddToSession(t.Context(), pub)
	require.NoError(t, err)
	rec := eventAck(packets.PUBREC, pub.PacketID, 0)
	require.NoError(t, s.PacketReceived(rec, nil))
	wire.Reset()
	store.readErr = errors.New("PUBREL read failed")
	observed := 0
	s.SetPublishEventHandler(session.PublishEventHandler{
		BeforeSend: func(session.PublishSend) { t.Error("must not transmit an unreadable PUBREL") },
		OnEvent: func(e session.PublishEvent) {
			assertEventUnlocked(t, s)
			observed++
			assert.Nil(t, e.Source, "callback source reads are disabled")
			assert.ErrorIs(t, e.Err, store.readErr)
		},
	})
	require.ErrorIs(t, s.PacketReceived(rec, nil), store.readErr)
	assert.Equal(t, 1, observed)
	assert.Zero(t, wire.Len())
	assert.Equal(t, 2, store.puts)
	assert.Equal(t, byte(packets.PUBREL), eventRead(t, store.storer, pub.PacketID).Type)
	select {
	case <-response:
		t.Fatal("read failure must not complete the transaction")
	default:
	}
}

// TestPublishSendLeavesNilPropertiesForCallback verifies that PrepareSend leaves nil Properties untouched and that
// callbacks can allocate wire-only properties without changing the source packet.
func TestPublishSendLeavesNilPropertiesForCallback(t *testing.T) {
	for _, source := range []*packets.ControlPacket{
		(&packets.Publish{Topic: "events/test", QoS: 1, PacketID: 7}).ToControlPacket(),
		{FixedHeader: packets.FixedHeader{Type: packets.PUBREL, Flags: 2}, Content: &packets.Pubrel{PacketID: 7}},
	} {
		t.Run(source.PacketType(), func(t *testing.T) {
			handler := session.PublishEventHandler{BeforeSend: func(e session.PublishSend) {
				props := &packets.Properties{User: []packets.User{{Key: "wire", Value: "only"}}}
				switch out := e.Outgoing.Content.(type) {
				case *packets.Publish:
					assert.Nil(t, out.Properties)
					out.Properties = props
				case *packets.Pubrel:
					assert.Nil(t, out.Properties)
					out.Properties = props
				}
			}}
			var wire bytes.Buffer
			_, err := handler.PrepareSend(source).WriteTo(&wire)
			require.NoError(t, err)
			got, err := packets.ReadPacket(&wire)
			require.NoError(t, err)
			assert.Equal(t, []packets.User{{Key: "wire", Value: "only"}}, eventUsers(got))
			switch src := source.Content.(type) {
			case *packets.Publish:
				assert.Nil(t, src.Properties)
			case *packets.Pubrel:
				assert.Nil(t, src.Properties)
			}
		})
	}
}
