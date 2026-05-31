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
	"reflect"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
)

func Test_RegisterAndRouteHandler(t *testing.T) {
	var handlerCalled bool
	var receivedTopic string

	handler := func(ctx context.Context, p *paho.Publish) {
		handlerCalled = true
		receivedTopic = p.Topic
	}

	r := NewRouter()
	r.RegisterHandler("test/topic", handler)
	r.Route(context.Background(), &packets.Publish{Topic: "test/topic", Properties: &packets.Properties{}})

	if !handlerCalled {
		t.Error("handler should have been called")
	}
	if receivedTopic != "test/topic" {
		t.Errorf("expected topic 'test/topic', got '%s'", receivedTopic)
	}
}

func Test_MultipleHandlersForSameTopic(t *testing.T) {
	var handler1Called, handler2Called bool

	handler1 := func(ctx context.Context, p *paho.Publish) {
		handler1Called = true
	}
	handler2 := func(ctx context.Context, p *paho.Publish) {
		handler2Called = true
	}

	r := NewRouter()
	r.RegisterHandler("test/topic", handler1)
	r.RegisterHandler("test/topic", handler2)
	r.Route(context.Background(), &packets.Publish{Topic: "test/topic", Properties: &packets.Properties{}})

	if !handler1Called || !handler2Called {
		t.Error("both handlers should have been called")
	}
}

func Test_UnregisterHandler(t *testing.T) {
	var handlerCalled bool

	handler := func(ctx context.Context, p *paho.Publish) {
		handlerCalled = true
	}

	r := NewRouter()
	r.RegisterHandler("test/topic", handler)
	r.UnregisterHandler("test/topic")
	r.Route(context.Background(), &packets.Publish{Topic: "test/topic", Properties: &packets.Properties{}})

	if handlerCalled {
		t.Error("handler should not have been called after unregistration")
	}
}

func Test_DefaultHandler(t *testing.T) {
	var defaultHandlerCalled, specificHandlerCalled bool

	specificHandler := func(ctx context.Context, p *paho.Publish) {
		specificHandlerCalled = true
	}
	defaultHandler := func(ctx context.Context, p *paho.Publish) {
		defaultHandlerCalled = true
	}

	r := NewRouter()
	r.RegisterHandler("specific/topic", specificHandler)
	r.DefaultHandler(defaultHandler)

	r.Route(context.Background(), &packets.Publish{Topic: "specific/topic", Properties: &packets.Properties{}})
	if !specificHandlerCalled {
		t.Error("specific handler should have been called")
	}

	specificHandlerCalled = false
	defaultHandlerCalled = false

	r.Route(context.Background(), &packets.Publish{Topic: "unmatched/topic", Properties: &packets.Properties{}})
	if !defaultHandlerCalled {
		t.Error("default handler should have been called for unmatched topic")
	}
	if specificHandlerCalled {
		t.Error("specific handler should not have been called for unmatched topic")
	}
}

func Test_DefaultHandlerUnset(t *testing.T) {
	var defaultHandlerCalled bool

	defaultHandler := func(ctx context.Context, p *paho.Publish) {
		defaultHandlerCalled = true
	}

	r := NewRouter()
	r.DefaultHandler(defaultHandler)
	r.DefaultHandler(nil)
	r.Route(context.Background(), &packets.Publish{Topic: "unmatched/topic", Properties: &packets.Properties{}})

	if defaultHandlerCalled {
		t.Error("default handler should not be called after being unset")
	}
}

func Test_WildcardSingleLevel(t *testing.T) {
	var h1Called, h2Called, h3Called bool

	r := NewRouter()
	r.RegisterHandler("sensors/+/status", func(ctx context.Context, p *paho.Publish) {
		h1Called = true
	})
	r.RegisterHandler("sensors/temp/+", func(ctx context.Context, p *paho.Publish) {
		h2Called = true
	})
	r.RegisterHandler("sensors/temp/status", func(ctx context.Context, p *paho.Publish) {
		h3Called = true
	})

	r.Route(context.Background(), &packets.Publish{Topic: "sensors/temp/status", Properties: &packets.Properties{}})
	if !h1Called {
		t.Error("single level wildcard handler should have been called")
	}

	h1Called = false
	h2Called = false
	h3Called = false

	r.Route(context.Background(), &packets.Publish{Topic: "sensors/temp/zone1", Properties: &packets.Properties{}})
	if !h2Called {
		t.Error("single level wildcard handler 2 should have been called")
	}
	if h1Called || h3Called {
		t.Error("other handlers should not have been called")
	}
}

func Test_WildcardMultiLevel(t *testing.T) {
	var h1Called, h2Called, h3Called bool

	r := NewRouter()
	r.RegisterHandler("sensors/#", func(ctx context.Context, p *paho.Publish) {
		h1Called = true
	})
	r.RegisterHandler("sensors/temp/#", func(ctx context.Context, p *paho.Publish) {
		h2Called = true
	})
	r.RegisterHandler("sensors/temp/zone1/status", func(ctx context.Context, p *paho.Publish) {
		h3Called = true
	})

	r.Route(context.Background(), &packets.Publish{Topic: "sensors/temp/zone1/status", Properties: &packets.Properties{}})
	if !h1Called || !h2Called || !h3Called {
		t.Error("all handlers should have been called for nested topic")
	}

	h1Called = false
	h2Called = false
	h3Called = false

	r.Route(context.Background(), &packets.Publish{Topic: "sensors/humidity", Properties: &packets.Properties{}})
	if !h1Called {
		t.Error("multi-level wildcard handler should have been called")
	}
	if h2Called || h3Called {
		t.Error("other handlers should not have been called")
	}
}

func Test_NoMatchingHandler(t *testing.T) {
	var handlerCalled bool

	handler := func(ctx context.Context, p *paho.Publish) {
		handlerCalled = true
	}

	r := NewRouter()
	r.RegisterHandler("test/topic", handler)
	r.Route(context.Background(), &packets.Publish{Topic: "other/topic", Properties: &packets.Properties{}})

	if handlerCalled {
		t.Error("handler should not have been called for non-matching topic")
	}
}

func Test_Middleware_SingleMiddleware(t *testing.T) {
	var executionOrder []string

	middleware1 := func(next Handler) Handler {
		return func(ctx context.Context, p *paho.Publish) {
			executionOrder = append(executionOrder, "m1-before")
			next(ctx, p)
			executionOrder = append(executionOrder, "m1-after")
		}
	}

	handler := func(ctx context.Context, p *paho.Publish) {
		executionOrder = append(executionOrder, "handler")
	}

	r := NewRouter()
	r.Use(middleware1)
	r.RegisterHandler("test/topic", handler)
	r.Route(context.Background(), &packets.Publish{Topic: "test/topic", Properties: &packets.Properties{}})

	expectedOrder := []string{"m1-before", "handler", "m1-after"}
	if !reflect.DeepEqual(executionOrder, expectedOrder) {
		t.Errorf("execution order incorrect, got: %v, want: %v", executionOrder, expectedOrder)
	}
}

func Test_Middleware_MultipleMiddleware(t *testing.T) {
	var executionOrder []string

	middleware1 := func(next Handler) Handler {
		return func(ctx context.Context, p *paho.Publish) {
			executionOrder = append(executionOrder, "m1-before")
			next(ctx, p)
			executionOrder = append(executionOrder, "m1-after")
		}
	}

	middleware2 := func(next Handler) Handler {
		return func(ctx context.Context, p *paho.Publish) {
			executionOrder = append(executionOrder, "m2-before")
			next(ctx, p)
			executionOrder = append(executionOrder, "m2-after")
		}
	}

	handler := func(ctx context.Context, p *paho.Publish) {
		executionOrder = append(executionOrder, "handler")
	}

	r := NewRouter()
	r.Use(middleware1)
	r.Use(middleware2)
	r.RegisterHandler("test/topic", handler)
	r.Route(context.Background(), &packets.Publish{Topic: "test/topic", Properties: &packets.Properties{}})

	expectedOrder := []string{"m1-before", "m2-before", "handler", "m2-after", "m1-after"}
	if !reflect.DeepEqual(executionOrder, expectedOrder) {
		t.Errorf("execution order incorrect, got: %v, want: %v", executionOrder, expectedOrder)
	}
}

func Test_Middleware_WithDefaultHandler(t *testing.T) {
	var executionOrder []string

	middleware := func(next Handler) Handler {
		return func(ctx context.Context, p *paho.Publish) {
			executionOrder = append(executionOrder, "middleware-before")
			next(ctx, p)
			executionOrder = append(executionOrder, "middleware-after")
		}
	}

	defaultHandler := func(ctx context.Context, p *paho.Publish) {
		executionOrder = append(executionOrder, "default-handler")
	}

	r := NewRouter()
	r.Use(middleware)
	r.DefaultHandler(defaultHandler)
	r.Route(context.Background(), &packets.Publish{Topic: "unmatched/topic", Properties: &packets.Properties{}})

	expectedOrder := []string{"middleware-before", "default-handler", "middleware-after"}
	if !reflect.DeepEqual(executionOrder, expectedOrder) {
		t.Errorf("execution order incorrect, got: %v, want: %v", executionOrder, expectedOrder)
	}
}

func Test_ContextPropagation(t *testing.T) {
	var receivedCtx context.Context

	handler := func(ctx context.Context, p *paho.Publish) {
		receivedCtx = ctx
	}

	r := NewRouter()
	r.RegisterHandler("test/topic", handler)
	r.Route(context.Background(), &packets.Publish{Topic: "test/topic", Properties: &packets.Properties{}})

	if receivedCtx == nil {
		t.Error("context should not be nil")
	}
	if receivedCtx != context.Background() {
		t.Error("context should be background context")
	}
}

func Test_TopicAlias(t *testing.T) {
	var receivedTopic string
	var callCount int

	handler := func(ctx context.Context, p *paho.Publish) {
		receivedTopic = p.Topic
		callCount++
	}

	r := NewRouter()
	r.RegisterHandler("real/topic", handler)

	alias := uint16(1)
	r.Route(context.Background(), &packets.Publish{
		Topic: "real/topic",
		Properties: &packets.Properties{
			TopicAlias: &alias,
		},
	})

	if callCount != 1 {
		t.Error("handler should have been called once")
	}
	if receivedTopic != "real/topic" {
		t.Errorf("expected topic 'real/topic', got '%s'", receivedTopic)
	}
}

func Test_match(t *testing.T) {
	tests := []struct {
		route    string
		topic    string
		expected bool
	}{
		{"test/topic", "test/topic", true},
		{"test", "test", true},
		{"test/+/status", "test/temp/status", true},
		{"test/+/status", "test/humidity/status", true},
		{"test/+/status", "test/temp/zone/status", false},
		{"+/topic", "test/topic", true},
		{"+/topic", "test/other/topic", false},
		{"test/#", "test/topic", true},
		{"test/#", "test/topic/nested", true},
		{"test/#", "other/topic", false},
		{"#", "any/topic", true},
		{"#", "any/nested/topic", true},
		{"test/topic", "other/topic", false},
		{"sensors/temp", "sensors/humidity", false},
	}

	for _, tc := range tests {
		t.Run(tc.route+":"+tc.topic, func(t *testing.T) {
			result := match(tc.route, tc.topic)
			if result != tc.expected {
				t.Errorf("match('%s', '%s') = %v, want %v", tc.route, tc.topic, result, tc.expected)
			}
		})
	}
}

func Test_routeSplit(t *testing.T) {
	tests := []struct {
		route    string
		expected []string
	}{
		{"test/topic", []string{"test", "topic"}},
		{"test/topic/nested", []string{"test", "topic", "nested"}},
		{"test", []string{"test"}},
		{"", nil},
		{"$share/group/test/topic", []string{"test", "topic"}},
		{"$share/group/test", []string{"test"}},
	}

	for _, tc := range tests {
		t.Run(tc.route, func(t *testing.T) {
			result := routeSplit(tc.route)
			if !reflect.DeepEqual(result, tc.expected) {
				t.Errorf("routeSplit('%s') = %v, want %v", tc.route, result, tc.expected)
			}
		})
	}
}

func Test_topicSplit(t *testing.T) {
	tests := []struct {
		topic    string
		expected []string
	}{
		{"test/topic", []string{"test", "topic"}},
		{"test/topic/nested", []string{"test", "topic", "nested"}},
		{"test", []string{"test"}},
		{"", nil},
	}

	for _, tc := range tests {
		t.Run(tc.topic, func(t *testing.T) {
			result := topicSplit(tc.topic)
			if !reflect.DeepEqual(result, tc.expected) {
				t.Errorf("topicSplit('%s') = %v, want %v", tc.topic, result, tc.expected)
			}
		})
	}
}

func Test_ConcurrentRouting(t *testing.T) {
	r := NewRouter()
	var callCount int
	done := make(chan bool)

	handler := func(ctx context.Context, p *paho.Publish) {
		callCount++
	}

	r.RegisterHandler("test/topic", handler)

	for i := 0; i < 10; i++ {
		go func() {
			r.Route(context.Background(), &packets.Publish{Topic: "test/topic", Properties: &packets.Properties{}})
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	if callCount != 10 {
		t.Errorf("expected handler to be called 10 times, got %d", callCount)
	}
}

func Test_ConcurrentRegistration(t *testing.T) {
	r := NewRouter()
	done := make(chan bool)
	var callCount int

	for i := 0; i < 5; i++ {
		go func(index int) {
			handler := func(ctx context.Context, p *paho.Publish) {
				callCount++
			}
			r.RegisterHandler("test/topic", handler)
			done <- true
		}(i)
	}

	for i := 0; i < 5; i++ {
		<-done
	}

	r.Route(context.Background(), &packets.Publish{Topic: "test/topic", Properties: &packets.Properties{}})

	if callCount != 5 {
		t.Errorf("expected handler to be called 5 times, got %d", callCount)
	}
}
