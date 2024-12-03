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
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/canonical/mqtt.golang/autopaho"
	"github.com/canonical/mqtt.golang/autopaho/queue"
	"github.com/canonical/mqtt.golang/autopaho/queue/file"
	"github.com/canonical/mqtt.golang/autopaho/queue/memory"
	"github.com/canonical/mqtt.golang/paho"
)

// the Queue interface does not include WaitForEmpty because it's not needed in autopaho; however it's
// useful here, and both implementations used offer the function.
type queueWaitForEmpty interface {
	queue.Queue
	WaitForEmpty() chan struct{}
}

// publish connects to the server and publishes the requested number of messages
// The body of each message will be the Varint encoded message count (1 - msgCount)
func publish(ctx context.Context, serverURL *url.URL, msgCount uint64) {
	dropAt := make(map[uint64]bool)
	for _, i := range disconnectAtCount {
		dropAt[i] = true
	}

	var q queueWaitForEmpty
	var err error
	if useMemoryQueue {
		q = memory.New()
	} else {
		// Store queue files in the current folder (a bit messy but makes it obvious if any files are left behind)
		q, err = file.New("./", "queue", ".msg")
		if err != nil {
			panic(err)
		}
	}

	// Previous runs of the test may have left messages queued; remove them!
	for {
		entry, err := q.Peek()
		if errors.Is(err, queue.ErrEmpty) {
			break
		}
		if err != nil {
			panic(err)
		}
		if err := entry.Remove(); err != nil {
			panic(err)
		}
	}

	cliCfg := autopaho.ClientConfig{
		Queue:                         q,
		ServerUrls:                    []*url.URL{serverURL},
		KeepAlive:                     20,   // Keepalive message should be sent every 20 seconds
		CleanStartOnInitialConnection: true, // Previous tests should not contaminate this one!
		SessionExpiryInterval:         60,   // If connection drops we want session to remain live whilst we reconnect
		OnConnectionUp: func(cm *autopaho.ConnectionManager, connAck *paho.Connack) {
			fmt.Println("publish: mqtt connection up")
		},
		OnConnectError: func(err error) { fmt.Printf("publish: error whilst attempting connection: %s\n", err) },
		Errors:         logger{prefix: "publish"},
		// Debug:          logger{prefix: "publish: debug"},
		PahoErrors: logger{prefix: "publishP"},
		// PahoDebug:      logger{prefix: "publishP: debug"},
		// canonical/mqtt.golang/paho provides base mqtt functionality, the below config will be passed in for each connection
		ClientConfig: paho.ClientConfig{
			ClientID:      "TestPub",
			OnClientError: func(err error) { fmt.Printf("publish: client error: %s\n", err) },
			OnServerDisconnect: func(d *paho.Disconnect) {
				if d.Properties != nil {
					fmt.Printf("publish: server requested disconnect: %s\n", d.Properties.ReasonString)
				} else {
					fmt.Printf("publish:server requested disconnect; reason code: %d\n", d.ReasonCode)
				}
			},
		},
	}

	c, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		panic(err)
	}
	// We do not wait for the connection to come up; we just start publishing and allow autopaho to handle things
	for i := uint64(1); i <= msgCount; i++ {
		if dropAt[i] {
			c.TerminateConnectionForTest()
		}
		if err = c.PublishViaQueue(ctx, &autopaho.QueuePublish{
			Publish: &paho.Publish{
				QoS:     QOS,
				Topic:   testTopic,
				Payload: binary.AppendUvarint([]byte{}, i),
			},
		}); err != nil {
			panic(err)
		}

		// The queue relies on the file ModTime to work out what file is oldest; this means the resolution of
		// update times becomes important. To ensure order is maintained, we add a delay between calls to `publish`
		// (in real systems it's unlikely messages will be written more quickly).
		time.Sleep(time.Millisecond)

		if ctx.Err() != nil {
			fmt.Println("publish: Aborting due to context")
			return
		}
	}

	fmt.Println("publish: Messages queued")
	q.WaitForEmpty()
	fmt.Println("publish: Messages all sent or inflight")

	// There is no simple way for us to wait for the messages to be sent (i.e. waiting for QOS2 process to complete),
	// so, we just wait on the context
	<-ctx.Done()
	fmt.Println("publish: Context cancelled")
	<-c.Done()
	fmt.Println("publish: Clean Shutdown")
}
