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

// build +unittest

package autopaho

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/canonical/mqtt.golang/internal/testserver"
	"github.com/canonical/mqtt.golang/packets"
	paholog "github.com/canonical/mqtt.golang/paho/log"
	"go.uber.org/goleak"

	"github.com/canonical/mqtt.golang/paho"
)

const shortDelay = 500 * time.Millisecond // Used when something should happen pretty quickly (increase when debugging)
const longerDelay = time.Second           // Longer delay than above (for things like test wide context timeout)

// When debugging uncomment the below (to prevent tests terminating too quickly)
// const shortDelay = time.Hour
// const longerDelay = time.Hour

const dummyURL = "tcp://127.0.0.1:1883"

// TestMain customised to verify no goroutine leaks (easy to introduce into the code!)
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestConnect confirms that the computed CONNECT packet is as anticipated
func TestConnect(t *testing.T) {

}

// TestDisconnect confirms that Disconnect closes the connection and exits cleanly
func TestDisconnect(t *testing.T) {
	t.Parallel()
	server, _ := url.Parse(dummyURL)
	serverLogger := paholog.NewTestLogger(t, "testServer:")
	logger := paholog.NewTestLogger(t, "test:")

	ts := testserver.New(serverLogger)

	type tsConnUpMsg struct {
		cancelFn func()        // Function to cancel test server context
		done     chan struct{} // Will be closed when the test server has disconnected (and shutdown)
	}
	var tsDone chan struct{}               // Set on AttemptConnection and closed when that test server connection is done
	tsConnUpChan := make(chan tsConnUpMsg) // Message will be sent when test server connection is up
	pahoConnUpChan := make(chan struct{})  // When autopaho reports connection is up write to channel will occur

	errCh := make(chan error, 2)
	config := ClientConfig{
		ServerUrls:       []*url.URL{server},
		KeepAlive:        60,
		ReconnectBackoff: NewConstantBackoff(time.Millisecond), // Retry connection very quickly!
		ConnectTimeout:   shortDelay,                           // Connection should come up very quickly
		AttemptConnection: func(ctx context.Context, _ ClientConfig, _ *url.URL) (net.Conn, error) {
			ctx, cancel := context.WithCancel(ctx)
			conn, done, err := ts.Connect(ctx)
			if err == nil { // The above may fail if attempted too quickly (before disconnect processed)
				tsConnUpChan <- tsConnUpMsg{cancelFn: cancel, done: done}
			} else {
				cancel()
			}
			tsDone = done
			return conn, err
		},
		OnConnectionUp: func(*ConnectionManager, *paho.Connack) { pahoConnUpChan <- struct{}{} },
		Debug:          logger,
		PahoDebug:      logger,
		PahoErrors:     logger,
		ClientConfig: paho.ClientConfig{
			ClientID: "test",
			OnServerDisconnect: func(disconnect *paho.Disconnect) {
				errCh <- fmt.Errorf("disconnect received: %v", disconnect)
			},
			OnClientError: func(err error) {
				errCh <- err
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm, err := NewConnection(ctx, config)
	if err != nil {
		t.Fatalf("expected NewConnection success: %s", err)
	}

	var connUpMsg tsConnUpMsg
	select {
	case connUpMsg = <-tsConnUpChan:
		defer connUpMsg.cancelFn()
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting initial connection request")
	}
	select {
	case <-pahoConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting connection up")
	}

	if !ts.Connected() {
		t.Fatal("test server should be connected")
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

	select {
	case err := <-errCh:
		t.Fatalf("callbacks should not be called on Disconnect: %s", err)
	default:
	}
}

// TestReconnect confirms that the connection is automatically re-established when lost
func TestReconnect(t *testing.T) {
	t.Parallel()
	server, _ := url.Parse(dummyURL)
	serverLogger := paholog.NewTestLogger(t, "testServer:")
	logger := paholog.NewTestLogger(t, "test:")

	ts := testserver.New(serverLogger)

	type tsConnUpMsg struct {
		cancelFn func()        // Function to cancel test server context
		done     chan struct{} // Will be closed when the test server has disconnected (and shutdown)
	}
	tsConnUpChan := make(chan tsConnUpMsg, 1) // Message will be sent when test server connection is up (buffered so we can detect unexpected attempts)
	pahoConnUpChan := make(chan struct{}, 1)  // When autopaho reports connection is up write to channel will occur

	atCount := 0

	// If we don't set the pinger, paho will recreate it each time; to confirm issue #277 does not reoccur we set it
	pinger := paho.NewDefaultPinger()
	pinger.SetDebug(paholog.NewTestLogger(t, "pinger:"))

	config := ClientConfig{
		ServerUrls:       []*url.URL{server},
		KeepAlive:        60,
		ReconnectBackoff: NewConstantBackoff(time.Millisecond), // Retry connection very quickly!
		ConnectTimeout:   shortDelay,                           // Connection should come up very quickly
		AttemptConnection: func(ctx context.Context, _ ClientConfig, _ *url.URL) (net.Conn, error) {
			atCount += 1
			if atCount == 2 { // fail on the initial reconnection attempt to exercise retry functionality
				return nil, errors.New("connection attempt failed")
			}
			ctx, cancel := context.WithCancel(ctx)
			conn, done, err := ts.Connect(ctx)
			if err == nil { // The above may fail if attempted too quickly (before disconnect processed)
				tsConnUpChan <- tsConnUpMsg{cancelFn: cancel, done: done}
			} else {
				cancel()
			}
			return conn, err
		},
		OnConnectionUp: func(*ConnectionManager, *paho.Connack) { pahoConnUpChan <- struct{}{} },
		Debug:          logger,
		PahoDebug:      logger,
		PahoErrors:     logger,
		ClientConfig: paho.ClientConfig{
			ClientID:    "test",
			PingHandler: pinger,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm, err := NewConnection(ctx, config)
	if err != nil {
		t.Fatalf("expected NewConnection success: %s", err)
	}

	var initialConnUpMsg tsConnUpMsg
	select {
	case initialConnUpMsg = <-tsConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting initial connection request")
	}
	select {
	case <-pahoConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting connection up")
	}

	// Force a disconnect
	initialConnUpMsg.cancelFn()
	select {
	case <-initialConnUpMsg.done:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting test server shutdown")
	}

	// Await reconnection
	var secondConnUpMsg tsConnUpMsg
	select {
	case secondConnUpMsg = <-tsConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting reconnection request")
	}
	select {
	case <-pahoConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting reconnection up")
	}

	// Ensure connection is stable (ref issue #227 where pinger caused connection to drop)
	select {
	case <-tsConnUpChan:
		t.Fatalf("connection should be stable after reconnection")
	case <-time.After(shortDelay):
	}

	// Clean shutdown
	cancel() // Cancelling outer context will cascade

	select {
	case <-secondConnUpMsg.done:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting second test server shutdown")
	}
	select {
	case <-cm.Done():
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting connection manager shutdown")
	}
}

// TestBasicPubSub performs pub/sub operations at each QOS level
func TestBasicPubSub(t *testing.T) {
	t.Parallel()
	server, _ := url.Parse(dummyURL)
	serverLogger := paholog.NewTestLogger(t, "testServer:")
	logger := paholog.NewTestLogger(t, "test:")
	ts := testserver.New(serverLogger)

	type tsConnUpMsg struct {
		cancelFn func()        // Function to cancel test server context
		done     chan struct{} // Will be closed when the test server has disconnected (and shutdown)
	}
	tsConnUpChan := make(chan tsConnUpMsg) // Message will be sent when test server connection is up
	pahoConnUpChan := make(chan struct{})  // When autopaho reports connection is up write to channel will occur

	const expectedMessages = 3
	var mrMu sync.Mutex
	mrDone := make(chan struct{}) // Closed when the expected messages have been received
	var messagesReceived []*paho.Publish

	atCount := 0

	config := ClientConfig{
		ServerUrls:       []*url.URL{server},
		KeepAlive:        60,
		ReconnectBackoff: NewConstantBackoff(time.Millisecond), // Retry connection very quickly!
		ConnectTimeout:   shortDelay,                           // Connection should come up very quickly
		AttemptConnection: func(ctx context.Context, _ ClientConfig, _ *url.URL) (net.Conn, error) {
			atCount += 1
			if atCount > 1 { // force failure if a reconnection is attempted (the connection should not drop in this test)
				return nil, errors.New("connection attempt failed")
			}
			ctx, cancel := context.WithCancel(ctx)
			conn, done, err := ts.Connect(ctx)
			if err == nil { // The above may fail if attempted too quickly (before disconnect processed)
				tsConnUpChan <- tsConnUpMsg{cancelFn: cancel, done: done}
			} else {
				cancel()
			}
			logger.Println("connection up")
			return conn, err
		},
		OnConnectionUp: func(*ConnectionManager, *paho.Connack) { pahoConnUpChan <- struct{}{} },
		Debug:          logger,
		PahoDebug:      logger,
		PahoErrors:     logger,
		ClientConfig: paho.ClientConfig{
			ClientID: "test",
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					mrMu.Lock()
					defer mrMu.Unlock()
					messagesReceived = append(messagesReceived, pr.Packet)
					if len(messagesReceived) == expectedMessages {
						close(mrDone)
					}
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
	var initialConnUpMsg tsConnUpMsg
	select {
	case initialConnUpMsg = <-tsConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting initial connection request")
	}
	select {
	case <-pahoConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting connection up")
	}

	testFmt := "Test%d"
	var subs []paho.SubscribeOptions
	for i := 0; i < 3; i++ {
		subs = append(subs, paho.SubscribeOptions{
			Topic: fmt.Sprintf(testFmt, i),
			QoS:   byte(i),
		})
	}
	if _, err = cm.Subscribe(ctx, &paho.Subscribe{
		Subscriptions: subs,
	}); err != nil {
		t.Fatalf("subscribe failed: %s", err)
	}

	// Note: The client does not currently support
	for i := 0; i < 3; i++ {
		t.Logf("publish QOS %d message", i)
		msg := fmt.Sprintf(testFmt, i)
		if _, err := cm.Publish(ctx, &paho.Publish{
			QoS:        byte(i),
			Topic:      msg,
			Properties: nil,
			Payload:    []byte(msg),
		}); err != nil {
			t.Fatalf("publish at QOS %d failed: %s", i, err)
		}
		t.Logf("publish QOS %d message complete", i)
	}

	// Wait until we have received the expected messages
	select {
	case <-mrDone:
	case <-time.After(shortDelay):
		mrMu.Lock()
		t.Fatalf("received %d of the expected %d messages (%v)", len(messagesReceived), expectedMessages, messagesReceived)
		// mrMu.Unlock() not needed as Fatal exits
	}

	// Note: While messages have been received, the QOS2 handshake process is probably still in progress
	// (router callback is called before any acknowledgement is sent).
	t.Log("messages received - closing connection")
	cancel() // Cancelling outer context will cascade
	select { // Wait for the local client to terminate
	case <-cm.Done():
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting connection manager shutdown")
	}

	select { // Wait for test server to terminate
	case <-initialConnUpMsg.done:
	case <-time.After(shortDelay):
		t.Fatal("test server did not shut down in a timely manner")
	}

	// Check we got what we expected
	for i := 0; i < 3; i++ {
		msg := fmt.Sprintf(testFmt, i)
		if messagesReceived[i].Topic != msg {
			t.Errorf("expected topic %s, got %s", msg, messagesReceived[i].Topic)
		}
		if string(messagesReceived[i].Payload) != msg {
			t.Errorf("expected message %v, got %v", []byte(msg), messagesReceived[i].Payload)
		}
		if err != nil {
			t.Fatalf("publish at QOS %d failed: %s", i, err)
		}
	}
}

func TestAuthenticate(t *testing.T) {
	t.Parallel()
	server, _ := url.Parse(dummyURL)
	serverLogger := paholog.NewTestLogger(t, "testServer:")
	logger := paholog.NewTestLogger(t, "test:")

	ts := testserver.New(serverLogger)

	type tsConnUpMsg struct {
		cancelFn func()        // Function to cancel test server context
		done     chan struct{} // Will be closed when the test server has disconnected (and shutdown)
	}
	tsConnUpChan := make(chan tsConnUpMsg) // Message will be sent when test server connection is up
	pahoConnUpChan := make(chan struct{})  // When autopaho reports connection is up write to channel will occur

	atCount := 0

	config := ClientConfig{
		ServerUrls:       []*url.URL{server},
		KeepAlive:        60,
		ReconnectBackoff: NewConstantBackoff(time.Millisecond), // Retry connection very quickly!
		ConnectTimeout:   shortDelay,                           // Connection should come up very quickly
		AttemptConnection: func(ctx context.Context, _ ClientConfig, _ *url.URL) (net.Conn, error) {
			atCount += 1
			if atCount == 2 { // fail on the initial reconnection attempt to exercise retry functionality
				return nil, errors.New("connection attempt failed")
			}
			ctx, cancel := context.WithCancel(ctx)
			conn, done, err := ts.Connect(ctx)
			if err == nil { // The above may fail if attempted too quickly (before disconnect processed)
				tsConnUpChan <- tsConnUpMsg{cancelFn: cancel, done: done}
			} else {
				cancel()
			}
			return conn, err
		},
		OnConnectionUp: func(*ConnectionManager, *paho.Connack) { pahoConnUpChan <- struct{}{} },
		Debug:          logger,
		PahoDebug:      logger,
		PahoErrors:     logger,
		ClientConfig: paho.ClientConfig{
			ClientID:    "test",
			AuthHandler: &fakeAuth{},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm, err := NewConnection(ctx, config)
	if err != nil {
		t.Fatalf("expected NewConnection success: %s", err)
	}

	var initialConnUpMsg tsConnUpMsg
	select {
	case initialConnUpMsg = <-tsConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting initial connection request")
	}
	select {
	case <-pahoConnUpChan:
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting connection up")
	}

	ctx, cf := context.WithTimeout(context.Background(), 5*time.Second)
	defer cf()
	ar, err := cm.Authenticate(ctx, &paho.Auth{
		ReasonCode: packets.AuthReauthenticate,
		Properties: &paho.AuthProperties{
			AuthMethod: "TEST",
			AuthData:   []byte("secret data"),
		},
	})
	if err != nil {
		t.Fatalf("authenticate failed: %s", err)
	}
	if !ar.Success {
		t.Fatal("authenticate failed")
	}

	cancel() // Cancelling outer context will cascade
	select { // Wait for the local client to terminate
	case <-cm.Done():
	case <-time.After(shortDelay):
		t.Fatal("timeout awaiting connection manager shutdown")
	}

	select { // Wait for test server to terminate
	case <-initialConnUpMsg.done:
	case <-time.After(shortDelay):
		t.Fatal("test server did not shut down in a timely manner")
	}
}

// fakeAuth implements the Auther interface to test auto.AuthHandler
type fakeAuth struct{}

func (f *fakeAuth) Authenticate(a *paho.Auth) *paho.Auth {
	return &paho.Auth{
		Properties: &paho.AuthProperties{
			AuthMethod: "TEST",
			AuthData:   []byte("secret data"),
		},
	}
}

func (f *fakeAuth) Authenticated() {}

// TestClientConfig_buildConnectPacket exercises buildConnectPacket checking that options and callbacks are applied
func TestClientConfig_buildConnectPacket(t *testing.T) {
	server, _ := url.Parse(dummyURL)

	config := ClientConfig{
		ServerUrls:                    []*url.URL{server},
		KeepAlive:                     5,
		ReconnectBackoff:              NewConstantBackoff(5 * time.Second),
		ConnectTimeout:                5 * time.Second,
		CleanStartOnInitialConnection: true, // Should set Clean Start flag on first connection attempt
		// extends the lower-level paho.ClientConfig
		ClientConfig: paho.ClientConfig{
			ClientID: "test",
		},
	}

	// Validate initial state
	cp, _ := config.buildConnectPacket(true, nil)

	if !cp.CleanStart {
		t.Errorf("Expected Clean Start to be true")
	}
	if cp.WillMessage != nil {
		t.Errorf("Expected empty Will message, got: %v", cp.WillMessage)
	}

	if cp.UsernameFlag != false || cp.Username != "" {
		t.Errorf("Expected absent/empty username, got: flag=%v username=%v", cp.UsernameFlag, cp.Username)
	}

	if cp.PasswordFlag != false || len(cp.Password) > 0 {
		t.Errorf("Expected absent/empty password, got: flag=%v password=%v", cp.PasswordFlag, cp.Password)
	}

	// Set some common parameters (using now depreciated functions)
	config.SetUsernamePassword("testuser", []byte("testpassword"))
	config.SetWillMessage(fmt.Sprintf("client/%s/state", config.ClientID), []byte("disconnected"), 1, true)

	cp, _ = config.buildConnectPacket(false, nil)
	if cp.CleanStart {
		t.Errorf("Expected Clean Start to be false")
	}

	if cp.UsernameFlag == false || cp.Username != "testuser" {
		t.Errorf("Expected a username, got: flag=%v username=%v", cp.UsernameFlag, cp.Username)
	}

	pMatch := bytes.Compare(cp.Password, []byte("testpassword"))

	if cp.PasswordFlag == false || len(cp.Password) == 0 || pMatch != 0 {
		t.Errorf("Expected a password, got: flag=%v password=%v", cp.PasswordFlag, cp.Password)
	}

	if cp.WillMessage == nil {
		t.Error("Expected a Will message, found nil")
	}

	if cp.WillMessage.Topic != "client/test/state" {
		t.Errorf("Will message topic did not match expected [%v], found [%v]", "client/test/state", cp.WillMessage.Topic)
	}

	if cp.WillMessage.QoS != byte(1) {
		t.Errorf("Will message QOS did not match expected [1]: found [%v]", cp.WillMessage.QoS)
	}

	if cp.WillMessage.Retain != true {
		t.Errorf("Will message Retain did not match expected [true]: found [%v]", cp.WillMessage.Retain)
	}

	if *(cp.WillProperties.WillDelayInterval) != 10 { // assumes default 2x keep alive
		t.Errorf("Will message Delay Interval did not match expected [10]: found [%v]", *(cp.Properties.WillDelayInterval))
	}

	// Set an override method for the CONNECT packet
	config.SetConnectPacketConfigurator(func(c *paho.Connect) (*paho.Connect, error) {
		delay := uint32(200)
		c.WillProperties.WillDelayInterval = &delay
		return c, nil
	})

	testUrl, _ := url.Parse("mqtt://mqtt_user:mqtt_pass@127.0.0.1:1883")
	cp, _ = config.buildConnectPacket(false, testUrl)

	if *(cp.WillProperties.WillDelayInterval) != 200 { // verifies the override
		t.Errorf("Will message Delay Interval did not match expected [200]: found [%v]", *(cp.Properties.WillDelayInterval))
	}
}

// Example of using ConnectPacketBuilder to extract server password from URL
func ExampleClientConfig_ConnectPacketBuilder() {
	serverURL, _ := url.Parse("mqtt://mqtt_user:mqtt_pass@127.0.0.1:1883")
	config := ClientConfig{
		ServerUrls:       []*url.URL{serverURL},
		ReconnectBackoff: NewConstantBackoff(5 * time.Second),
		ConnectTimeout:   5 * time.Second,
		ClientConfig: paho.ClientConfig{
			ClientID: "test",
		},
	}
	config.ConnectPacketBuilder = func(c *paho.Connect, u *url.URL) (*paho.Connect, error) {
		// Extracting password from URL
		c.Username = u.User.Username()
		// up to user to catch empty password passed via URL
		p, _ := u.User.Password()
		c.Password = []byte(p)
		return c, nil
	}
	cp, _ := config.buildConnectPacket(false, serverURL)
	fmt.Printf("user: %s, pass: %s", cp.Username, string(cp.Password))
	// Output: user: mqtt_user, pass: mqtt_pass
}
