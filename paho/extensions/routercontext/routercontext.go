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
package routercontext

import (
	"context"
	"strings"
	"sync"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/log"
)

// Handler is a type for a function that is invoked by a Router when it has received a Publish.
// Handler receives context.Context along with the message for cancellation and timeout control.
// Handlers should complete quickly (start a go routine for long-running processes) and
// should not call functions within the paho instance that triggered them (due to potential deadlocks).
type Handler func(context.Context, *paho.Publish)

// Middleware is a type for a function that wraps a Handler to add additional functionality
// such as logging, metrics, or error handling.
type Middleware func(Handler) Handler

// Router is a context-aware router implementation that allows for unique and multiple
// Handlers per topic with support for middleware to wrap handlers with additional functionality.
// It matches topics using MQTT topic subscription patterns including + (single level) and # (multi-level) wildcards.
type Router struct {
	sync.RWMutex
	defaultHandler Handler
	subscriptions  map[string][]Handler
	middlewares    []Middleware
	aliases        map[uint16]string
	debug          log.Logger
}

// NewRouter instantiates and returns an instance of a context-aware Router
func NewRouter() *Router {
	return &Router{
		subscriptions: make(map[string][]Handler),
		middlewares:   make([]Middleware, 0),
		aliases:       make(map[uint16]string),
		debug:         log.NOOPLogger{},
	}
}

// RegisterHandler registers a Handler for the given topic pattern.
// Multiple handlers can be registered for the same topic, and all matching handlers will be called.
func (r *Router) RegisterHandler(topic string, h Handler) {
	r.debug.Println("registering handler for:", topic)
	r.Lock()
	defer r.Unlock()

	r.subscriptions[topic] = append(r.subscriptions[topic], h)
}

// UnregisterHandler removes all handlers registered for the given topic pattern.
func (r *Router) UnregisterHandler(topic string) {
	r.debug.Println("unregistering handler for:", topic)
	r.Lock()
	defer r.Unlock()

	delete(r.subscriptions, topic)
}

// Use registers one or more middleware to wrap all handlers with additional functionality.
// Middleware are applied in the order they are registered, wrapping from outside to inside.
func (r *Router) Use(m ...Middleware) {
	r.debug.Println("registering middleware")
	r.Lock()
	defer r.Unlock()

	r.middlewares = append(r.middlewares, m...)
}

// Route routes a Publish message to all matching handlers based on topic subscription patterns.
// Handlers are called with a background context. Topic aliases are resolved and cached.
func (r *Router) Route(pb *packets.Publish) {
	r.debug.Println("routing message for:", pb.Topic)
	r.RLock()
	defer r.RUnlock()

	m := paho.PublishFromPacketPublish(pb)

	ctx := context.Background()

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

// SetDebugLogger sets the logger to be used for printing debug information for the router.
func (r *Router) SetDebugLogger(l log.Logger) {
	r.debug = l
}

// DefaultHandler sets a handler to be called for messages that don't match any registered topic pattern.
// Pass nil to unset the default handler.
func (r *Router) DefaultHandler(h Handler) {
	r.debug.Println("registering default handler")
	r.Lock()
	defer r.Unlock()
	r.defaultHandler = h
}

// wrapHandler wraps a Handler with all registered middleware in reverse order.
// This ensures middleware is applied in the order they were registered.
func (r *Router) wrapHandler(h Handler) Handler {
	if len(r.middlewares) == 0 {
		return h
	}
	for i := len(r.middlewares) - 1; i >= 0; i-- {
		h = r.middlewares[i](h)
	}
	return h
}

// match determines if a route pattern matches a topic string.
// Supports MQTT wildcards: + (single level) and # (multi-level).
func match(route, topic string) bool {
	return route == topic || routeIncludesTopic(route, topic)
}

// matchDeep recursively matches route segments against topic segments.
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

// routeIncludesTopic checks if a route pattern includes a specific topic by splitting and matching segments.
func routeIncludesTopic(route, topic string) bool {
	return matchDeep(routeSplit(route), topicSplit(topic))
}

// routeSplit splits a route pattern string by '/' and handles shared subscriptions ($share prefix).
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

// topicSplit splits a topic string by '/' into segments for pattern matching.
func topicSplit(topic string) []string {
	if len(topic) == 0 {
		return nil
	}
	return strings.Split(topic, "/")
}
