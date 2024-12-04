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
	"context"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/canonical/mqtt.golang/mock/testserver"
	"github.com/canonical/mqtt.golang/packets"
	"github.com/canonical/mqtt.golang/paho"
	paholog "github.com/canonical/mqtt.golang/paho/log"
	"github.com/canonical/mqtt.golang/paho/session/state"
)

//
// This file contains tests that focus on session state persistence and delivery of QOS1/2 messages.
//

// TestDisconnectAfterOutgoingPublish Confirms that a QOS1/2 Publish will be resent if the connection drops before
// the `PUBLISH` is acknowledged
func TestDisconnectAfterOutgoingPublish(t *testing.T) {
	t.Parallel()
	server, _ := url.Parse(dummyURL)
	serverLogger := paholog.NewTestLogger(t, "testServer:")
	logger := paholog.NewTestLogger(t, "test:")

	ts := testserver.New(serverLogger)

	// We will track the number of `PUBLISH` packets received at each QOS level. The test server will drop the connection
	// the first time a packet is received at each level
	receivedByQos := [3]int{}
	ts.SetPacketReceivedCallback(func(cp *packets.ControlPacket) error {
		pub, ok := cp.Content.(*packets.Publish)
		if !ok {
			return nil
		}
		receivedByQos[pub.QoS]++
		if receivedByQos[pub.QoS] == 1 {
			return fmt.Errorf("first message at QOS %d received, disconnecting", pub.QoS)
		}
		return nil
	})

	type tsConnUpMsg struct {
		cancelFn func()        // Function to cancel test server context
		done     chan struct{} // Will be closed when the test server has disconnected (and shutdown)
	}
	tsConnUpChan := make(chan tsConnUpMsg) // Message will be sent when test server connection is up
	var tsDone chan struct{}               // Set on AttemptConnection and closed when that test server connection is done
	pahoConnUpChan := make(chan struct{})  // When autopaho reports connection is up write to channel will occur

	// custom session because we don't want the client to close it when the connection is lost
	session := state.NewInMemory()
	session.SetErrorLogger(paholog.NewTestLogger(t, "sessionError:"))
	session.SetDebugLogger(paholog.NewTestLogger(t, "sessionDebug:"))
	defer session.Close()
	connectCount := 0
	config := ClientConfig{
		ServerUrls:       []*url.URL{server},
		KeepAlive:        60,
		ReconnectBackoff: NewConstantBackoff(time.Millisecond), // Retry connection very quickly!
		ConnectTimeout:   shortDelay,                           // Connection should come up very quickly
		AttemptConnection: func(ctx context.Context, _ ClientConfig, _ *url.URL) (net.Conn, error) {
			ctx, cancel := context.WithCancel(ctx) // Note: go vet warning is invalid
			conn, done, err := ts.Connect(ctx)
			if err == nil { // The above may fail if attempted too quickly (before disconnect processed)
				connectCount++
				if connectCount == 1 {
					tsConnUpChan <- tsConnUpMsg{cancelFn: cancel, done: done}
				}
			} else {
				logger.Println("connection attempt failed", err)
				cancel()
			}
			tsDone = done
			logger.Println("connection up")
			return conn, err
		},
		OnConnectionUp: func(*ConnectionManager, *paho.Connack) {
			if connectCount == 1 {
				pahoConnUpChan <- struct{}{}
			}
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
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), longerDelay)
	defer cancel()
	cm, err := NewConnection(ctx, config)
	if err != nil {
		t.Fatalf("expected NewConnection success: %s", err)
	}

	// Wait for connection to come up
	select {
	case <-tsConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting initial connection request")
	}
	select {
	case <-pahoConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting connection up")
	}

	// We send a QOS1 and QOS2 message; behind the scenes the callback will drop the connection ensuring that the
	// message is not acknowledged on the first attempt
	for qos := 1; qos <= 2; qos++ {
		testFmt := "Test%d"

		t.Logf("publish QOS %d message", qos)
		msg := fmt.Sprintf(testFmt, qos)
		pubResult := make(chan error)

		go func() {
			_, err := cm.Publish(ctx, &paho.Publish{
				QoS:        byte(qos),
				Topic:      msg,
				Properties: nil,
				Payload:    []byte(msg),
			})
			pubResult <- err
		}()

		// Wait for `Publish` to complete
		select {
		case err := <-pubResult:
			if err != nil {
				t.Fatalf("publish at QOS %d failed: %s", qos, err)
			}
		case <-time.After(longerDelay):
			t.Fatalf("publish at QOS %d did not complete in the time expected", qos)
		}
		t.Logf("publish QOS %d message complete", qos)
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

	// Confirm that both messages were sent twice (initial attempt then resent)
	for qos := 1; qos <= 2; qos++ {
		if receivedByQos[qos] != 2 {
			t.Errorf("expected 2 messages at QOS %d, got %d", qos, receivedByQos[qos])
		}
	}
}

// TestQueueResume Loads some messages into a queue and then connects (simulating a disk based queue containing messages
// at startup).
func TestQueueResume(t *testing.T) {
	t.Parallel()
	server, _ := url.Parse(dummyURL)
	serverLogger := paholog.NewTestLogger(t, "testServer:")
	logger := paholog.NewTestLogger(t, "test:")

	ts := testserver.New(serverLogger)

	// We will track the number of `PUBLISH` packets received at each QOS level. The test server will drop the connection
	// the first time a packet is received at each level
	receivedByQos := [3]int{}
	ts.SetPacketReceivedCallback(func(cp *packets.ControlPacket) error {
		pub, ok := cp.Content.(*packets.Publish)
		if !ok {
			return nil
		}
		receivedByQos[pub.QoS]++
		if receivedByQos[pub.QoS] == 1 {
			return fmt.Errorf("first message at QOS %d received, disconnecting", pub.QoS)
		}
		return nil
	})

	type tsConnUpMsg struct {
		cancelFn func()        // Function to cancel test server context
		done     chan struct{} // Will be closed when the test server has disconnected (and shutdown)
	}
	tsConnUpChan := make(chan tsConnUpMsg) // Message will be sent when test server connection is up
	var tsDone chan struct{}               // Set on AttemptConnection and closed when that test server connection is done
	pahoConnUpChan := make(chan struct{})  // When autopaho reports connection is up write to channel will occur

	// custom session because we don't want the client to close it when the connection is lost
	session := state.NewInMemory()
	session.SetErrorLogger(paholog.NewTestLogger(t, "sessionError:"))
	session.SetDebugLogger(paholog.NewTestLogger(t, "sessionDebug:"))
	defer session.Close()
	connectCount := 0
	config := ClientConfig{
		ServerUrls:       []*url.URL{server},
		KeepAlive:        60,
		ReconnectBackoff: NewConstantBackoff(time.Millisecond), // Retry connection very quickly!
		ConnectTimeout:   shortDelay,                           // Connection should come up very quickly
		AttemptConnection: func(ctx context.Context, _ ClientConfig, _ *url.URL) (net.Conn, error) {
			ctx, cancel := context.WithCancel(ctx) // Note: go vet warning is invalid
			conn, done, err := ts.Connect(ctx)
			if err == nil { // The above may fail if attempted too quickly (before disconnect processed)
				connectCount++
				if connectCount == 1 {
					tsConnUpChan <- tsConnUpMsg{cancelFn: cancel, done: done}
				}
			} else {
				logger.Println("connection attempt failed", err)
				cancel()
			}
			tsDone = done
			logger.Println("connection up")
			return conn, err
		},
		OnConnectionUp: func(*ConnectionManager, *paho.Connack) {
			if connectCount == 1 {
				pahoConnUpChan <- struct{}{}
			}
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
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), longerDelay)
	defer cancel()
	cm, err := NewConnection(ctx, config)
	if err != nil {
		t.Fatalf("expected NewConnection success: %s", err)
	}

	// Wait for connection to come up
	select {
	case <-tsConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting initial connection request")
	}
	select {
	case <-pahoConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting connection up")
	}

	// We send a QOS1 and QOS2 message; behind the scenes the callback will drop the connection ensuring that the
	// message is not acknowledged on the first attempt
	for qos := 1; qos <= 2; qos++ {
		testFmt := "Test%d"

		t.Logf("publish QOS %d message", qos)
		msg := fmt.Sprintf(testFmt, qos)
		pubResult := make(chan error)

		go func() {
			_, err := cm.Publish(ctx, &paho.Publish{
				QoS:        byte(qos),
				Topic:      msg,
				Properties: nil,
				Payload:    []byte(msg),
			})
			pubResult <- err
		}()

		// Wait for `Publish` to complete
		select {
		case err := <-pubResult:
			if err != nil {
				t.Fatalf("publish at QOS %d failed: %s", qos, err)
			}
		case <-time.After(longerDelay):
			t.Fatalf("publish at QOS %d did not complete in the time expected", qos)
		}
		t.Logf("publish QOS %d message complete", qos)
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

	// Confirm that both messages were sent twice (initial attempt then resent)
	for qos := 1; qos <= 2; qos++ {
		if receivedByQos[qos] != 2 {
			t.Errorf("expected 2 messages at QOS %d, got %d", qos, receivedByQos[qos])
		}
	}
}
