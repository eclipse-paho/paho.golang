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

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/autopaho/examples/routercontext/middleware"
	"github.com/eclipse/paho.golang/paho"
)

const clientID = "PahoGoClient" // Change this to something random if using a public test server

// This example demonstrates the use of a StandardRouter; please note that the router API is likely to change
// prior to the release of v1.0.

func main() {
	// App will run until cancelled by user (e.g. ctrl-c)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// We will connect to the Mosquitto test server (note that you may see messages that other users publish)
	u, err := url.Parse("mqtt://test.mosquitto.org:1883")
	if err != nil {
		panic(err)
	}

	router := paho.NewStandardContextRouter()

	// Registering middleware handlers
	router.Use(middleware.Logger)
	router.Use(middleware.Recoverer)

	router.DefaultHandler(func(ctx context.Context, p *paho.Publish) {
		slog.InfoContext(ctx, fmt.Sprintf("defaulthandler received message with topic: %s", p.Topic))
	})

	cliCfg := autopaho.ClientConfig{
		ServerUrls: []*url.URL{u},
		KeepAlive:  20, // Keepalive message should be sent every 20 seconds
		// We don't want the broker to delete any session info when we disconnect
		CleanStartOnInitialConnection: true,
		SessionExpiryInterval:         0,
		OnConnectError:                func(err error) { fmt.Printf("error whilst attempting connection: %s\n", err) },
		// eclipse/paho.golang/paho provides base mqtt functionality, the below config will be passed in for each connection
		ClientConfig: paho.ClientConfig{
			// If you are using QOS 1/2, then it's important to specify a client id (which must be unique)
			ClientID: clientID,
			// OnPublishReceived is a slice of functions that will be called when a message is received.
			// You can write the function(s) yourself or use the supplied Router
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					router.Route(pr.Packet.Packet())
					return true, nil // we assume that the router handles all messages (todo: amend router API)
				}},
			OnClientError: func(err error) { fmt.Printf("client error: %s\n", err) },
			OnServerDisconnect: func(d *paho.Disconnect) {
				if d.Properties != nil {
					fmt.Printf("server requested disconnect: %s\n", d.Properties.ReasonString)
				} else {
					fmt.Printf("server requested disconnect; reason code: %d\n", d.ReasonCode)
				}
			},
		},
	}

	c, err := autopaho.NewConnection(ctx, cliCfg) // starts process; will reconnect until context cancelled
	if err != nil {
		panic(err)
	}

	if err = c.AwaitConnection(ctx); err != nil {
		panic(err)
	}

	// In most cases subscribing in OnConnectionUp is recommended (so subscription will be re-established after
	// a reconnection. However, for the purposes of this demo subscribing here is simpler.
	if _, err := c.Subscribe(context.Background(), &paho.Subscribe{
		Subscriptions: []paho.SubscribeOptions{
			{Topic: "test/#", QoS: 1}, // For this example, we get all messages under test
		},
	}); err != nil {
		panic(fmt.Sprintf("failed to subscribe (%s). This is likely to mean no messages will be received.", err))
	}

	// Handlers can be registered/deregistered at any time. It's important to note that you need to subscribe AND create
	// a handler
	router.RegisterHandler("test/test/#", func(ctx context.Context, p *paho.Publish) {
		slog.InfoContext(ctx, fmt.Sprintf("test/test/# received message with topic: %s", p.Topic))
	})
	router.RegisterHandler("test/test/foo", func(ctx context.Context, p *paho.Publish) {
		slog.InfoContext(ctx, fmt.Sprintf("test/test/foo received message with topic: %s", p.Topic))
	})
	router.RegisterHandler("test/nomatch", func(ctx context.Context, p *paho.Publish) {
		slog.InfoContext(ctx, fmt.Sprintf("test/nomatch received message with topic: %s", p.Topic))
	})
	router.RegisterHandler("test/panic", func(ctx context.Context, p *paho.Publish) {
		slog.InfoContext(ctx, fmt.Sprintf("test/panic received message with topic: %s", p.Topic))
		panic("This is a panic!")
	})
	router.RegisterHandler("test/quit", func(_ context.Context, p *paho.Publish) { stop() }) // Context will be cancelled if we receive a matching message

	// We publish three messages to test out the various route handlers
	topics := []string{"test/test", "test/test/foo", "test/xxNoMatch", "test/panic", "test/quit"}
	for _, t := range topics {
		if _, err := c.Publish(ctx, &paho.Publish{
			QoS:     1,
			Topic:   t,
			Payload: []byte("TestMessage on topic: " + t),
		}); err != nil {
			if ctx.Err() == nil {
				panic(err) // Publish will exit when context cancelled or if something went wrong
			}
		}
	}

	<-c.Done() // Wait for clean shutdown (cancelling the context triggered the shutdown)
}
