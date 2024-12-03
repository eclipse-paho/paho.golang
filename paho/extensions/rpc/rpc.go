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

	"github.com/canonical/mqtt.golang/paho"
)

// Handler is the struct providing a request/response functionality for the paho
// MQTT v5 client
type Handler struct {
	sync.Mutex
	c          *paho.Client
	correlData map[string]chan *paho.Publish
}

func NewHandler(ctx context.Context, c *paho.Client) (*Handler, error) {
	h := &Handler{
		c:          c,
		correlData: make(map[string]chan *paho.Publish),
	}

	responseTopic := fmt.Sprintf("%s/responses", c.ClientID())
	c.AddOnPublishReceived(func(pr paho.PublishReceived) (bool, error) {
		if pr.Packet.Topic == responseTopic {
			h.responseHandler(pr.Packet)
			return true, nil
		}
		return false, nil
	})

	_, err := c.Subscribe(ctx, &paho.Subscribe{
		Subscriptions: []paho.SubscribeOptions{
			{Topic: fmt.Sprintf("%s/responses", c.ClientID()), QoS: 1},
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

func (h *Handler) Request(ctx context.Context, pb *paho.Publish) (*paho.Publish, error) {
	cID := fmt.Sprintf("%d", time.Now().UnixNano())
	rChan := make(chan *paho.Publish, 1) // Buffered to prevent goroutine leak when context cancelled

	h.addCorrelID(cID, rChan)

	if pb.Properties == nil {
		pb.Properties = &paho.PublishProperties{}
	}

	pb.Properties.CorrelationData = []byte(cID)
	pb.Properties.ResponseTopic = fmt.Sprintf("%s/responses", h.c.ClientID())
	pb.Retain = false

	_, err := h.c.Publish(ctx, pb)
	if err != nil {
		return nil, err
	}

	select {
	case resp := <-rChan:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
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
