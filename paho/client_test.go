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

package paho

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/canonical/mqtt.golang/mock/basictestserver"
	"github.com/canonical/mqtt.golang/packets"
	paholog "github.com/canonical/mqtt.golang/paho/log"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClient(t *testing.T) {
	c := NewClient(ClientConfig{})

	require.NotNil(t, c)
	require.NotNil(t, c.config.Session)
	require.NotNil(t, c.config.Router)
	require.NotNil(t, c.config.PingHandler)

	assert.Equal(t, uint16(65535), c.serverProps.ReceiveMaximum)
	assert.Equal(t, uint8(2), c.serverProps.MaximumQoS)
	assert.Equal(t, uint32(0), c.serverProps.MaximumPacketSize)
	assert.Equal(t, uint16(0), c.serverProps.TopicAliasMaximum)
	assert.True(t, c.serverProps.RetainAvailable)
	assert.True(t, c.serverProps.WildcardSubAvailable)
	assert.True(t, c.serverProps.SubIDAvailable)
	assert.True(t, c.serverProps.SharedSubAvailable)

	assert.Equal(t, uint16(65535), c.clientProps.ReceiveMaximum)
	assert.Equal(t, uint8(2), c.clientProps.MaximumQoS)
	assert.Equal(t, uint32(0), c.clientProps.MaximumPacketSize)
	assert.Equal(t, uint16(0), c.clientProps.TopicAliasMaximum)

	assert.Equal(t, 10*time.Second, c.config.PacketTimeout)
}

func TestClientConnect(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "ClientConnect:")
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.CONNACK, &packets.Connack{
		ReasonCode:     0,
		SessionPresent: false,
		Properties: &packets.Properties{
			MaximumPacketSize: Uint32(12345),
			MaximumQOS:        Byte(1),
			ReceiveMaximum:    Uint16(12345),
			TopicAliasMaximum: Uint16(200),
		},
	})
	go ts.Run()
	defer func() {
		ts.Stop()
	}()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	cp := &Connect{
		KeepAlive:  30,
		ClientID:   "testClient",
		CleanStart: true,
		Properties: &ConnectProperties{
			ReceiveMaximum: Uint16(200),
		},
		WillMessage: &WillMessage{
			Topic:   "will/topic",
			Payload: []byte("am gone"),
		},
		WillProperties: &WillProperties{
			WillDelayInterval: Uint32(200),
		},
	}

	ca, err := c.Connect(context.Background(), cp)
	require.Nil(t, err)
	assert.Equal(t, uint8(0), ca.ReasonCode)
}

func TestClientSubscribe(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "ClientSubscribe:")
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.SUBACK, &packets.Suback{
		Reasons:    []byte{1, 2, 0},
		Properties: &packets.Properties{},
	})
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	clientCtx := basicClientInitialisation(c)
	c.publishPackets = make(chan *packets.Publish)
	c.workers.Add(2)
	go func() {
		defer c.workers.Done()
		c.incoming(clientCtx)
	}()
	go func() {
		defer c.workers.Done()
		c.config.PingHandler.Run(clientCtx, c.config.Conn, 30)
	}()
	c.config.Session.ConAckReceived(c.config.Conn, &packets.Connect{}, &packets.Connack{})

	s := &Subscribe{
		Subscriptions: []SubscribeOptions{
			{Topic: "test/1", QoS: 1},
			{Topic: "test/2", QoS: 2},
			{Topic: "test/3", QoS: 0},
		},
	}

	sa, err := c.Subscribe(context.Background(), s)
	require.Nil(t, err)
	assert.Equal(t, []byte{1, 2, 0}, sa.Reasons)

	time.Sleep(10 * time.Millisecond)
}

func TestClientUnsubscribe(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "ClientUnsubscribe:")
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.UNSUBACK, &packets.Unsuback{
		Reasons:    []byte{0, 17},
		Properties: &packets.Properties{},
	})
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	clientCtx := basicClientInitialisation(c)
	c.publishPackets = make(chan *packets.Publish)
	c.workers.Add(2)
	go func() {
		defer c.workers.Done()
		c.incoming(clientCtx)
	}()
	go func() {
		defer c.workers.Done()
		c.config.PingHandler.Run(clientCtx, c.config.Conn, 30)
	}()
	c.config.Session.ConAckReceived(c.config.Conn, &packets.Connect{}, &packets.Connack{})

	u := &Unsubscribe{
		Topics: []string{
			"test/1",
			"test/2",
		},
	}

	ua, err := c.Unsubscribe(context.Background(), u)
	require.Nil(t, err)
	assert.Equal(t, []byte{0, 17}, ua.Reasons)
}

func TestClientPublishQoS0(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "ClientPublishQoS0:")
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer"))
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	clientCtx := basicClientInitialisation(c)
	c.publishPackets = make(chan *packets.Publish)
	c.workers.Add(2)
	go func() {
		defer c.workers.Done()
		c.incoming(clientCtx)
	}()
	go func() {
		defer c.workers.Done()
		c.config.PingHandler.Run(clientCtx, c.config.Conn, 30)
	}()
	c.config.Session.ConAckReceived(c.config.Conn, &packets.Connect{}, &packets.Connack{})

	p := &Publish{
		Topic:   "test/0",
		QoS:     0,
		Payload: []byte("test payload"),
	}

	pr, err := c.Publish(context.Background(), p)
	require.Nil(t, err)
	require.NotNil(t, pr)

	time.Sleep(10 * time.Millisecond)
}

func TestClientPublishQoS1(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "ClientPublishQoS1:")
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.PUBACK, &packets.Puback{
		ReasonCode: packets.PubackSuccess,
		Properties: &packets.Properties{},
	})
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	clientCtx := basicClientInitialisation(c)
	c.publishPackets = make(chan *packets.Publish)
	c.workers.Add(2)
	go func() {
		defer c.workers.Done()
		c.incoming(clientCtx)
	}()
	go func() {
		defer c.workers.Done()
		c.config.PingHandler.Run(clientCtx, c.config.Conn, 30)
	}()
	c.config.Session.ConAckReceived(c.config.Conn, &packets.Connect{}, &packets.Connack{})

	p := &Publish{
		Topic:   "test/1",
		QoS:     1,
		Payload: []byte("test payload"),
	}

	pa, err := c.Publish(context.Background(), p)
	require.Nil(t, err)
	assert.Equal(t, uint8(0), pa.ReasonCode)
}

func TestClientPublishQoS2(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "ClientPublishQoS2:")
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.PUBREC, &packets.Pubrec{
		ReasonCode: packets.PubrecSuccess,
		Properties: &packets.Properties{},
	})
	ts.SetResponse(packets.PUBCOMP, &packets.Pubcomp{
		ReasonCode: packets.PubcompSuccess,
		Properties: &packets.Properties{},
	})
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	clientCtx := basicClientInitialisation(c)
	c.publishPackets = make(chan *packets.Publish)
	c.workers.Add(2)
	go func() {
		defer c.workers.Done()
		c.incoming(clientCtx)
	}()
	go func() {
		defer c.workers.Done()
		c.config.PingHandler.Run(clientCtx, c.config.Conn, 30)
	}()
	c.config.Session.ConAckReceived(c.config.Conn, &packets.Connect{}, &packets.Connack{})

	p := &Publish{
		Topic:   "test/2",
		QoS:     2,
		Payload: []byte("test payload"),
	}

	pr, err := c.Publish(context.Background(), p)
	require.Nil(t, err)
	assert.Equal(t, uint8(0), pr.ReasonCode)
}

func TestClientReceiveQoS0(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "TestClientReceiveQoS0:")

	rChan := make(chan struct{})
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
		OnPublishReceived: []func(PublishReceived) (bool, error){ // Noop handler
			func(pr PublishReceived) (bool, error) {
				assert.Equal(t, "test/0", pr.Packet.Topic)
				assert.Equal(t, "test payload", string(pr.Packet.Payload))
				assert.Equal(t, byte(0), pr.Packet.QoS)
				close(rChan)
				return true, nil
			}},
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	clientCtx := basicClientInitialisation(c)
	c.publishPackets = make(chan *packets.Publish)
	c.workers.Add(2)
	go func() {
		defer c.workers.Done()
		c.incoming(clientCtx)
	}()
	go func() {
		defer c.workers.Done()
		c.config.PingHandler.Run(clientCtx, c.config.Conn, 30)
	}()
	c.config.Session.ConAckReceived(c.config.Conn, &packets.Connect{}, &packets.Connack{})
	go c.routePublishPackets()

	err := ts.SendPacket(&packets.Publish{
		Topic:   "test/0",
		QoS:     0,
		Payload: []byte("test payload"),
	})
	require.NoError(t, err)

	<-rChan
}

func TestClientReceiveQoS1(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "TestClientReceiveQoS1:")

	rChan := make(chan struct{})
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
		OnPublishReceived: []func(PublishReceived) (bool, error){ // Noop handler
			func(pr PublishReceived) (bool, error) {
				assert.Equal(t, "test/1", pr.Packet.Topic)
				assert.Equal(t, "test payload", string(pr.Packet.Payload))
				assert.Equal(t, byte(1), pr.Packet.QoS)
				close(rChan)
				return true, nil
			}},
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	clientCtx := basicClientInitialisation(c)
	c.publishPackets = make(chan *packets.Publish)
	c.workers.Add(2)
	go func() {
		defer c.workers.Done()
		c.incoming(clientCtx)
	}()
	go func() {
		defer c.workers.Done()
		c.config.PingHandler.Run(clientCtx, c.config.Conn, 30)
	}()
	c.config.Session.ConAckReceived(c.config.Conn, &packets.Connect{}, &packets.Connack{})
	go c.routePublishPackets()

	err := ts.SendPacket(&packets.Publish{
		PacketID: 1,
		Topic:    "test/1",
		QoS:      1,
		Payload:  []byte("test payload"),
	})
	require.NoError(t, err)

	<-rChan
}

func TestClientReceiveQoS2(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "TestClientReceiveQoS2:")

	rChan := make(chan struct{})
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
		OnPublishReceived: []func(PublishReceived) (bool, error){ // Noop handler
			func(pr PublishReceived) (bool, error) {
				assert.Equal(t, "test/2", pr.Packet.Topic)
				assert.Equal(t, "test payload", string(pr.Packet.Payload))
				assert.Equal(t, byte(2), pr.Packet.QoS)
				close(rChan)
				return true, nil
			}},
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	clientCtx := basicClientInitialisation(c)
	c.publishPackets = make(chan *packets.Publish)
	c.workers.Add(2)
	go func() {
		defer c.workers.Done()
		c.incoming(clientCtx)
	}()
	go func() {
		defer c.workers.Done()
		c.config.PingHandler.Run(clientCtx, c.config.Conn, 30)
	}()
	c.config.Session.ConAckReceived(c.config.Conn, &packets.Connect{}, &packets.Connack{})
	go c.routePublishPackets()

	err := ts.SendPacket(&packets.Publish{
		PacketID: 1,
		Topic:    "test/2",
		QoS:      2,
		Payload:  []byte("test payload"),
	})
	require.NoError(t, err)

	<-rChan
}

func TestClientReceiveAndAckInOrder(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "ClientReceiveAndAckInOrder:")

	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.CONNACK, &packets.Connack{
		ReasonCode:     0,
		SessionPresent: false,
		Properties: &packets.Properties{
			MaximumPacketSize: Uint32(12345),
			MaximumQOS:        Byte(1),
			ReceiveMaximum:    Uint16(12345),
			TopicAliasMaximum: Uint16(200),
		},
	})
	go ts.Run()
	defer ts.Stop()

	var (
		wg                   sync.WaitGroup
		actualPublishPackets []packets.Publish
		expectedPacketsCount = 3
	)

	wg.Add(expectedPacketsCount)
	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
		OnPublishReceived: []func(PublishReceived) (bool, error){ // Noop handler
			func(pr PublishReceived) (bool, error) {
				defer wg.Done()
				actualPublishPackets = append(actualPublishPackets, *pr.Packet.Packet())
				return true, nil
			}},
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	ctx := context.Background()
	ca, err := c.Connect(ctx, &Connect{
		KeepAlive:  30,
		ClientID:   "testClient",
		CleanStart: true,
		Properties: &ConnectProperties{
			ReceiveMaximum: Uint16(200),
		},
	})
	require.Nil(t, err)
	assert.Equal(t, uint8(0), ca.ReasonCode)

	var expectedPublishPackets []packets.Publish
	for i := 1; i <= expectedPacketsCount; i++ {
		p := packets.Publish{
			PacketID: uint16(i),
			Topic:    fmt.Sprintf("test/%d", i),
			Payload:  []byte(fmt.Sprintf("test payload %d", i)),
			QoS:      1,
			Properties: &packets.Properties{
				User: make([]packets.User, 0),
			},
		}
		expectedPublishPackets = append(expectedPublishPackets, p)
		require.NoError(t, ts.SendPacket(&p))
	}

	wg.Wait()

	require.Equal(t, expectedPublishPackets, actualPublishPackets)
	expectedAcks := []packets.Puback{
		{PacketID: 1, ReasonCode: 0, Properties: &packets.Properties{}},
		{PacketID: 2, ReasonCode: 0, Properties: &packets.Properties{}},
		{PacketID: 3, ReasonCode: 0, Properties: &packets.Properties{}},
	}
	require.Eventually(t,
		func() bool {
			return cmp.Equal(expectedAcks, ts.ReceivedPubacks())
		},
		time.Second,
		10*time.Millisecond,
		cmp.Diff(expectedAcks, ts.ReceivedPubacks()),
	)
}

func TestManualAcksInOrder(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "ManualAcksInOrder:")
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.CONNACK, &packets.Connack{
		ReasonCode:     0,
		SessionPresent: false,
		Properties: &packets.Properties{
			MaximumPacketSize: Uint32(12345),
			MaximumQOS:        Byte(1),
			ReceiveMaximum:    Uint16(12345),
			TopicAliasMaximum: Uint16(200),
		},
	})
	go ts.Run()
	defer ts.Stop()

	var (
		wg                   sync.WaitGroup
		actualPublishPackets []packets.Publish
		expectedPacketsCount = 3
	)

	wg.Add(expectedPacketsCount)
	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
		OnPublishReceived: []func(PublishReceived) (bool, error){
			func(pr PublishReceived) (bool, error) {
				defer wg.Done()
				actualPublishPackets = append(actualPublishPackets, *pr.Packet.Packet())
				require.NoError(t, pr.Client.Ack(pr.Packet))
				return true, nil
			},
		},
		EnableManualAcknowledgment: true,
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	ctx := context.Background()
	ca, err := c.Connect(ctx, &Connect{
		KeepAlive:  30,
		ClientID:   "testClient",
		CleanStart: true,
		Properties: &ConnectProperties{
			ReceiveMaximum: Uint16(200),
		},
	})
	require.Nil(t, err)
	assert.Equal(t, uint8(0), ca.ReasonCode)

	var expectedPublishPackets []packets.Publish
	for i := 1; i <= expectedPacketsCount; i++ {
		p := packets.Publish{
			PacketID: uint16(i),
			Topic:    fmt.Sprintf("test/%d", i),
			Payload:  []byte(fmt.Sprintf("test payload %d", i)),
			QoS:      1,
			Properties: &packets.Properties{
				User: make([]packets.User, 0),
			},
		}
		expectedPublishPackets = append(expectedPublishPackets, p)
		require.NoError(t, ts.SendPacket(&p))
	}

	wg.Wait()

	require.Equal(t, expectedPublishPackets, actualPublishPackets)
	expectedAcks := []packets.Puback{
		{PacketID: 1, ReasonCode: 0, Properties: &packets.Properties{}},
		{PacketID: 2, ReasonCode: 0, Properties: &packets.Properties{}},
		{PacketID: 3, ReasonCode: 0, Properties: &packets.Properties{}},
	}
	require.Eventually(t,
		func() bool {
			return cmp.Equal(expectedAcks, ts.ReceivedPubacks())
		},
		time.Second,
		10*time.Millisecond,
		cmp.Diff(expectedAcks, ts.ReceivedPubacks()),
	)

	// Test QoS 0 packets are ignored
	require.NoError(t, c.Ack(&Publish{QoS: 0, PacketID: 11233}))

	// Test packets not found
	require.True(t, errors.Is(c.Ack(&Publish{QoS: 1, PacketID: 123}), ErrPacketNotFound))
	require.True(t, errors.Is(c.Ack(&Publish{QoS: 2, PacketID: 65535}), ErrPacketNotFound))
}

func TestReceiveServerDisconnect(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "ServerDisconnect:")
	rChan := make(chan struct{})
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
		OnServerDisconnect: func(d *Disconnect) {
			assert.Equal(t, byte(packets.DisconnectServerShuttingDown), d.ReasonCode)
			assert.Equal(t, d.Properties.ReasonString, "GONE!")
			close(rChan)
		},
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	clientCtx := basicClientInitialisation(c)
	c.publishPackets = make(chan *packets.Publish)
	c.workers.Add(2)
	go func() {
		defer c.workers.Done()
		c.incoming(clientCtx)
	}()
	go func() {
		defer c.workers.Done()
		c.config.PingHandler.Run(clientCtx, c.config.Conn, 30)
	}()
	c.config.Session.ConAckReceived(c.config.Conn, &packets.Connect{}, &packets.Connack{})

	err := ts.SendPacket(&packets.Disconnect{
		ReasonCode: packets.DisconnectServerShuttingDown,
		Properties: &packets.Properties{
			ReasonString: "GONE!",
		},
	})
	require.NoError(t, err)

	<-rChan
}

func TestAuthenticate(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "Authenticate:")
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.AUTH, &packets.Auth{
		ReasonCode: packets.AuthSuccess,
	})
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn:        ts.ClientConn(),
		AuthHandler: &fakeAuth{},
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	clientCtx := basicClientInitialisation(c)
	c.publishPackets = make(chan *packets.Publish)
	c.workers.Add(2)
	go func() {
		defer c.workers.Done()
		c.incoming(clientCtx)
	}()
	go func() {
		defer c.workers.Done()
		c.config.PingHandler.Run(clientCtx, c.config.Conn, 30)
	}()
	c.config.Session.ConAckReceived(c.config.Conn, &packets.Connect{}, &packets.Connack{})

	ctx, cf := context.WithTimeout(context.Background(), 5*time.Second)
	defer cf()
	ar, err := c.Authenticate(ctx, &Auth{
		ReasonCode: packets.AuthReauthenticate,
		Properties: &AuthProperties{
			AuthMethod: "TEST",
			AuthData:   []byte("secret data"),
		},
	})
	require.Nil(t, err)
	assert.True(t, ar.Success)

	time.Sleep(10 * time.Millisecond)
}

type TestAuth struct {
	auther        func(*Auth) *Auth
	authenticated func()
}

func (t *TestAuth) Authenticate(a *Auth) *Auth {
	return t.auther(a)
}

func (t *TestAuth) Authenticated() {
	t.authenticated()
}

func waitTimeout(wg *sync.WaitGroup, t time.Duration) bool {
	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()
	select {
	case <-c:
		return false
	case <-time.After(t):
		return true
	}
}

func TestAuthenticateOnConnect(t *testing.T) {
	var wg sync.WaitGroup
	auther := TestAuth{
		auther: func(a *Auth) *Auth {
			return &Auth{
				ReasonCode: packets.AuthContinueAuthentication,
				Properties: &AuthProperties{
					AuthMethod: "testauth",
					AuthData:   []byte("client-final-data"),
				},
			}
		},
		authenticated: func() {
			wg.Done()
		},
	}

	clientLogger := paholog.NewTestLogger(t, "AuthenticateOnConnect:")
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.CONNACK, &packets.Auth{
		ReasonCode: packets.AuthContinueAuthentication,
		Properties: &packets.Properties{
			AuthMethod: "testauth",
			AuthData:   []byte("server first data"),
		},
	})
	ts.SetResponse(packets.AUTH, &packets.Connack{
		ReasonCode: packets.ConnackSuccess,
		Properties: &packets.Properties{
			AuthMethod: "testauth",
			AuthData:   []byte("server final data"),
		},
	})
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn:        ts.ClientConn(),
		AuthHandler: &auther,
	})
	require.NotNil(t, c)
	defer c.close()
	c.SetDebugLogger(clientLogger)

	cp := &Connect{
		KeepAlive:  30,
		ClientID:   "testClient",
		CleanStart: true,
		Properties: &ConnectProperties{
			AuthMethod: "testauth",
			AuthData:   []byte("client first data"),
		},
	}
	wg.Add(1)

	ca, err := c.Connect(context.Background(), cp)
	require.Nil(t, err)
	assert.Equal(t, uint8(0), ca.ReasonCode)

	assert.False(t, waitTimeout(&wg, 1*time.Second))
}

func TestCleanup(t *testing.T) {
	serverLogger := paholog.NewTestLogger(t, "TestServer:")
	ts := basictestserver.New(serverLogger)
	go ts.Run()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
	})
	require.NotNil(t, c)
	basicClientInitialisation(c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelling to make the client fail on the connection attempt
	ca, err := c.Connect(ctx, &Connect{
		ClientID: "testClient",
	})
	require.True(t, errors.Is(err, context.Canceled))
	require.Nil(t, ca)

	// client should have shutdown cleanly (and waited before returning)
	select {
	case <-c.Done():
	default:
		t.Error("client should be done")

	}

	// verify that it's possible to try again
	ts.Stop()
	ts = basictestserver.New(serverLogger)
	ts.SetResponse(packets.CONNACK, &packets.Connack{
		ReasonCode:     0,
		SessionPresent: false,
		Properties: &packets.Properties{
			MaximumPacketSize: Uint32(12345),
			MaximumQOS:        Byte(1),
			ReceiveMaximum:    Uint16(12345),
			TopicAliasMaximum: Uint16(200),
		},
	})
	go ts.Run()
	defer ts.Stop()

	ctx = context.Background()
	c = NewClient(ClientConfig{
		Conn: ts.ClientConn(),
	})
	require.NotNil(t, c)
	ca, err = c.Connect(ctx, &Connect{
		KeepAlive:  30,
		ClientID:   "testClient",
		CleanStart: true,
		Properties: &ConnectProperties{
			ReceiveMaximum: Uint16(200),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, uint8(0), ca.ReasonCode)
}

func TestDisconnect(t *testing.T) {
	clientLogger := paholog.NewTestLogger(t, "Disconnect:")
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.CONNACK, &packets.Connack{
		ReasonCode:     0,
		SessionPresent: false,
		Properties: &packets.Properties{
			MaximumPacketSize: Uint32(12345),
			MaximumQOS:        Byte(1),
			ReceiveMaximum:    Uint16(12345),
			TopicAliasMaximum: Uint16(200),
		},
	})
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
	})
	require.NotNil(t, c)
	basicClientInitialisation(c)
	c.SetDebugLogger(clientLogger)
	defer c.close()

	ctx := context.Background()
	ca, err := c.Connect(ctx, &Connect{
		KeepAlive:  30,
		ClientID:   "testClient",
		CleanStart: true,
		Properties: &ConnectProperties{
			ReceiveMaximum: Uint16(200),
		},
	})
	require.Nil(t, err)
	assert.Equal(t, uint8(0), ca.ReasonCode)

	err = c.Disconnect(&Disconnect{})
	require.NoError(t, err)

	// client should have shutdown cleanly (and waited before returning)
	select {
	case <-c.Done():
	default:
		t.Error("client should be done")

	}

	// disconnect again should return an error but not block
	err = c.Disconnect(&Disconnect{})
	require.True(t, errors.Is(err, io.ErrClosedPipe))
}

func TestCloseDeadlock(t *testing.T) {
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.CONNACK, &packets.Connack{
		ReasonCode:     0,
		SessionPresent: false,
		Properties: &packets.Properties{
			MaximumPacketSize: Uint32(12345),
			MaximumQOS:        Byte(1),
			ReceiveMaximum:    Uint16(12345),
			TopicAliasMaximum: Uint16(200),
		},
	})
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
	})
	require.NotNil(t, c)

	ctx := context.Background()
	ca, err := c.Connect(ctx, &Connect{
		KeepAlive:  30,
		ClientID:   "testClient",
		CleanStart: true,
		Properties: &ConnectProperties{
			ReceiveMaximum: Uint16(200),
		},
	})
	require.Nil(t, err)
	assert.Equal(t, uint8(0), ca.ReasonCode)

	routines := 100
	wg := sync.WaitGroup{}
	wg.Add(routines * 2)
	for i := 0; i < routines; i++ {
		go func() {
			defer wg.Done()
			c.close()
		}()
		go func() {
			defer wg.Done()
			_ = c.Disconnect(&Disconnect{})
		}()
	}
	wg.Wait()
}

func TestSendOnClosedChannel(t *testing.T) {
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.CONNACK, &packets.Connack{
		ReasonCode:     0,
		SessionPresent: false,
		Properties: &packets.Properties{
			MaximumPacketSize: Uint32(12345),
			MaximumQOS:        Byte(1),
			ReceiveMaximum:    Uint16(12345),
			TopicAliasMaximum: Uint16(200),
		},
	})
	go ts.Run()
	defer ts.Stop()

	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(),
	})
	require.NotNil(t, c)

	if testing.Verbose() {
		l := log.New(os.Stdout, t.Name(), log.LstdFlags)
		c.SetDebugLogger(l)
		c.SetErrorLogger(l)
	}

	ctx := context.Background()
	ca, err := c.Connect(ctx, &Connect{
		KeepAlive:  30,
		ClientID:   "testClient",
		CleanStart: true,
		Properties: &ConnectProperties{
			ReceiveMaximum: Uint16(200),
		},
	})
	require.Nil(t, err)
	assert.Equal(t, uint8(0), ca.ReasonCode)

	go func() {
		for i := uint16(0); true; i++ {
			err := ts.SendPacket(&packets.Publish{
				Payload:  []byte("ciao"),
				Topic:    "test",
				PacketID: i,
				QoS:      1,
			})
			if err != nil {
				t.Logf("Send packet error: %v", err)
				return
			}
		}
	}()

	time.Sleep(10 * time.Millisecond)
	c.close()
}

// fakeAuth implements the Auther interface to test client.AuthHandler
type fakeAuth struct{}

func (f *fakeAuth) Authenticate(a *Auth) *Auth {
	return &Auth{
		Properties: &AuthProperties{
			AuthMethod: "TEST",
			AuthData:   []byte("secret data"),
		},
	}
}

func (f *fakeAuth) Authenticated() {}

// TestAddOnPublishReceived checks we can add and remove onPublishReceived callbacks
func TestAddOnPublishReceived(t *testing.T) {
	callAll := func(c *Client) {
		for _, f := range c.onPublishReceived {
			f(PublishReceived{Packet: &Publish{Properties: &PublishProperties{}}})
		}
	}

	c := NewClient(ClientConfig{})
	require.Lenf(t, c.onPublishReceived, 1, "onPublishReceived must contain one record (as c.Router is nil)")
	require.Lenf(t, c.onPublishReceivedTracker, 1, "onPublishReceived must contain one record")
	testOne := 0
	removeOne := c.AddOnPublishReceived(func(_ PublishReceived) (bool, error) { testOne++; return false, nil })
	require.Lenf(t, c.onPublishReceived, 2, "onPublishReceived should have one item")
	require.Lenf(t, c.onPublishReceivedTracker, 2, "onPublishReceived should have one item")
	callAll(c)
	require.Equal(t, 1, testOne, "Expected 1")
	removeOne()
	callAll(c)
	require.Equal(t, 1, testOne, "Expected 1")

	testTwo := 0
	testThree := 0
	removeTwo := c.AddOnPublishReceived(func(_ PublishReceived) (bool, error) { testTwo++; return false, nil })
	removeThree := c.AddOnPublishReceived(func(_ PublishReceived) (bool, error) { testThree++; return false, nil })
	callAll(c)
	require.Equal(t, 1, testOne, "Expected 1")
	require.Equal(t, 1, testTwo, "Expected 1")
	require.Equal(t, 1, testThree, "Expected 1")
	removeTwo()
	callAll(c)
	require.Equal(t, 1, testOne, "Expected 1")
	require.Equal(t, 1, testTwo, "Expected 1")
	require.Equal(t, 2, testThree, "Expected 2")
	removeThree()
	callAll(c)
	require.Equal(t, 1, testOne, "Expected 1")
	require.Equal(t, 1, testTwo, "Expected 1")
	require.Equal(t, 2, testThree, "Expected 2")

	// Test with a pre-populated onPublishReceived
	testOne = 0
	testTwo = 0
	testThree = 0
	c = NewClient(ClientConfig{
		OnPublishReceived: []func(PublishReceived) (bool, error){
			func(_ PublishReceived) (bool, error) { testOne++; return false, nil },
			func(_ PublishReceived) (bool, error) { testTwo++; return false, nil },
		},
	})
	callAll(c)
	require.Equal(t, 1, testOne, "Expected 1")
	require.Equal(t, 1, testTwo, "Expected 1")
	require.Equal(t, 0, testThree, "Expected 0")
	removeThree = c.AddOnPublishReceived(func(_ PublishReceived) (bool, error) { testThree++; return false, nil })
	callAll(c)
	require.Equal(t, 2, testOne, "Expected 2")
	require.Equal(t, 2, testTwo, "Expected 2")
	require.Equal(t, 1, testThree, "Expected 1")
	removeThree()
	callAll(c)
	require.Equal(t, 3, testOne, "Expected 3")
	require.Equal(t, 3, testTwo, "Expected 3")
	require.Equal(t, 1, testThree, "Expected 1")
}

// basicClientInitialisation initialises a Client that will be used without calling Connect
// performs the least configuration possible such that calling `close()` will cleanly shutdown
func basicClientInitialisation(c *Client) context.Context {
	ctx, cancelFunc := context.WithCancel(context.Background())
	c.cancelFunc = cancelFunc
	done := make(chan struct{})
	c.done = done
	context.AfterFunc(ctx, func() { c.shutdown(done) })
	return ctx
}
