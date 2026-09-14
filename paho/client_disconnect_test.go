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
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eclipse/paho.golang/packets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// disconnectTestConnection leaves broker reads to the test after CONNACK, so
// tests can exercise a stalled writer.
func disconnectTestConnection(t *testing.T) (*Client, net.Conn, <-chan error) {
	t.Helper()
	clientConn, broker := net.Pipe()
	reported := make(chan error, 8)
	c := NewClient(ClientConfig{
		Conn:          packets.NewThreadSafeConn(clientConn),
		PacketTimeout: time.Second,
		OnClientError: func(err error) { reported <- err },
	})
	handshake := make(chan error, 1)
	go func() {
		_, err := packets.ReadPacket(broker)
		if err == nil {
			_, err = (&packets.Connack{Properties: &packets.Properties{}}).WriteTo(broker)
		}
		handshake <- err
	}()
	t.Cleanup(func() { _ = broker.Close() })
	_, err := c.Connect(context.Background(), &Connect{ClientID: "aliases", Properties: &ConnectProperties{TopicAliasMaximum: Uint16(1)}})
	require.NoError(t, err)
	require.NoError(t, <-handshake)
	t.Cleanup(c.close)
	return c, broker, reported
}

// TestDisconnectStalledWriter checks that `disconnect` will close the connection even if the writer is stalled.
// There is a proposal for a breaking change to replace `Disconnect` with `disconnect`.
func TestDisconnectStalledWriter(t *testing.T) {
	// Use real time: waiting on the connection mutex prevents synctest's
	// fake clock from advancing.
	for _, trigger := range []string{"direct", "invalid alias"} {
		for _, pendingPublish := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pendingPublish=%t", trigger, pendingPublish), func(t *testing.T) {
				c, broker, reported := disconnectTestConnection(t)
				if pendingPublish {
					go func() {
						_, _ = c.Publish(context.Background(), &Publish{QoS: 1, Topic: "outbound", Payload: []byte("data")})
					}()
					// Read only the first byte, leaving PUBLISH blocked while
					// it holds the connection's write lock.
					require.NoError(t, broker.SetReadDeadline(time.Now().Add(time.Second)))
					_, err := io.ReadFull(broker, make([]byte, 1))
					require.NoError(t, err)
				}
				result := make(chan error, 1)
				if trigger == "direct" {
					ctx, cancel := context.WithTimeout(context.Background(), c.config.PacketTimeout)
					defer cancel()
					go func() { result <- c.disconnect(ctx, &Disconnect{}) }()
				} else {
					aliasTestSend(t, broker, &packets.Publish{Topic: "topic", Properties: &packets.Properties{TopicAlias: Uint16(0)}})
				}
				select {
				case <-c.Done():
				case <-time.After(3 * c.config.PacketTimeout):
					t.Fatal("DISCONNECT did not shut down client while broker stopped reading")
				}
				if trigger == "direct" {
					select {
					case err := <-result:
						require.ErrorIs(t, err, context.DeadlineExceeded)
						require.ErrorIs(t, err, io.ErrClosedPipe)
					case <-time.After(time.Second):
						t.Fatal("Disconnect did not return after shutdown")
					}
				} else {
					select {
					case err := <-reported:
						_, ok := errors.AsType[*topicAliasError](err)
						require.True(t, ok, "alias error missing from %v", err)
						assert.ErrorContains(t, err, "topic alias must be greater than zero")
						assert.ErrorContains(t, err, "sending topic alias disconnect")
					case <-time.After(time.Second):
						t.Fatal("alias error was not reported")
					}
				}
			})
		}
	}
}

// TestDisconnectCancelledContext confirms that disconnect functions with an already cancelled context
func TestDisconnectCancelledContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _, _ := disconnectTestConnection(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.ErrorIs(t, c.disconnect(ctx, &Disconnect{}), context.Canceled)
		select {
		case <-c.Done():
		default:
			t.Fatal("disconnect returned before shutdown completed")
		}
	})
}

// TestDisconnectContextWaitsForShutdown confirms that `disconnect` does not return before the client shuts down
func TestDisconnectContextWaitsForShutdown(t *testing.T) {
	for _, writeCompletes := range []bool{false, true} {
		t.Run(fmt.Sprintf("writeCompletes=%t", writeCompletes), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, broker, _ := disconnectTestConnection(t)
				entered := make(chan struct{})
				release := make(chan struct{})
				releaseCallback := sync.OnceFunc(func() { close(release) })
				defer releaseCallback()
				c.AddOnPublishReceived(func(PublishReceived) (bool, error) {
					close(entered)
					<-release
					return true, nil
				})
				aliasTestSend(t, broker, &packets.Publish{Topic: "topic"})
				<-entered

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() { result <- c.disconnect(ctx, &Disconnect{}) }()
				if writeCompletes {
					packet, err := packets.ReadPacket(broker)
					require.NoError(t, err)
					require.Equal(t, byte(packets.DISCONNECT), packet.Type)
				}
				synctest.Wait() // Waiting either on the write or on the blocked callback.
				cancel()
				synctest.Wait()
				select {
				case <-result:
					t.Fatal("disconnect returned while a publish callback was still running")
				default:
				}
				select {
				case <-c.Done():
					t.Fatal("client finished shutdown while a publish callback was still running")
				default:
				}

				releaseCallback()
				select {
				case err := <-result:
					if writeCompletes {
						require.NoError(t, err, "cancellation during shutdown must not change a successful send")
					} else {
						require.ErrorIs(t, err, context.Canceled)
						require.ErrorIs(t, err, io.ErrClosedPipe)
					}
				case <-time.After(time.Second):
					t.Fatal("disconnect did not return after the callback completed")
				}
				select {
				case <-c.Done():
				default:
					t.Fatal("disconnect returned before shutdown completed")
				}
			})
		})
	}
}
