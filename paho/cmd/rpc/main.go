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
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/canonical/mqtt.golang/paho"
	"github.com/canonical/mqtt.golang/paho/extensions/rpc"
)

func init() {
	ic := make(chan os.Signal, 1)
	signal.Notify(ic, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ic
		os.Exit(0)
	}()
}

type Request struct {
	Function string `json:"function"`
	Param1   int    `json:"param1"`
	Param2   int    `json:"param2"`
}

type Response struct {
	Value int `json:"value"`
}

func listener(server, rTopic, username, password string) {
	var v sync.WaitGroup

	v.Add(1)

	go func() {
		conn, err := net.Dial("tcp", server)
		if err != nil {
			log.Fatalf("Failed to connect to %s: %s", server, err)
		}

		c := paho.NewClient(paho.ClientConfig{
			Conn: conn,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					m := pr.Packet
					if m.Properties != nil && m.Properties.CorrelationData != nil && m.Properties.ResponseTopic != "" {
						log.Printf("Received message with response topic %s and correl id %s\n%s", m.Properties.ResponseTopic, string(m.Properties.CorrelationData), string(m.Payload))

						var r Request
						var resp Response

						if err := json.NewDecoder(bytes.NewReader(m.Payload)).Decode(&r); err != nil {
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
						_, err := pr.Client.Publish(context.Background(), &paho.Publish{
							Properties: &paho.PublishProperties{
								CorrelationData: m.Properties.CorrelationData,
							},
							Topic:   m.Properties.ResponseTopic,
							Payload: body,
						})
						if err != nil {
							log.Fatalf("failed to publish message: %s", err)
						}
						return true, nil
					}
					return false, nil
				},
			},
		})

		cp := &paho.Connect{
			KeepAlive:  30,
			CleanStart: true,
			ClientID:   "listen1",
			Username:   username,
			Password:   []byte(password),
		}

		if username != "" {
			cp.UsernameFlag = true
		}
		if password != "" {
			cp.PasswordFlag = true
		}

		ca, err := c.Connect(context.Background(), cp)
		if err != nil {
			log.Fatalln(err)
		}
		if ca.ReasonCode != 0 {
			log.Fatalf("Failed to connect to %s : %d - %s", server, ca.ReasonCode, ca.Properties.ReasonString)
		}

		fmt.Printf("Connected to %s\n", server)

		_, err = c.Subscribe(context.Background(), &paho.Subscribe{
			Subscriptions: []paho.SubscribeOptions{
				{Topic: rTopic, QoS: 0},
			},
		})
		if err != nil {
			log.Fatalf("failed to subscribe: %s", err)
		}

		v.Done()

		for {
			time.Sleep(1 * time.Second)
		}
	}()

	v.Wait()
}

func main() {
	server := flag.String("server", "127.0.0.1:1883", "The full URL of the MQTT server to connect to")
	rTopic := flag.String("rtopic", "rpc/request", "Topic for requests to go to")
	username := flag.String("username", "", "A username to authenticate to the MQTT server")
	password := flag.String("password", "", "Password to match username")
	flag.Parse()

	// paho.SetDebugLogger(log.New(os.Stderr, "RPC: ", log.LstdFlags))

	listener(*server, *rTopic, *username, *password)

	conn, err := net.Dial("tcp", *server)
	if err != nil {
		log.Fatalf("Failed to connect to %s: %s", *server, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := paho.NewClient(paho.ClientConfig{
		OnPublishReceived: []func(paho.PublishReceived) (bool, error){ // Noop handler
			func(pr paho.PublishReceived) (bool, error) {
				return true, nil
			}},
		Conn: conn,
	})

	cp := &paho.Connect{
		KeepAlive:  30,
		CleanStart: true,
		Username:   *username,
		Password:   []byte(*password),
	}

	if *username != "" {
		cp.UsernameFlag = true
	}
	if *password != "" {
		cp.PasswordFlag = true
	}

	ca, err := c.Connect(context.Background(), cp)
	if err != nil {
		log.Fatalln(err)
	}
	if ca.ReasonCode != 0 {
		log.Fatalf("Failed to connect to %s : %d - %s", *server, ca.ReasonCode, ca.Properties.ReasonString)
	}

	fmt.Printf("Connected to %s\n", *server)

	h, err := rpc.NewHandler(ctx, c)
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
}
