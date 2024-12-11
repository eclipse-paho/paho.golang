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
	"reflect"
	"testing"

	"github.com/eclipse/paho.golang/packets"
)

func Test_match(t *testing.T) {
	tests := []struct {
		name  string
		route string
		topic string
		want  bool
	}{
		{"basic1", "a/b", "a/b", true},
		{"basic2", "a", "a/b", false},
		{"plus1", "a/+", "a/b", true},
		{"plus2", "+/b", "a/b", true},
		{"plus3", "a/+/c", "a/b/c", true},
		{"plus4", "a/+/c", "a/asdf/c", true},
		{"hash1", "#", "a/b", true},
		{"hash2", "a/#", "a/b", true},
		{"hash3", "b/#", "a/b", false},
		{"hash4", "#", "", true},
		{"share1", "$share/group1/a/b", "a/b", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := match(tt.route, tt.topic); got != tt.want {
				t.Errorf("match() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_routeIncludesTopic(t *testing.T) {
	type args struct {
		route string
		topic string
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeIncludesTopic(tt.args.route, tt.args.topic); got != tt.want {
				t.Errorf("routeIncludesTopic() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_routeSplit(t *testing.T) {
	type args struct {
		route string
	}
	tests := []struct {
		name string
		args args
		want []string
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeSplit(tt.args.route); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("routeSplit() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_routeDefault(t *testing.T) {
	var r1Count, r2Count int

	ctx := context.Background()
	r1 := func(c context.Context, p *Publish) { r1Count++ }
	r2 := func(c context.Context, p *Publish) { r2Count++ }

	r := NewStandardRouter()
	r.RegisterHandler("test", r1)

	r.Route(ctx, &packets.Publish{Topic: "test", Properties: &packets.Properties{}})
	if r1Count != 1 {
		t.Errorf("router1 should have been called r1: %d, r2: %d", r1Count, r2Count)
	}
	// Confirm that unset default does not cause issue
	r.Route(ctx, &packets.Publish{Topic: "xxyy", Properties: &packets.Properties{}})
	if r1Count != 1 {
		t.Errorf("router1 should not have been called r1: %d, r2: %d", r1Count, r2Count)
	}

	r.DefaultHandler(r2)
	r.Route(ctx, &packets.Publish{Topic: "test", Properties: &packets.Properties{}})
	if r1Count != 2 || r2Count != 0 {
		t.Errorf("router1 should been called r1: %d, r2: %d", r1Count, r2Count)
	}
	r.Route(ctx, &packets.Publish{Topic: "xxyy", Properties: &packets.Properties{}})
	if r1Count != 2 || r2Count != 1 {
		t.Errorf("router2 should have been called r1: %d, r2: %d", r1Count, r2Count)
	}

	r.DefaultHandler(nil)
	r.Route(ctx, &packets.Publish{Topic: "xxyy", Properties: &packets.Properties{}})
	if r1Count != 2 || r2Count != 1 {
		t.Errorf("no router should have been called r1: %d, r2: %d", r1Count, r2Count)
	}

}

func Test_routeContextPropagation(t *testing.T) {
	type ctxKey string
	testKey := ctxKey("test-key")
	testValue := "test-value"

	var receivedValue string
	handler := func(ctx context.Context, p *Publish) {
		if v, ok := ctx.Value(testKey).(string); ok {
			receivedValue = v
		}
	}

	r := NewStandardRouter()
	r.RegisterHandler("test/topic", handler)

	// Create a context with a test value
	ctx := context.WithValue(context.Background(), testKey, testValue)
	
	// Route a message with the context
	r.Route(ctx, &packets.Publish{
		Topic: "test/topic",
		Properties: &packets.Properties{},
	})

	// Verify the context value was correctly propagated
	if receivedValue != testValue {
		t.Errorf("context value not propagated correctly, got: %v, want: %v", receivedValue, testValue)
	}

	// Test with a cancelled context
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	
	var contextCancelled bool
	cancelHandler := func(ctx context.Context, p *Publish) {
		if ctx.Err() == context.Canceled {
			contextCancelled = true
		}
	}

	r.RegisterHandler("test/cancel", cancelHandler)
	r.Route(cancelCtx, &packets.Publish{
		Topic: "test/cancel",
		Properties: &packets.Properties{},
	})

	if !contextCancelled {
		t.Error("cancelled context was not properly propagated to handler")
	}
}
