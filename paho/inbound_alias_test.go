/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
 *
 * All rights reserved. This program and the accompanying materials
 * are made available under the terms of the Eclipse Public License v2.0
 * and Eclipse Distribution License v1.0 which accompany this distribution.
 *
 * The Eclipse Public License is available at
 *    https://www.eclipse.org/legal/epl-2.0/
 * and the Eclipse Distribution License is available at
 *    http://www.eclipse.org/org/documents/edl-v10.php.
 *
 * AI Disclosure: This file was largely generated using OpenAI Codex and
 * refined through discussion and review. The AI-generated portions are
 * made available under CC0-1.0, rather than the project licences above.
 * Existing and human-authored portions retain their applicable licences.
 *
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI Codex
 */

package paho

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho/session/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// aliasTestConnection connects a real Client and session to an in-memory broker.
// max sets the advertised alias limit; resumed sets CONNACK's Session Present flag.
// It returns the client, the broker connection used to send packets, and a channel recording client acknowledgments
// and DISCONNECT packets. The broker leaves QoS 2 exchanges unfinished, so tests can resume them after reconnecting.
func aliasTestConnection(t *testing.T, cfg ClientConfig, max *uint16, resumed bool) (*Client, net.Conn, <-chan *packets.ControlPacket) {
	t.Helper()
	clientConn, brokerConn := net.Pipe()
	cfg.Conn = packets.NewThreadSafeConn(clientConn)
	broker := packets.NewThreadSafeConn(brokerConn)
	received := make(chan *packets.ControlPacket, 16)
	brokerDone := make(chan struct{})
	go func() {
		defer close(brokerDone)
		defer close(received)
		for {
			p, err := packets.ReadPacket(broker)
			if err != nil {
				return
			}
			switch p.Type {
			case packets.CONNECT:
				_, err = (&packets.Connack{SessionPresent: resumed, Properties: &packets.Properties{}}).WriteTo(broker)
			case packets.PINGREQ:
				_, err = (&packets.Pingresp{}).WriteTo(broker)
			default:
				received <- p
			}
			if err != nil {
				return
			}
		}
	}()
	c := NewClient(cfg)
	_, err := c.Connect(context.Background(), &Connect{ClientID: "aliases", Properties: &ConnectProperties{TopicAliasMaximum: max, SessionExpiryInterval: Uint32(600)}})
	require.NoError(t, err)
	t.Cleanup(func() {
		c.close()
		_ = broker.Close()
		<-brokerDone
	})
	return c, broker, received
}

// aliasTestSend sends a broker PUBLISH with a test payload and non-nil properties.
// It fails the test if the packet cannot be written.
func aliasTestSend(t *testing.T, broker net.Conn, p *packets.Publish) {
	t.Helper()
	// A nonempty payload also avoids a zero-byte net.Pipe write at packet end.
	p.Payload = []byte("reading")
	if p.Properties == nil {
		p.Properties = &packets.Properties{}
	}
	_, err := p.WriteTo(broker)
	require.NoError(t, err)
}

// aliasTestPacket waits for the next packet recorded by the broker and checks its type. A timeout or closed connection
// fails the test instead of leaving it blocked.
func aliasTestPacket(t *testing.T, ch <-chan *packets.ControlPacket, typ byte) *packets.ControlPacket {
	t.Helper()
	select {
	case p := <-ch:
		require.NotNil(t, p, "connection closed before expected packet")
		require.Equal(t, typ, p.Type)
		return p
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for broker packet")
		return nil
	}
}

// aliasTestPublish waits for a message delivered to an application handler. It fails the test if no message arrives
// before the timeout.
func aliasTestPublish(t *testing.T, ch <-chan *Publish) *Publish {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for resolved PUBLISH")
		return nil
	}
}

// TestClientInboundAliasesAutomatic checks that configured callbacks, routers, and callbacks added later all receive
// resolved topics without an alias handler.
func TestClientInboundAliasesAutomatic(t *testing.T) {
	for _, mode := range []string{"callbacks", "configured router", "default router", "router callback"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				delivered := make(chan *Publish, 4)
				handle := func(pr PublishReceived) (bool, error) { delivered <- pr.Packet; return true, nil }
				cfg := ClientConfig{}
				switch mode {
				case "callbacks":
					cfg.OnPublishReceived = []func(PublishReceived) (bool, error){handle}
				case "configured router":
					cfg.Router = NewStandardRouterWithDefault(func(p *Publish) { delivered <- p })
				case "router callback":
					router := NewStandardRouterWithDefault(func(p *Publish) { delivered <- p })
					cfg.OnPublishReceived = []func(PublishReceived) (bool, error){func(pr PublishReceived) (bool, error) { router.Route(pr.Packet.Packet()); return true, nil }}
				}
				c, broker, _ := aliasTestConnection(t, cfg, Uint16(1), false)
				if mode == "default router" {
					c.AddOnPublishReceived(handle)
				}
				aliasTestSend(t, broker, &packets.Publish{Topic: "sensors/temperature", Properties: &packets.Properties{TopicAlias: Uint16(1)}})
				aliasTestSend(t, broker, &packets.Publish{Properties: &packets.Properties{TopicAlias: Uint16(1)}})
				for range 2 {
					assert.Equal(t, "sensors/temperature", aliasTestPublish(t, delivered).Topic)
				}
			})
		})
	}
}

// TestClientInboundAliasesKeepQueuedTopics holds application callbacks while an alias is reassigned. Queued messages
// must retain the topic that applied when each packet arrived, rather than picking up the alias's latest mapping.
func TestClientInboundAliasesKeepQueuedTopics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		delivered := make(chan *Publish, 4)
		release := make(chan struct{})
		var once sync.Once
		_, broker, _ := aliasTestConnection(t, ClientConfig{OnPublishReceived: []func(PublishReceived) (bool, error){func(pr PublishReceived) (bool, error) { <-release; delivered <- pr.Packet; return true, nil }}}, Uint16(1), false)
		defer once.Do(func() { close(release) })
		for _, topic := range []string{"old/topic", "", "new/topic", ""} {
			aliasTestSend(t, broker, &packets.Publish{Topic: topic, Properties: &packets.Properties{TopicAlias: Uint16(1)}})
		}
		synctest.Wait() // All four packets have been read while callbacks are held.
		once.Do(func() { close(release) })
		for _, want := range []string{"old/topic", "old/topic", "new/topic", "new/topic"} {
			assert.Equal(t, want, aliasTestPublish(t, delivered).Topic)
		}
	})
}

// TestClientInboundAliasOnQoS2Retransmission checks that a previously acknowledged message can register an alias after
// reconnecting without being delivered twice. It covers automatic and manual acknowledgments and checks that a DUP
// flag does not suppress a message the session has not previously acknowledged.
func TestClientInboundAliasOnQoS2Retransmission(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprintf("manualAck=%t", manual), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := state.NewInMemory()
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				delivered := make(chan *Publish, 4)
				cfg := ClientConfig{
					Session:                    s,
					EnableManualAcknowledgment: manual,
					OnPublishReceived: []func(PublishReceived) (bool, error){
						func(pr PublishReceived) (bool, error) {
							delivered <- pr.Packet
							return true, nil
						},
					},
				}
				c1, b1, wire1 := aliasTestConnection(t, cfg, Uint16(2), false)
				aliasTestSend(t, b1, &packets.Publish{QoS: 2, PacketID: 1, Topic: "sensors/temperature"})
				first := aliasTestPublish(t, delivered)
				if manual {
					require.NoError(t, c1.Ack(first))
				}
				aliasTestPacket(t, wire1, packets.PUBREC)
				c1.close() // Retain the PUBREC state, as if the broker never received it.

				c2, b2, wire2 := aliasTestConnection(t, cfg, Uint16(2), true)
				aliasTestSend(t, b2, &packets.Publish{QoS: 2, PacketID: 1, Duplicate: true, Topic: "sensors/temperature", Properties: &packets.Properties{TopicAlias: Uint16(1)}})
				aliasTestPacket(t, wire2, packets.PUBREC)
				select {
				case <-delivered:
					t.Fatal("QoS 2 retransmission was delivered twice")
				default:
				}
				aliasTestSend(t, b2, &packets.Publish{Properties: &packets.Properties{TopicAlias: Uint16(1)}})
				assert.Equal(t, "sensors/temperature", aliasTestPublish(t, delivered).Topic)

				// A DUP flag alone does not prove prior delivery: an unknown packet
				// ID must still reach the application.
				aliasTestSend(t, b2, &packets.Publish{QoS: 2, PacketID: 2, Duplicate: true, Topic: "new/topic", Properties: &packets.Properties{TopicAlias: Uint16(2)}})
				unseen := aliasTestPublish(t, delivered)
				assert.Equal(t, "new/topic", unseen.Topic)
				assert.True(t, unseen.Duplicate())
				if manual {
					require.NoError(t, c2.Ack(unseen))
				}
				aliasTestPacket(t, wire2, packets.PUBREC)
			})
		})
	}
}

// TestClientInboundAliasesRejectInvalid checks that invalid topics and aliases produce the expected DISCONNECT and
// reported error before delivery or acknowledgment.
// This must hold for every QoS, including retransmissions suppressed by the session.
func TestClientInboundAliasesRejectInvalid(t *testing.T) {
	tests := []struct {
		name    string
		maximum *uint16
		topic   string
		alias   *uint16
		reason  byte
	}{
		{"zero alias", Uint16(2), "topic", Uint16(0), packets.DisconnectTopicAliasInvalid},
		{"above maximum", Uint16(2), "topic", Uint16(3), packets.DisconnectTopicAliasInvalid},
		{"aliases disabled", Uint16(0), "topic", Uint16(1), packets.DisconnectTopicAliasInvalid},
		{"maximum absent", nil, "topic", Uint16(1), packets.DisconnectTopicAliasInvalid},
		{"unknown alias", Uint16(2), "", Uint16(1), packets.DisconnectProtocolError},
		{"empty topic without alias", Uint16(2), "", nil, packets.DisconnectProtocolError},
	}
	for _, tt := range tests {
		for _, delivery := range []string{"QoS0", "QoS1", "QoS2", "QoS2 duplicate"} {
			t.Run(tt.name+"/"+delivery, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s := state.NewInMemory()
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					delivered := make(chan *Publish, 4)
					errorsReceived := make(chan error, 1)
					cfg := ClientConfig{Session: s, OnPublishReceived: []func(PublishReceived) (bool, error){func(pr PublishReceived) (bool, error) { delivered <- pr.Packet; return true, nil }}, OnClientError: func(err error) {
						if _, ok := errors.AsType[*topicAliasError](err); ok {
							errorsReceived <- err
						}
					}}
					duplicate := delivery == "QoS2 duplicate"
					if duplicate {
						c1, b1, w1 := aliasTestConnection(t, cfg, tt.maximum, false)
						aliasTestSend(t, b1, &packets.Publish{QoS: 2, PacketID: 1, Topic: "topic"})
						aliasTestPublish(t, delivered)
						aliasTestPacket(t, w1, packets.PUBREC)
						c1.close()
					}
					c, broker, wire := aliasTestConnection(t, cfg, tt.maximum, duplicate)
					qos := byte(0)
					if delivery == "QoS1" {
						qos = 1
					} else if delivery != "QoS0" {
						qos = 2
					}
					aliasTestSend(t, broker, &packets.Publish{QoS: qos, PacketID: 1, Duplicate: duplicate, Topic: tt.topic, Properties: &packets.Properties{TopicAlias: tt.alias}})
					d := aliasTestPacket(t, wire, packets.DISCONNECT).Content.(*packets.Disconnect)
					assert.Equal(t, tt.reason, d.ReasonCode)
					select {
					case <-c.Done():
					case <-time.After(time.Second):
						t.Fatal("invalid alias did not shut down client")
					}
					select {
					case err := <-errorsReceived:
						assert.ErrorContains(t, err, d.Properties.ReasonString)
					case <-time.After(time.Second):
						t.Fatal("alias error was not reported")
					}
					synctest.Wait()
					assert.Empty(t, delivered, "invalid PUBLISH reached an application callback")
					for p := range wire {
						assert.NotContains(t, []byte{packets.PUBACK, packets.PUBREC}, p.Type, "invalid PUBLISH was acknowledged")
					}
				})
			})
		}
	}
}

// TestClientInboundAliasesAreConnectionScoped checks that reconnecting clears alias mappings even when both the MQTT
// session and the router are reused.
func TestClientInboundAliasesAreConnectionScoped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := state.NewInMemory()
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		delivered := make(chan *Publish, 4)
		// Reuse a router as AutoPaho does when creating clients for new connections.
		router := NewStandardRouterWithDefault(func(p *Publish) { delivered <- p })
		cfg := ClientConfig{Session: s, Router: router}
		c1, b1, _ := aliasTestConnection(t, cfg, Uint16(1), false)
		aliasTestSend(t, b1, &packets.Publish{Topic: "old/topic", Properties: &packets.Properties{TopicAlias: Uint16(1)}})
		assert.Equal(t, "old/topic", aliasTestPublish(t, delivered).Topic)
		c1.close()
		_, b2, w2 := aliasTestConnection(t, cfg, Uint16(1), true)
		aliasTestSend(t, b2, &packets.Publish{Properties: &packets.Properties{TopicAlias: Uint16(1)}})
		d := aliasTestPacket(t, w2, packets.DISCONNECT).Content.(*packets.Disconnect)
		assert.Equal(t, byte(packets.DisconnectProtocolError), d.ReasonCode)
		assert.Empty(t, delivered)
	})
}

// TestClientInboundAliasesWithSharedRouter checks that two active connections sharing a router can use the same alias
// number for different topics.
func TestClientInboundAliasesWithSharedRouter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		delivered := make(chan *Publish, 4)
		router := NewStandardRouterWithDefault(func(p *Publish) { delivered <- p })
		cfg := ClientConfig{Router: router}
		_, b1, _ := aliasTestConnection(t, cfg, Uint16(1), false)
		_, b2, _ := aliasTestConnection(t, cfg, Uint16(1), false)
		for i, broker := range []net.Conn{b1, b2} {
			topic := fmt.Sprintf("broker%d/topic", i)
			aliasTestSend(t, broker, &packets.Publish{Topic: topic, Properties: &packets.Properties{TopicAlias: Uint16(1)}})
			assert.Equal(t, topic, aliasTestPublish(t, delivered).Topic)
		}
		for i, broker := range []net.Conn{b1, b2} {
			aliasTestSend(t, broker, &packets.Publish{Properties: &packets.Properties{TopicAlias: Uint16(1)}})
			assert.Equal(t, fmt.Sprintf("broker%d/topic", i), aliasTestPublish(t, delivered).Topic)
		}
	})
}
