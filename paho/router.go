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

package paho

import (
	"context"
	"strings"
	"sync"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho/log"
)

// MessageHandler is a type for a function that is invoked
// by a Router when it has received a Publish.
// MessageHandlers should complete quickly (start a go routine for
// long-running processes) and should not call functions within the
// paho instance that triggered them (due to potential deadlocks).
type MessageHandler func(*Publish)

// Router is an interface of the functions for a struct that is
// used to handle invoking MessageHandlers depending on the
// the topic the message was published on.
// RegisterHandler() takes a string of the topic, and a MessageHandler
// to be invoked when Publishes are received that match that topic
// UnregisterHandler() takes a string of the topic to remove
// MessageHandlers for
// Route() takes a Publish message and determines which MessageHandlers
// should be invoked
type Router interface {
	RegisterHandler(string, MessageHandler)
	UnregisterHandler(string)
	Route(*packets.Publish)
	SetDebugLogger(log.Logger)
}

// StandardRouter is a library provided implementation of a Router that
// allows for unique and multiple MessageHandlers per topic
type StandardRouter struct {
	sync.RWMutex
	defaultHandler MessageHandler
	subscriptions  map[string][]MessageHandler
	aliases        map[uint16]string
	debug          log.Logger
}

// NewStandardRouter instantiates and returns an instance of a StandardRouter
func NewStandardRouter() *StandardRouter {
	return &StandardRouter{
		subscriptions: make(map[string][]MessageHandler),
		aliases:       make(map[uint16]string),
		debug:         log.NOOPLogger{},
	}
}

// NewStandardRouterWithDefault instantiates and returns an instance of a StandardRouter
// with the default handler set to the value passed in (for convenience when creating
// handler inline).
func NewStandardRouterWithDefault(h MessageHandler) *StandardRouter {
	r := NewStandardRouter()
	r.DefaultHandler(h)
	return r
}

// RegisterHandler is the library provided StandardRouter's
// implementation of the required interface function()
func (r *StandardRouter) RegisterHandler(topic string, h MessageHandler) {
	r.debug.Println("registering handler for:", topic)
	r.Lock()
	defer r.Unlock()

	r.subscriptions[topic] = append(r.subscriptions[topic], h)
}

// UnregisterHandler is the library provided StandardRouter's
// implementation of the required interface function()
func (r *StandardRouter) UnregisterHandler(topic string) {
	r.debug.Println("unregistering handler for:", topic)
	r.Lock()
	defer r.Unlock()

	delete(r.subscriptions, topic)
}

// Route is the library provided StandardRouter's implementation
// of the required interface function()
func (r *StandardRouter) Route(pb *packets.Publish) {
	r.debug.Println("routing message for:", pb.Topic)
	r.RLock()
	defer r.RUnlock()

	m := PublishFromPacketPublish(pb)

	var topic string
	if pb.Properties.TopicAlias != nil {
		r.debug.Println("message is using topic aliasing")
		if pb.Topic != "" {
			// Register new alias
			r.debug.Printf("registering new topic alias '%d' for topic '%s'", *pb.Properties.TopicAlias, m.Topic)
			r.aliases[*pb.Properties.TopicAlias] = pb.Topic
		}
		if t, ok := r.aliases[*pb.Properties.TopicAlias]; ok {
			r.debug.Printf("aliased topic '%d' translates to '%s'", *pb.Properties.TopicAlias, m.Topic)
			topic = t
		}
	} else {
		topic = m.Topic
	}

	handlerCalled := false
	for route, handlers := range r.subscriptions {
		if match(route, topic) {
			r.debug.Println("found handler for:", route)
			for _, handler := range handlers {
				handler(m)
				handlerCalled = true
			}
		}
	}

	if !handlerCalled && r.defaultHandler != nil {
		r.defaultHandler(m)
	}
}

// SetDebugLogger sets the logger l to be used for printing debug
// information for the router
func (r *StandardRouter) SetDebugLogger(l log.Logger) {
	r.debug = l
}

// DefaultHandler sets handler to be called for messages that don't trigger another handler
// Pass nil to unset.
func (r *StandardRouter) DefaultHandler(h MessageHandler) {
	r.debug.Println("registering default handler")
	r.Lock()
	defer r.Unlock()
	r.defaultHandler = h
}

func match(route, topic string) bool {
	return route == topic || routeIncludesTopic(route, topic)
}

func matchDeep(route []string, topic []string) bool {
	if len(route) == 0 {
		return len(topic) == 0
	}

	if len(topic) == 0 {
		return route[0] == "#"
	}

	if route[0] == "#" {
		return true
	}

	if (route[0] == "+") || (route[0] == topic[0]) {
		return matchDeep(route[1:], topic[1:])
	}
	return false
}

func routeIncludesTopic(route, topic string) bool {
	return matchDeep(routeSplit(route), topicSplit(topic))
}

func routeSplit(route string) []string {
	if len(route) == 0 {
		return nil
	}
	var result []string
	if strings.HasPrefix(route, "$share") {
		result = strings.Split(route, "/")[2:]
	} else {
		result = strings.Split(route, "/")
	}
	return result
}

func topicSplit(topic string) []string {
	if len(topic) == 0 {
		return nil
	}
	return strings.Split(topic, "/")
}

// NewSingleHandlerRouter instantiates a router that will call the passed in message handler for all
// inbound messages (assuming `RegisterHandler` is never called).
//
// Deprecated: SingleHandlerRouter has been removed because it did not meet the requirements set out
// in the `Router` interface documentation. This function is only included to maintain compatibility,
// but there are limits (this version does not ignore calls to `RegisterHandler`).
func NewSingleHandlerRouter(h MessageHandler) *StandardRouter {
	return NewStandardRouterWithDefault(h)
}

// MessageContextHandler is a type for a function that is invoked
// by a ContextRouter when it has received a Publish.
// MessageContextHandlers should complete quickly (start a go routine for
// long-running processes) and should not call functions within the
// paho instance that triggered them (due to potential deadlocks).
// The context passed can be used for cancellation and timeout control.
type MessageContextHandler func(context.Context, *Publish)

// MessageContextMiddleware is a type for a function that wraps a MessageContextHandler
// to add additional functionality such as logging, metrics, or error handling.
type MessageContextMiddleware func(MessageContextHandler) MessageContextHandler

// ContextRouter is an interface of the functions for a struct that is
// used to handle invoking MessageContextHandlers depending on the
// the topic the message was published on.
type ContextRouter interface {
	// RegisterHandler() takes a string of the topic, and a MessageContextHandler
	RegisterHandler(string, MessageContextHandler)
	// UnregisterHandler() takes a string of the topic to remove
	UnregisterHandler(string)
	// Use() takes MessageContextMiddleware to wrap handlers with additional functionality
	Use(...MessageContextMiddleware)
	// Route() takes a Publish message and determines which MessageHandlers
	Route(*packets.Publish)
	// SetDebugLogger sets the logger l to be used for printing debug information for the router
	SetDebugLogger(log.Logger)
}

// StandardContextRouter is a library provided implementation of a ContextRouter that
// allows for unique and multiple MessageContextHandlers per topic with support for
// middleware to wrap handlers with additional functionality.
type StandardContextRouter struct {
	sync.RWMutex
	defaultHandler MessageContextHandler
	subscriptions  map[string][]MessageContextHandler
	middlewares    []MessageContextMiddleware
	aliases        map[uint16]string
	debug          log.Logger
}

// NewStandardContextRouter instantiates and returns an instance of a StandardContextRouter
func NewStandardContextRouter() *StandardContextRouter {
	return &StandardContextRouter{
		subscriptions: make(map[string][]MessageContextHandler),
		middlewares:   make([]MessageContextMiddleware, 0),
		aliases:       make(map[uint16]string),
		debug:         log.NOOPLogger{},
	}
}

// RegisterHandler is the library provided StandardContextRouter's
// implementation of the required interface function()
func (r *StandardContextRouter) RegisterHandler(topic string, h MessageContextHandler) {
	r.debug.Println("registering handler for:", topic)
	r.Lock()
	defer r.Unlock()

	r.subscriptions[topic] = append(r.subscriptions[topic], h)
}

// UnregisterHandler is the library provided StandardContextRouter's
// implementation of the required interface function()
func (r *StandardContextRouter) UnregisterHandler(topic string) {
	r.debug.Println("unregistering handler for:", topic)
	r.Lock()
	defer r.Unlock()

	delete(r.subscriptions, topic)
}

// Use registers middleware to wrap all handlers with additional functionality
func (r *StandardContextRouter) Use(m MessageContextMiddleware) {
	r.debug.Println("registering middleware")
	r.Lock()
	defer r.Unlock()

	r.middlewares = append(r.middlewares, m)
}

// Route is the library provided StandardContextRouter's implementation
// of the required interface function()
func (r *StandardContextRouter) Route(pb *packets.Publish) {
	r.debug.Println("routing message for:", pb.Topic)
	r.RLock()
	defer r.RUnlock()

	m := PublishFromPacketPublish(pb)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var topic string
	if pb.Properties.TopicAlias != nil {
		r.debug.Println("message is using topic aliasing")
		if pb.Topic != "" {
			// Register new alias
			r.debug.Printf("registering new topic alias '%d' for topic '%s'", *pb.Properties.TopicAlias, m.Topic)
			r.aliases[*pb.Properties.TopicAlias] = pb.Topic
		}
		if t, ok := r.aliases[*pb.Properties.TopicAlias]; ok {
			r.debug.Printf("aliased topic '%d' translates to '%s'", *pb.Properties.TopicAlias, m.Topic)
			topic = t
		}
	} else {
		topic = m.Topic
	}

	handlerCalled := false
	for route, handlers := range r.subscriptions {
		if match(route, topic) {
			r.debug.Println("found handler for:", route)
			for _, handler := range handlers {
				r.wrapHandler(handler)(ctx, m)
				handlerCalled = true
			}
		}
	}

	if !handlerCalled && r.defaultHandler != nil {
		r.wrapHandler(r.defaultHandler)(ctx, m)
	}
}

func (r *StandardContextRouter) wrapHandler(h MessageContextHandler) MessageContextHandler {
	if len(r.middlewares) == 0 {
		return h
	}
	for i := len(r.middlewares) - 1; i >= 0; i-- {
		h = r.middlewares[i](h)
	}
	return h
}

// SetDebugLogger sets the logger l to be used for printing debug
// information for the router
func (r *StandardContextRouter) SetDebugLogger(l log.Logger) {
	r.debug = l
}

// DefaultHandler sets handler to be called for messages that don't trigger another handler
// Pass nil to unset.
func (r *StandardContextRouter) DefaultHandler(h MessageContextHandler) {
	r.debug.Println("registering default handler")
	r.Lock()
	defer r.Unlock()
	r.defaultHandler = h
}
