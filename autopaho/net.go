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
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/canonical/mqtt.golang/packets"
	"github.com/canonical/mqtt.golang/paho"

	"golang.org/x/net/proxy"
)

// Network (establishing connection) functionality for AutoPaho

// establishServerConnection - establishes a connection with the MQTT server retrying until successful or the
// context is cancelled (in which case nil will be returned).
func establishServerConnection(ctx context.Context, cfg ClientConfig, firstConnection bool) (*paho.Client, *paho.Connack) {
	// Note: We do not touch b.cli in order to avoid adding thread safety issues.

	var attempt int = 0
	for {
		// Delay before attempting connection
		select {
		case <-time.After(cfg.ReconnectBackoff(attempt)):
		case <-ctx.Done():
			return nil, nil
		}
		for _, u := range cfg.ServerUrls {
			var connack *paho.Connack

			cp, err := cfg.buildConnectPacket(firstConnection, u)
			if err == nil {
				connectionCtx, cancelConnCtx := context.WithTimeout(ctx, cfg.ConnectTimeout)

				if cfg.AttemptConnection != nil { // Use custom function if it is provided
					cfg.Conn, err = cfg.AttemptConnection(ctx, cfg, u)
				} else {
					switch strings.ToLower(u.Scheme) {
					case "mqtt", "tcp", "":
						cfg.Conn, err = attemptTCPConnection(connectionCtx, u.Host)
					case "ssl", "tls", "mqtts", "mqtt+ssl", "tcps":
						cfg.Conn, err = attemptTLSConnection(connectionCtx, cfg.TlsCfg, u.Host)
					case "ws":
						cfg.Conn, err = attemptWebsocketConnection(connectionCtx, nil, cfg.WebSocketCfg, u)
					case "wss":
						cfg.Conn, err = attemptWebsocketConnection(connectionCtx, cfg.TlsCfg, cfg.WebSocketCfg, u)
					default:
						if cfg.OnConnectError != nil {
							cfg.OnConnectError(fmt.Errorf("unsupported scheme (%s) user in url %s", u.Scheme, u.String()))
						}
						cancelConnCtx()
						continue
					}
				}

				if err == nil {
					cli := paho.NewClient(cfg.ClientConfig)
					if cfg.PahoDebug != nil {
						cli.SetDebugLogger(cfg.PahoDebug)
					}

					if cfg.PahoErrors != nil {
						cli.SetErrorLogger(cfg.PahoErrors)
					}

					connack, err = cli.Connect(connectionCtx, cp) // will return an error if the connection is unsuccessful (checks the reason code)
					if err == nil {                               // Successfully connected
						cancelConnCtx()
						return cli, connack
					}
				}
				cancelConnCtx()
			}

			// Possible failure was due to outer context being cancelled
			if ctx.Err() != nil {
				return nil, nil
			}
			cfg.Debug.Printf("failed to connect to %s: %s", u.String(), err)

			if cfg.OnConnectError != nil {
				cerr := fmt.Errorf("failed to connect to %s: %w", u.String(), err)
				if connack != nil {
					cerr = NewConnackError(err, connack)
				}
				cfg.OnConnectError(cerr)
			}
		}

		attempt++
	}
}

// attemptTCPConnection - makes a single attempt at establishing a TCP connection with the server
func attemptTCPConnection(ctx context.Context, address string) (net.Conn, error) {
	allProxy := os.Getenv("all_proxy")
	if len(allProxy) == 0 {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", address)
	}
	// Note: if custom dialer does not implement proxy.ContextDialer, a new goroutine is blocked ("leaked")
	// until the provided implementation of Dial() times out
	return proxy.Dial(ctx, "tcp", address)
}

// attemptTLSConnection - makes a single attempt at establishing a TLS connection with the server
func attemptTLSConnection(ctx context.Context, tlsCfg *tls.Config, address string) (net.Conn, error) {
	allProxy := os.Getenv("all_proxy")
	if len(allProxy) == 0 {
		d := tls.Dialer{
			Config: tlsCfg,
		}
		conn, err := d.DialContext(ctx, "tcp", address)
		return packets.NewThreadSafeConn(conn), err
	}

	// Note: if custom dialer does not implement proxy.ContextDialer, a new goroutine is blocked ("leaked")
	// until the provided implementation of Dial() times out
	conn, err := proxy.Dial(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}

	tlsConn := tls.Client(conn, tlsCfg)

	err = tlsConn.Handshake()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	return packets.NewThreadSafeConn(tlsConn), err
}

// attemptWebsocketConnection - makes a single attempt at establishing a websocket connection with the server
func attemptWebsocketConnection(ctx context.Context, tlsc *tls.Config, cfg *WebSocketConfig, serverURL *url.URL) (net.Conn, error) {
	var dialer *websocket.Dialer
	var requestHeader http.Header
	if cfg != nil {
		if cfg.Dialer != nil {
			dialer = cfg.Dialer(serverURL, tlsc)
		}
		if cfg.Header != nil {
			requestHeader = cfg.Header(serverURL, tlsc)
		}
	}
	if dialer == nil {
		d := *websocket.DefaultDialer // Take a copy as we modify a few values
		d.TLSClientConfig = tlsc
		d.Subprotocols = []string{"mqtt"}
		dialer = &d
	}
	ws, _, err := dialer.DialContext(ctx, serverURL.String(), requestHeader)
	if err != nil {
		return nil, fmt.Errorf("websocket connection failed: %w", err)
	}

	wrapper := &websocketConnector{
		Conn:   ws,
		Locker: &sync.Mutex{},
	}
	return wrapper, err
}

// websocketConnector is a websocket wrapper so it satisfies the net.Conn interface so it is a
// drop in replacement of the golang.org/x/net/websocket package.
// Implementation guide taken from https://github.com/gorilla/websocket/issues/282
type websocketConnector struct {
	*websocket.Conn
	r   io.Reader
	rio sync.Mutex

	// Used by packets.ControlPacket.WriteTo to ensure thread-safe writes
	sync.Locker
}

// SetDeadline sets both the read and write deadlines
// Note: deadlines are fatal in websocket connections (so this does not really match net.Conn)
func (c *websocketConnector) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	err := c.SetWriteDeadline(t)
	return err
}

// Write writes data to the websocket
func (c *websocketConnector) Write(p []byte) (int, error) {
	err := c.WriteMessage(websocket.BinaryMessage, p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// Read reads the current websocket frame
func (c *websocketConnector) Read(p []byte) (int, error) {
	c.rio.Lock()
	defer c.rio.Unlock()
	for {
		if c.r == nil {
			// Advance to next message.
			var err error
			_, c.r, err = c.NextReader()
			if err != nil {
				return 0, err
			}
		}
		n, err := c.r.Read(p)
		if err == io.EOF {
			// At end of message.
			c.r = nil
			if n > 0 {
				return n, nil
			}
			// No data read, continue to next message.
			continue
		}
		return n, err
	}
}
