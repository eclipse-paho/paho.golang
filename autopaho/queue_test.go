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

package autopaho

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho/queue"
	memqueue "github.com/eclipse/paho.golang/autopaho/queue/memory"
	"github.com/eclipse/paho.golang/internal/testserver"
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	paholog "github.com/eclipse/paho.golang/paho/log"
	"github.com/eclipse/paho.golang/paho/session/state"
	memstore "github.com/eclipse/paho.golang/paho/store/memory"
)

//
// This file contains tests that focus on confirming that queued messages are delivered
//

// TestQueuedMessages attempts to send 100 messages before the connection comes up and then 100 more afterwards (with
// disconnects during the process).
func TestQueuedMessages(t *testing.T) {
	t.Parallel()
	server, _ := url.Parse(dummyURL)
	serverLogger := paholog.NewTestLogger(t, "testServer:")
	logger := paholog.NewTestLogger(t, "test:")

	ts := testserver.New(serverLogger)
	got200Messages := make(chan struct{}) // Closed when 200 messages received
	var receivedPublish []*packets.Publish
	lastDup := 0 // Position in receivedPublish of the last duplicate we received

	ts.SetPacketReceivedCallback(func(cp *packets.ControlPacket) error {
		pub, ok := cp.Content.(*packets.Publish)
		if !ok {
			return nil
		}
		if pub.Duplicate { // Ignore duplicates if we have received them previously and they are in order
			for i, msg := range receivedPublish {
				if bytes.Compare(msg.Payload, pub.Payload) == 0 {
					if i >= lastDup {
						lastDup = i // ignoring i<lastDup means out of order messages will result in out of order receivedPublish
						return nil
					}
				}
			}
		}
		receivedPublish = append(receivedPublish, pub)
		if l := len(receivedPublish); l == 200 {
			close(got200Messages)
		} else if l%20 == 0 {
			return fmt.Errorf("disconnecting every 20 messages") // Test interaction of queue and store
		}
		return nil
	})

	// We expect messages
	pahoConnUpChan := make(chan struct{}) // Closed first time autopaho reports connection is up

	var allowConnection atomic.Bool

	// Add a corrupt item to the queue (zero bytes) - this should be logged and ignored
	q := memqueue.New()
	if _, err := q.Enqueue(bytes.NewReader(nil)); err != nil {
		t.Fatalf("failed to add corrupt zero byte item to queue")
	}

	// custom session because we don't want the client to close it when the connection is lost
	var tsDone chan struct{} // Set on AttemptConnection and closed when that test server connection is done
	clientStore := memstore.New()
	serverStore := memstore.New()
	session := state.New(clientStore, serverStore)
	session.SetErrorLogger(paholog.NewTestLogger(t, "sessionError:"))
	session.SetDebugLogger(paholog.NewTestLogger(t, "sessionDebug:"))
	defer session.Close()

	// Add a corrupt item to the store (zero bytes) - this should be logged and ignored
	clientStore.Put(1, packets.PUBLISH, bytes.NewReader(nil))
	serverStore.Put(1, packets.PUBREC, bytes.NewReader(nil))

	connectCount := 0
	config := ClientConfig{
		ServerUrls:       []*url.URL{server},
		KeepAlive:        60,
		ReconnectBackoff: NewConstantBackoff(500 * time.Millisecond), // Retry connection very quickly!
		ConnectTimeout:   shortDelay,                                 // Connection should come up very quickly
		Queue:            q,
		AttemptConnection: func(ctx context.Context, _ ClientConfig, _ *url.URL) (net.Conn, error) {
			if !allowConnection.Load() {
				return nil, fmt.Errorf("some random error")
			}
			var conn net.Conn
			var err error
			conn, tsDone, err = ts.Connect(ctx)
			return conn, err
		},
		OnConnectionUp: func(*ConnectionManager, *paho.Connack) {
			connectCount++
			if connectCount == 1 {
				close(pahoConnUpChan)
			}
		},
		Debug:                         logger,
		Errors:                        logger,
		PahoDebug:                     logger,
		PahoErrors:                    logger,
		CleanStartOnInitialConnection: false, // Want session to stay up (this is the default)
		SessionExpiryInterval:         600,   // If 0 then the state will be removed when the connection drops
		ClientConfig: paho.ClientConfig{
			ClientID: "test",
			Session:  session,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){ // Noop handler
				func(pr paho.PublishReceived) (bool, error) {
					return true, nil
				}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), longestDelay)
	defer cancel()
	cm, err := NewConnection(ctx, config)
	if err != nil {
		t.Fatalf("expected NewConnection success: %s", err)
	}

	testFmt := "Test%d"
	// Start with an invalid message; this will be queued but then rejected by paho.PublishWithOptions, and it's
	// important that it's discarded (otherwise it would be retried continually) - issue #214
	if err = cm.PublishViaQueue(ctx, &QueuePublish{
		Publish: &paho.Publish{
			QoS:        3,
			Topic:      fmt.Sprintf(testFmt, 0),
			Properties: nil,
			Payload:    []byte(fmt.Sprintf(testFmt, 0)),
		},
	}); err != nil {
		t.Fatalf("invalid publish failed")
	}

	// Transmit first 100 messages (should go into queue)
	for i := 1; i <= 100; i++ {
		msg := fmt.Sprintf(testFmt, i)
		if err = cm.PublishViaQueue(ctx, &QueuePublish{
			Publish: &paho.Publish{
				QoS:        1,
				Topic:      msg,
				Properties: nil,
				Payload:    []byte(msg),
			},
		}); err != nil {
			t.Fatalf("publish %d failed", i)
		}
		time.Sleep(time.Millisecond) // for logging
	}

	// Allow connection to come up and wait for this to happen
	allowConnection.Store(true)
	select {
	case <-pahoConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting connection up")
	}

	// Transmit another 100 messages
	for i := 101; i <= 200; i++ {
		msg := fmt.Sprintf(testFmt, i)
		if err = cm.PublishViaQueue(ctx, &QueuePublish{
			Publish: &paho.Publish{
				QoS:        1,
				Topic:      msg,
				Properties: nil,
				Payload:    []byte(msg),
			},
		}); err != nil {
			t.Fatalf("publish %d failed", i)
		}
	}

	// Wait for all messages to be received
	select {
	case <-got200Messages:
	case <-time.After(longestDelay):
		t.Fatalf("%s: timeout awaiting messages (got %d) ", time.Now(), len(receivedPublish)) // this is a data race but delay should prevent issues
	}

	// Disconnect
	disconnectErr := make(chan error)
	go func() {
		disconnectErr <- cm.Disconnect(ctx)
	}()
	select {
	case err = <-disconnectErr:
		if err != nil {
			t.Fatalf("Disconnect returned error: %s", err)
		}
	case <-time.After(longerDelay):
		t.Fatal("Disconnect should return relatively quickly")
	}

	// Connection manager should be Done
	select {
	case <-cm.Done():
	case <-time.After(shortDelay):
		t.Fatal("connection manager should be done after Disconnect Called")
	}

	// The test server should have picked up the dropped connection
	select {
	case <-tsDone:
	case <-time.After(shortDelay):
		t.Fatal("test server did not shutdown within expected time")
	}

	// Check that the queue is empty
	if _, err := cm.queue.Peek(); !errors.Is(err, queue.ErrEmpty) {
		t.Error("queue should be empty")
	}

	// Check that we received the expected messages, in the expected order
	for i := 1; i <= 200; i++ {
		exp := fmt.Sprintf(testFmt, i)
		if string(receivedPublish[i-1].Payload) != exp {
			t.Errorf("expected %s, got %s", exp, receivedPublish[i-1])
		}
	}
}

// TestPreloadPublish begin connection with PUBLISH packets in queue and a slow server
// Replicates issue #196 - this was caused by the wait for receive maximum slot taking
// longer than paho.PacketTimeout
func TestPreloadPublish(t *testing.T) {
	t.Parallel()

	// Bring up server
	server, _ := url.Parse(dummyURL)
	serverLogger := paholog.NewTestLogger(t, "testServer:")
	logger := paholog.NewTestLogger(t, "test:")

	ts := testserver.New(serverLogger)

	var publishReceived int32
	gotMessage := make(map[string]bool)
	got5Messages := make(chan struct{})

	ts.SetPacketReceivedCallback(func(cp *packets.ControlPacket) error {
		if cp.Type != packets.PUBLISH {
			return nil // Ignore packets other than PUBLISH
		}
		pub := cp.Content.(*packets.Publish)
		gotMessage[pub.Topic] = true
		if len(gotMessage) == 5 {
			gotMessage["z"] = true // stop the above from being true if called again
			close(got5Messages)
		}

		if publishReceived == 0 { // test server process in one go routine, so this will block all initial PUBLISH requests
			time.Sleep(shortDelay) // delay ack
		}
		publishReceived++
		return nil
	})
	ts.SetConnectCallback(func(cp *packets.Connect, cap *packets.Connack) {
		rm := uint16(2)
		cap.Properties.ReceiveMaximum = &rm
	})

	q := memqueue.New()
	for i := 0; i < 5; i++ {
		r, w := io.Pipe()

		go func() {
			publish := packets.Publish{
				Topic:   strconv.Itoa(i),
				Payload: []byte("packet: " + strconv.Itoa(i)),
				QoS:     1,
			}
			_, _ = publish.WriteTo(w)
			w.Close()
		}()

		if _, err := q.Enqueue(r); err != nil {
			t.Fatalf("failed to enqueue: %s", err)
		}
	}

	// custom session because we don't want the client to close it when the connection is lost
	var tsDone chan struct{} // Set on AttemptConnection and closed when that test server connection is done
	session := state.NewInMemory()
	session.SetErrorLogger(paholog.NewTestLogger(t, "sessionError:"))
	session.SetDebugLogger(paholog.NewTestLogger(t, "sessionDebug:"))
	defer session.Close()
	config := ClientConfig{
		ServerUrls:       []*url.URL{server},
		KeepAlive:        0,
		ReconnectBackoff: NewConstantBackoff(shortDelay), // Retry connection very quickly!
		ConnectTimeout:   shortDelay,                     // Connection should come up very quickly
		Queue:            q,
		AttemptConnection: func(ctx context.Context, _ ClientConfig, _ *url.URL) (net.Conn, error) {
			var conn net.Conn
			var err error
			conn, tsDone, err = ts.Connect(ctx)
			return conn, err
		},
		Debug:                         logger,
		PahoDebug:                     logger,
		PahoErrors:                    logger,
		CleanStartOnInitialConnection: false, // Want session to stay up (this is the default)
		SessionExpiryInterval:         600,   // If 0 then the state will be removed when the connection drops
		ClientConfig: paho.ClientConfig{
			ClientID: "test",
			Session:  session,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){ // Noop handler
				func(pr paho.PublishReceived) (bool, error) {
					return true, nil
				}},
			PacketTimeout: 250 * time.Millisecond, // test server should be able to respond very quickly!
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cm, err := NewConnection(ctx, config)
	if err != nil {
		t.Fatalf("expected NewConnection success: %s", err)
	}

	// Wait for all messages to be received
	select {
	case <-got5Messages:
	case <-time.After(5 * longerDelay): // Need a bit longer...
		t.Fatalf("timeout awaiting messages (received %d)", publishReceived)
	}

	// Disconnect
	disconnectErr := make(chan error)
	go func() {
		disconnectErr <- cm.Disconnect(ctx)
	}()
	select {
	case err = <-disconnectErr:
		if err != nil {
			t.Fatalf("Disconnect returned error: %s", err)
		}
	case <-time.After(longerDelay):
		t.Fatal("Disconnect should return relatively quickly")
	}

	// Connection manager should be Done
	select {
	case <-cm.Done():
	case <-time.After(shortDelay):
		t.Fatal("connection manager should be done after Disconnect Called")
	}

	// The test server should have picked up the dropped connection
	select {
	case <-tsDone:
	case <-time.After(shortDelay):
		t.Fatal("test server did not shutdown within expected time")
	}
}
