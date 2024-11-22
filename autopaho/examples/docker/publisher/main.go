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
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/canonical/mqtt.golang/autopaho"
	"github.com/canonical/mqtt.golang/paho"
	"github.com/canonical/mqtt.golang/paho/log"
	"github.com/canonical/mqtt.golang/paho/session/state"
	storefile "github.com/canonical/mqtt.golang/paho/store/file"
)

// Connect to the server and publish a message periodically
func main() {
	cfg, err := getConfig()
	if err != nil {
		panic(err)
	}

	var sessionState *state.State
	if len(cfg.sessionFolder) == 0 {
		sessionState = state.NewInMemory()
	} else {
		cliState, err := storefile.New(cfg.sessionFolder, "pubdemo_cli_", ".pkt")
		if err != nil {
			panic(err)
		}
		srvState, err := storefile.New(cfg.sessionFolder, "pubdemo_srv_", ".pkt")
		if err != nil {
			panic(err)
		}
		sessionState = state.New(cliState, srvState)
	}

	cliCfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{cfg.serverURL},
		KeepAlive:                     cfg.keepAlive,
		CleanStartOnInitialConnection: false, // the default
		SessionExpiryInterval:         60,    // Session remains live 60 seconds after disconnect
		ReconnectBackoff:              autopaho.NewConstantBackoff(cfg.connectRetryDelay),
		OnConnectionUp:                func(*autopaho.ConnectionManager, *paho.Connack) { fmt.Println("mqtt connection up") },
		OnConnectError:                func(err error) { fmt.Printf("error whilst attempting connection: %s\n", err) },
		Debug:                         log.NOOPLogger{},
		ClientConfig: paho.ClientConfig{
			ClientID:      cfg.clientID,
			Session:       sessionState,
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

	if cfg.debug {
		cliCfg.Debug = logger{prefix: "autoPaho"}
		cliCfg.PahoDebug = logger{prefix: "paho"}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Connect to the server - this will return immediately after initiating the connection process
	cm, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		panic(err)
	}

	var wg sync.WaitGroup

	// Start off a goRoutine that publishes messages
	wg.Add(1)
	go func() {
		defer wg.Done()
		var count uint64
		for {
			// AwaitConnection will return immediately if connection is up; adding this call stops publication whilst
			// connection is unavailable.
			err = cm.AwaitConnection(ctx)
			if err != nil { // Should only happen when context is cancelled
				fmt.Printf("publisher done (AwaitConnection: %s)\n", err)
				return
			}

			count += 1
			// The message could be anything; lets make it JSON containing a simple count (makes it simpler to track the messages)
			msg, err := json.Marshal(struct {
				Count uint64
			}{Count: count})
			if err != nil {
				panic(err)
			}

			// Publish will block, so we run it in a goRoutine
			// Note that we could use PublishViaQueue if we wanted to trust that the library will deliver (and, ideally,
			// use a file-based queue and state).
			go func(msg []byte) {
				pr, err := cm.Publish(ctx, &paho.Publish{
					QoS:     cfg.qos,
					Topic:   cfg.topic,
					Payload: msg,
				})
				if err != nil {
					fmt.Printf("error publishing message %s: %s\n", msg, err)
				} else if pr.ReasonCode != 0 && pr.ReasonCode != 16 { // 16 = Server received message but there are no subscribers
					fmt.Printf("reason code %d received for message %s\n", pr.ReasonCode, msg)
				} else if cfg.printMessages {
					fmt.Printf("sent message: %s\n", msg)
				}
			}(msg)

			select {
			case <-time.After(cfg.delayBetweenMessages):
			case <-ctx.Done():
				fmt.Println("publisher done")
				return
			}
		}
	}()

	// Wait for a signal before exiting
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	signal.Notify(sig, syscall.SIGTERM)

	<-sig
	fmt.Println("signal caught - exiting")
	cancel()

	wg.Wait()
	fmt.Println("shutdown complete")
}

// logger implements the paho.Logger interface
type logger struct {
	prefix string
}

// Println is the library provided NOOPLogger's
// implementation of the required interface function()
func (l logger) Println(v ...interface{}) {
	fmt.Println(append([]interface{}{l.prefix + ":"}, v...)...)
}

// Printf is the library provided NOOPLogger's
// implementation of the required interface function(){}
func (l logger) Printf(format string, v ...interface{}) {
	if len(format) > 0 && format[len(format)-1] != '\n' {
		format = format + "\n" // some log calls in paho do not add \n
	}
	fmt.Printf(l.prefix+":"+format, v...)
}
