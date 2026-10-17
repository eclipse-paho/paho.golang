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
	"fmt"
	"net"
	"net/url"
	"slices"
	"testing"
	"time"

	filequeue "github.com/eclipse/paho.golang/autopaho/queue/file"
	"github.com/eclipse/paho.golang/internal/testserver"
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	paholog "github.com/eclipse/paho.golang/paho/log"
	"github.com/eclipse/paho.golang/paho/session"
	"github.com/eclipse/paho.golang/paho/session/state"
	filestore "github.com/eclipse/paho.golang/paho/store/file"
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
