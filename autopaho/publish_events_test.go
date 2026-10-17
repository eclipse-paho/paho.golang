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

package autopaho

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho/queue"
	filequeue "github.com/eclipse/paho.golang/autopaho/queue/file"
	memqueue "github.com/eclipse/paho.golang/autopaho/queue/memory"
	"github.com/eclipse/paho.golang/internal/testserver"
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	paholog "github.com/eclipse/paho.golang/paho/log"
	"github.com/eclipse/paho.golang/paho/session"
	"github.com/eclipse/paho.golang/paho/session/state"
	filestore "github.com/eclipse/paho.golang/paho/store/file"
	memstore "github.com/eclipse/paho.golang/paho/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueuedPublishEventsRestored exercises queue -> PUBLISH -> PUBREL -> callback, starting with a reopened disk queue
// and checking the actual wire packets.
func TestQueuedPublishEventsRestored(t *testing.T) {
	// Create a queue and add two records
	dir := t.TempDir()
	q, err := filequeue.New(dir, "queue-", ".msg")
	require.NoError(t, err)
	offline := &ConnectionManager{queue: q}
	for _, qos := range []byte{1, 2} {
		require.NoError(t, offline.PublishViaQueue(t.Context(), &QueuePublish{Publish: &paho.Publish{
			QoS: qos, Topic: "events/queue", Payload: []byte("body"),
			Properties: &paho.PublishProperties{User: paho.UserProperties{
				{Key: "app-id", Value: fmt.Sprintf("queued-%d", qos)}, // will be filtered in BeforeSend
				{Key: "public", Value: "keep"},                        // Should be retained (and sent to test broker)
			}},
		}}))
	}
	// We now recreate the queue (effectively throwing away the old one) so everything is read from disk
	q, err = filequeue.New(dir, "queue-", ".msg")
	require.NoError(t, err)
	clientStore, err := filestore.New(dir, "client-", ".pkt")
	require.NoError(t, err)
	serverStore, err := filestore.New(dir, "server-", ".pkt")
	require.NoError(t, err)
	s := state.New(clientStore, serverStore)
	t.Cleanup(func() { _ = s.Close() })

	wire := make(chan byte, 3)
	ts := testserver.New(paholog.NewTestLogger(t, "broker:"))
	ts.SetPacketReceivedCallback(func(cp *packets.ControlPacket) error {
		var users []packets.User
		switch p := cp.Content.(type) {
		case *packets.Publish:
			users = p.Properties.User
			assert.Contains(t, users, packets.User{Key: "public", Value: "keep"})
		case *packets.Pubrel:
			users = p.Properties.User
		default:
			return nil
		}
		// Ensure filtered property does not reach broker
		for _, u := range users {
			assert.NotEqual(t, "app-id", u.Key, "local ID reached broker")
		}
		wire <- cp.Type
		return nil
	})
	type completion struct {
		id      string
		trigger byte
	}
	completed := make(chan completion, 2)
	handler := session.PublishEventHandler{
		ReadStoredPacket: true,
		BeforeSend: func(e session.PublishSend) {
			filter := func(u packets.User) bool { return u.Key == "app-id" }
			switch p := e.Outgoing.Content.(type) {
			case *packets.Publish:
				props := *p.Properties
				props.User = slices.Clone(props.User)
				p.Properties = &props
				p.Properties.User = slices.DeleteFunc(p.Properties.User, filter)
			case *packets.Pubrel:
				props := *p.Properties
				props.User = slices.Clone(props.User)
				p.Properties = &props
				p.Properties.User = slices.DeleteFunc(p.Properties.User, filter)
			}
		},
		PreparePubrel: func(e session.PubrelPreparation) {
			for _, u := range e.Source.Properties.User {
				if u.Key == "app-id" {
					e.Outgoing.Properties.User = append(e.Outgoing.Properties.User, u)
				}
			}
		},
		OnEvent: func(e session.PublishEvent) {
			assert.NoError(t, e.Err)
			switch e.Type {
			case session.PublishEventComplete:
				var users []packets.User
				switch p := e.Source.Content.(type) {
				case *packets.Publish:
					users = p.Properties.User
				case *packets.Pubrel:
					users = p.Properties.User
				}
				for _, u := range users {
					if u.Key == "app-id" {
						completed <- completion{u.Value, e.Trigger.Type}
					}
				}
			}
		},
	}
	// Connect to the test server (note that dummyURL is not used because we provide AttemptConnection)
	serverURL, err := url.Parse(dummyURL)
	require.NoError(t, err)
	serverDone := make(chan chan struct{}, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cm, err := NewConnection(ctx, ClientConfig{
		ServerUrls: []*url.URL{serverURL}, Queue: q, SessionExpiryInterval: 600,
		AttemptConnection: func(ctx context.Context, _ ClientConfig, _ *url.URL) (net.Conn, error) {
			conn, done, err := ts.Connect(ctx)
			if err == nil {
				serverDone <- done
			}
			return conn, err
		},
		ClientConfig: paho.ClientConfig{ClientID: "events", Session: s, PublishEvents: handler},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		select {
		case <-cm.Done():
		case <-time.After(time.Second):
			t.Error("connection manager did not stop")
		}
		select {
		case done := <-serverDone:
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("test server did not stop")
			}
		default:
		}
	})
	// Expect delivery events for both PUBLISH requests
	got := map[string]byte{}
	for range 2 {
		select {
		case event := <-completed:

			got[event.id] = event.trigger
		case <-ctx.Done():

			t.Fatal("timed out waiting for publish events")
		}
	}
	assert.Equal(t, map[string]byte{"queued-1": packets.PUBACK, "queued-2": packets.PUBCOMP}, got)
	// Test broker should receive 3 packets (2 for the QOS2 message)
	for range 3 {
		select {
		case <-wire:
		case <-ctx.Done():

			t.Fatal("timed out waiting for wire packets")
		}
	}
	select {
	case <-q.WaitForEmpty():
	case <-ctx.Done():
		t.Fatal("queue did not drain")
	}
	ids, err := clientStore.List()
	require.NoError(t, err)
	assert.Empty(t, ids)
}

// TestQueuedPublishDiscarded checks the generic event contract, including optional source reads and queue removal errors.
func TestQueuedPublishDiscarded(t *testing.T) {
	for _, tc := range []struct {
		name        string
		readSource  bool
		failRemove  bool
		returnEmpty bool
		noObserver  bool
	}{
		{name: "source reads disabled"},
		{name: "source reads enabled", readSource: true},
		{name: "removal retried", failRemove: true},
		{name: "removal returns ErrEmpty", returnEmpty: true},
		{name: "no observer", noObserver: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			var notifications, removals int
			handler := session.PublishEventHandler{
				ReadStoredPacket: tc.readSource,
				BeforeSend:       func(session.PublishSend) { t.Error("rejected publication must not be sent") },
				PreparePubrel:    func(session.PubrelPreparation) { t.Error("rejected publication must not enter session") },
			}
			if !tc.noObserver {
				handler.OnEvent = func(e session.PublishEvent) {
					notifications++
					assert.Equal(t, session.PublishEventQueueDiscarded, e.Type)
					assert.ErrorIs(t, e.Err, paho.ErrInvalidArguments)
					assert.Zero(t, e.PacketID)
					assert.Nil(t, e.Trigger)
					if assert.NotNil(t, e.Source) {
						assert.Equal(t, byte(packets.PUBLISH), e.Source.Type)
						pub := e.Source.Content.(*packets.Publish)
						assert.Equal(t, byte(1), pub.QoS)
						assert.Equal(t, []byte("body"), pub.Payload)
						assert.Contains(t, pub.Properties.User, packets.User{Key: "app-id", Value: "rejected"})
					}
				}
			}
			q := queueWithRemoveCallback{Queue: memqueue.New(), remove: func(e queue.Entry) error {
				removals++
				assert.Zero(t, notifications, "retained entries must not report terminal outcomes")
				if tc.failRemove && removals == 1 {
					return errors.New("temporary queue removal failure")
				}
				require.NoError(t, e.Remove())
				cancel() // Stop the worker after it handles this successful removal.
				if tc.returnEmpty {
					return queue.ErrEmpty
				}
				return nil
			}}
			s := state.NewInMemory()
			defer s.Close()
			cfg := paho.ClientConfig{Session: s, PublishEvents: handler}
			connUp := make(chan struct{})
			close(connUp)
			cm := ConnectionManager{
				// Local validation happens before a connection or session transaction is needed.
				cli: paho.NewClient(cfg), cfg: ClientConfig{ClientConfig: cfg}, queue: q,
				connUp: connUp, connDown: make(chan struct{}),
				debug: paholog.NOOPLogger{}, errors: paholog.NOOPLogger{},
			}
			require.NoError(t, cm.PublishViaQueue(ctx, &QueuePublish{Publish: &paho.Publish{
				QoS: 1, Payload: []byte("body"), // A missing topic is rejected locally.
				Properties: &paho.PublishProperties{User: paho.UserProperties{{Key: "app-id", Value: "rejected"}}},
			}}))
			require.ErrorIs(t, cm.managePublishQueue(ctx), context.Canceled)
			wantRemovals := 1
			if tc.failRemove {
				wantRemovals++
			}
			assert.Equal(t, wantRemovals, removals)
			if tc.noObserver {
				assert.Zero(t, notifications)
			} else {
				assert.Equal(t, 1, notifications)
			}
			_, err := q.Peek()
			require.ErrorIs(t, err, queue.ErrEmpty)
		})
	}
}

// TestQueuedPublishStoredBeforeWriteFailure verifies that a network error after session acceptance removes the queue
// entry without reporting a terminal queue outcome. The stored publication remains available for retransmission.
func TestQueuedPublishStoredBeforeWriteFailure(t *testing.T) {
	for _, qos := range []byte{1, 2} {
		t.Run(fmt.Sprintf("qos%d", qos), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			conn, peer := testserver.NewConnPair()
			defer conn.Close()
			require.NoError(t, peer.Close()) // Every write fails, after the PUBLISH has been stored.
			store := memstore.New()
			s := state.New(store, memstore.New())
			defer s.Close()
			var attempts, removals int
			cfg := paho.ClientConfig{Conn: conn, Session: s, PublishEvents: session.PublishEventHandler{
				BeforeSend: func(session.PublishSend) { attempts++ },
				OnEvent:    func(e session.PublishEvent) { t.Errorf("unexpected event after session acceptance: %+v", e) },
			}}
			cli := paho.NewClient(cfg)
			expiry := uint32(600)
			require.NoError(t, s.ConAckReceived(conn,
				&packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}}, &packets.Connack{}))
			q := queueWithRemoveCallback{Queue: memqueue.New(), remove: func(e queue.Entry) error {
				removals++
				cancel()
				return e.Remove()
			}}
			connUp := make(chan struct{})
			close(connUp)
			cm := ConnectionManager{
				cli: cli, cfg: ClientConfig{ClientConfig: cfg}, queue: q,
				connUp: connUp, connDown: make(chan struct{}),
				debug: paholog.NOOPLogger{}, errors: paholog.NOOPLogger{},
			}
			require.NoError(t, cm.PublishViaQueue(ctx, &QueuePublish{Publish: &paho.Publish{
				QoS: qos, Topic: "events/stored", Payload: []byte("body"),
				Properties: &paho.PublishProperties{User: paho.UserProperties{{Key: "app-id", Value: "stored"}}},
			}}))
			require.ErrorIs(t, cm.managePublishQueue(ctx), context.Canceled)
			assert.Equal(t, 1, attempts)
			assert.Equal(t, 1, removals)
			_, err := q.Peek()
			require.ErrorIs(t, err, queue.ErrEmpty)
			ids, err := store.List()
			require.NoError(t, err)
			require.Len(t, ids, 1)
			r, err := store.Get(ids[0])
			require.NoError(t, err)
			defer r.Close()
			cp, err := packets.ReadPacket(r)
			require.NoError(t, err)
			pub := cp.Content.(*packets.Publish)
			assert.Equal(t, qos, pub.QoS)
			assert.NotZero(t, pub.PacketID)
			assert.Contains(t, pub.Properties.User, packets.User{Key: "app-id", Value: "stored"})
		})
	}
}

type queueWithRemoveCallback struct {
	queue.Queue
	remove func(queue.Entry) error
}

func (q queueWithRemoveCallback) Peek() (queue.Entry, error) {
	e, err := q.Queue.Peek()
	if err != nil {
		return nil, err
	}
	return entryWithRemoveCallback{Entry: e, remove: q.remove}, nil
}

type entryWithRemoveCallback struct {
	queue.Entry
	remove func(queue.Entry) error
}

func (e entryWithRemoveCallback) Remove() error {
	return e.remove(e.Entry)
}
