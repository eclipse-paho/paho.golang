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
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/canonical/mqtt.golang/autopaho"
	"github.com/canonical/mqtt.golang/autopaho/extensions/rpc"
	"github.com/canonical/mqtt.golang/paho"
)

// rpc - demonstrates the use of autopaho/extensions/rpc
// Sets up a listener that responds to messages on a topic (defaults to `rpc/request`). The request body should be a
// simple math equation in JSON format (e.g. `{"function":"mul", "param1": 10, "param2": 5}`).
//
// A second connection to the broker is then established and a few sample equations sent.

type Request struct {
	Function string `json:"function"`
	Param1   int    `json:"param1"`
	Param2   int    `json:"param2"`
}

type Response struct {
	Value int `json:"value"`
}

const qos = 0

func listener(ctx context.Context, cliCfg autopaho.ClientConfig, topic string, qos byte) {
	initialSubscriptionMade := make(chan struct{}) // Closed when subscription made (otherwise we might send request before subscription in place)
	var initialSubscriptionOnce sync.Once          // We only want to close the above once!

	// Subscribing in OnConnectionUp is the recommended approach because this ensures the subscription is reestablished
	// following reconnection (the subscription should survive `cliCfg.SessionExpiryInterval` after disconnection,
	// but in this case that is 0, and it's safer if we don't assume the session survived anyway).
	cliCfg.OnConnectionUp = func(cm *autopaho.ConnectionManager, connAck *paho.Connack) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(5*time.Second))
		defer cancel()
		if _, err := cm.Subscribe(ctx, &paho.Subscribe{
			Subscriptions: []paho.SubscribeOptions{
				{Topic: topic, QoS: qos},
			},
		}); err != nil {
			fmt.Printf("listener failed to subscribe (%s). This is likely to mean no messages will be received.", err)
			return
		}
		initialSubscriptionOnce.Do(func() { close(initialSubscriptionMade) })
	}
	cliCfg.OnPublishReceived = []func(paho.PublishReceived) (bool, error){
		func(received paho.PublishReceived) (bool, error) {
			if received.Packet.Properties != nil && received.Packet.Properties.CorrelationData != nil && received.Packet.Properties.ResponseTopic != "" {
				log.Printf("Received message with response topic %s and correl id %s\n%s", received.Packet.Properties.ResponseTopic, string(received.Packet.Properties.CorrelationData), string(received.Packet.Payload))

				var r Request
				var resp Response

				if err := json.NewDecoder(bytes.NewReader(received.Packet.Payload)).Decode(&r); err != nil {
					log.Printf("Failed to decode Request: %v", err)
				}

				switch r.Function {
				case "add":
					resp.Value = r.Param1 + r.Param2
				case "mul":
					resp.Value = r.Param1 * r.Param2
				case "div":
					resp.Value = r.Param1 / r.Param2
				case "sub":
					resp.Value = r.Param1 - r.Param2
				}

				body, _ := json.Marshal(resp)
				_, err := received.Client.Publish(ctx, &paho.Publish{
					Properties: &paho.PublishProperties{
						CorrelationData: received.Packet.Properties.CorrelationData,
					},
					Topic:   received.Packet.Properties.ResponseTopic,
					Payload: body,
				})
				if err != nil {
					log.Fatalf("failed to publish message: %s", err)
				}
			}
			return true, nil
		},
	}

	_, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		panic(err)
	}

	// Connection must be up, and subscription made, within a reasonable time period.
	// In a real app you would probably not wait for the subscription, but it's important here because otherwise the
	// request colud be sent before the subscription is in place.
	connCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case <-connCtx.Done():
		log.Fatalf("listener failed to connect & subscribe: %s", err)
	case <-initialSubscriptionMade:
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	server := flag.String("server", "mqtt://mqtt.eclipseprojects.io:1883", "The full URL of the MQTT server to connect to")
	rTopic := flag.String("rtopic", "rpc/request", "Topic for requests to go to")
	flag.Parse()

	serverUrl, err := url.Parse(*server)
	if err != nil {
		panic(err)
	}

	genericCfg := autopaho.ClientConfig{
		ServerUrls:       []*url.URL{serverUrl},
		KeepAlive:        30,
		ReconnectBackoff: autopaho.NewConstantBackoff(2 * time.Second),
		ConnectTimeout:   5 * time.Second,
		OnConnectError:   func(err error) { fmt.Printf("error whilst attempting connection: %s\n", err) },
		ClientConfig: paho.ClientConfig{
			OnClientError: func(err error) { fmt.Printf("requested disconnect: %s\n", err) },
			OnServerDisconnect: func(d *paho.Disconnect) {
				if d.Properties != nil {
					fmt.Printf("requested disconnect: %s\n", d.Properties.ReasonString)
				} else {
					fmt.Printf("requested disconnect; reason code: %d\n", d.ReasonCode)
				}
			},
		},
	}

	lCfg := genericCfg
	lCfg.ClientConfig.ClientID = "rpc-listener"
	listener(ctx, lCfg, *rTopic, qos) // Start the listener (this will respond to requests)

	cliCfg := genericCfg
	cliCfg.ClientID = "rpc-requestor"

	initialSubscriptionMade := make(chan struct{}) // Closed when subscription made (otherwise we might send request before subscription in place)
	var initialSubscriptionOnce sync.Once          // We only want to close the above once!

	cliCfg.OnConnectionUp = func(cm *autopaho.ConnectionManager, connAck *paho.Connack) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(5*time.Second))
		defer cancel()
		if _, err := cm.Subscribe(ctx, &paho.Subscribe{
			Subscriptions: []paho.SubscribeOptions{
				{Topic: fmt.Sprintf("%s/responses", cliCfg.ClientID), QoS: qos},
			},
		}); err != nil {
			fmt.Printf("requestor failed to subscribe (%s). This is likely to mean no messages will be received.", err)
			return
		}
		initialSubscriptionOnce.Do(func() { close(initialSubscriptionMade) })
	}

	router := paho.NewStandardRouter()
	cliCfg.OnPublishReceived = []func(paho.PublishReceived) (bool, error){
		func(p paho.PublishReceived) (bool, error) {
			router.Route(p.Packet.Packet())
			return false, nil
		},
	}

	cm, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		panic(err)
	}

	// Wait for the subscription to be made (otherwise we may miss the response!)
	connCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case <-connCtx.Done():
		log.Fatalf("requestor failed to connect & subscribe: %s", err)
	case <-initialSubscriptionMade:
	}

	h, err := rpc.NewHandler(ctx, rpc.HandlerOpts{
		Conn:             cm,
		Router:           router,
		ResponseTopicFmt: "%s/responses",
		ClientID:         cliCfg.ClientID,
	})
	if err != nil {
		log.Fatal(err)
	}

	resp, err := h.Request(ctx, &paho.Publish{
		Topic:   *rTopic,
		Payload: []byte(`{"function":"mul", "param1": 10, "param2": 5}`),
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Received response: %s", string(resp.Payload))

	resp, err = h.Request(ctx, &paho.Publish{
		Topic:   *rTopic,
		Payload: []byte(`{"function":"mul", "param1": 10, "param2": 5}`),
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Received response: %s", string(resp.Payload))

	resp, err = h.Request(ctx, &paho.Publish{
		Topic:   *rTopic,
		Payload: []byte(`{"function":"add", "param1": 5, "param2": 7}`),
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Received response: %s", string(resp.Payload))
}
