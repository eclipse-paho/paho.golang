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

// Connect to the server, subscribe, and write messages received to a file

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/canonical/mqtt.golang/autopaho"
	"github.com/canonical/mqtt.golang/paho"
	"github.com/canonical/mqtt.golang/paho/session/state"
	storefile "github.com/canonical/mqtt.golang/paho/store/file"
)

func main() {
	cfg, err := getConfig()
	if err != nil {
		panic(err)
	}

	// Create a handler that will deal with incoming messages
	h := NewHandler(cfg.writeToDisk, cfg.outputFileName, cfg.writeToStdOut)
	defer h.Close()

	var sessionState *state.State
	if len(cfg.sessionFolder) == 0 {
		sessionState = state.NewInMemory()
	} else {
		cliState, err := storefile.New(cfg.sessionFolder, "subdemo_cli_", ".pkt")
		if err != nil {
			panic(err)
		}
		srvState, err := storefile.New(cfg.sessionFolder, "subdemo_srv_", ".pkt")
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
		OnConnectionUp: func(cm *autopaho.ConnectionManager, connAck *paho.Connack) {
			fmt.Println("mqtt connection up")
			if _, err := cm.Subscribe(context.Background(), &paho.Subscribe{
				Subscriptions: []paho.SubscribeOptions{
					{Topic: cfg.topic, QoS: cfg.qos},
				},
			}); err != nil {
				fmt.Printf("failed to subscribe (%s). This is likely to mean no messages will be received.", err)
				return
			}
			fmt.Println("mqtt subscription made")
		},
		OnConnectError: func(err error) { fmt.Printf("error whilst attempting connection: %s\n", err) },
		ClientConfig: paho.ClientConfig{
			ClientID: cfg.clientID,
			Session:  sessionState,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					h.handle(pr.Packet)
					return true, nil
				},
			},
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

	//
	// Connect to the server
	//
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cm, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		panic(err)
	}

	// Messages will be handled through the callback so we really just need to wait until a shutdown
	// is requested
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	signal.Notify(sig, syscall.SIGTERM)

	<-sig
	fmt.Println("signal caught - exiting")

	// We could cancel the context at this point but will call Disconnect instead (this waits for autopaho to shutdown)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = cm.Disconnect(ctx)

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
