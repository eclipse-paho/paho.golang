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

package rpc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/canonical/mqtt.golang/autopaho"
	"github.com/canonical/mqtt.golang/paho"
)

// Handler is the struct providing a request/response functionality for the paho
// MQTT v5 client
type Handler struct {
	sync.Mutex
	cm            *autopaho.ConnectionManager
	correlData    map[string]chan *paho.Publish
	responseTopic string
}

type HandlerOpts struct {
	Conn             *autopaho.ConnectionManager
	Router           paho.Router
	ResponseTopicFmt string
	ClientID         string
}

func NewHandler(ctx context.Context, opts HandlerOpts) (*Handler, error) {
	h := &Handler{
		cm:         opts.Conn,
		correlData: make(map[string]chan *paho.Publish),
	}

	h.responseTopic = fmt.Sprintf(opts.ResponseTopicFmt, opts.ClientID)

	opts.Router.RegisterHandler(h.responseTopic, h.responseHandler)

	_, err := opts.Conn.Subscribe(ctx, &paho.Subscribe{
		Subscriptions: []paho.SubscribeOptions{
			{Topic: h.responseTopic, QoS: 1},
		},
	})
	if err != nil {
		return nil, err
	}

	return h, nil
}

func (h *Handler) addCorrelID(cID string, r chan *paho.Publish) {
	h.Lock()
	defer h.Unlock()

	h.correlData[cID] = r
}

func (h *Handler) getCorrelIDChan(cID string) chan *paho.Publish {
	h.Lock()
	defer h.Unlock()

	rChan := h.correlData[cID]
	delete(h.correlData, cID)

	return rChan
}

func (h *Handler) Request(ctx context.Context, pb *paho.Publish) (resp *paho.Publish, err error) {
	cID := fmt.Sprintf("%d", time.Now().UnixNano())
	rChan := make(chan *paho.Publish)

	h.addCorrelID(cID, rChan)

	if pb.Properties == nil {
		pb.Properties = &paho.PublishProperties{}
	}

	pb.Properties.CorrelationData = []byte(cID)
	pb.Properties.ResponseTopic = h.responseTopic
	pb.Retain = false

	_, err = h.cm.Publish(ctx, pb)
	if err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("context ended")
	case resp = <-rChan:
		return resp, nil
	}
}

func (h *Handler) responseHandler(pb *paho.Publish) {
	if pb.Properties == nil || pb.Properties.CorrelationData == nil {
		return
	}

	rChan := h.getCorrelIDChan(string(pb.Properties.CorrelationData))
	if rChan == nil {
		return
	}

	rChan <- pb
}
