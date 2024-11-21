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

package packets

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSubscribePackUnpack confirms that subscribe packets are packed/unpacked as per the spec
func TestSubscribePackUnpack(t *testing.T) {
	tests := []struct {
		name string
		sub  *Subscribe
		want []byte
	}{
		{
			name: "Spec example (Figure 3-20)",
			sub: &Subscribe{
				Properties: &Properties{},
				PacketID:   10,
				Subscriptions: []SubOptions{
					{
						Topic:             "a/b",
						QoS:               1,
						RetainHandling:    0,
						NoLocal:           false,
						RetainAsPublished: false,
					},
					{
						Topic:             "c/d",
						QoS:               2,
						RetainHandling:    0,
						NoLocal:           false,
						RetainAsPublished: false,
					}},
			},
			want: []byte{0b10000010, 0x0F, 0x0, 0xa, 0x0, 0x0, 0x3, 0x61, 0x2f, 0x62, 0x01, 0x0, 0x3, 0x63, 0x2f, 0x64, 0x2},
		},
		{
			name: "NoLocal",
			sub: &Subscribe{
				Properties: &Properties{},
				PacketID:   10,
				Subscriptions: []SubOptions{
					{
						Topic:             "NoLocal",
						QoS:               0,
						RetainHandling:    0,
						NoLocal:           true,
						RetainAsPublished: false,
					},
				},
			},
			want: []byte{0x82, 0xd, 0x0, 0xa, 0x0, 0x0, 0x7, 0x4e, 0x6f, 0x4c, 0x6f, 0x63, 0x61, 0x6c, 0b00000100},
		},
		{
			name: "Retain As Published",
			sub: &Subscribe{
				Properties: &Properties{},
				PacketID:   10,
				Subscriptions: []SubOptions{
					{
						Topic:             "RAP",
						QoS:               0,
						RetainHandling:    0,
						NoLocal:           false,
						RetainAsPublished: true,
					},
				},
			},
			want: []byte{0x82, 0x9, 0x0, 0xa, 0x0, 0x0, 0x3, 0x52, 0x41, 0x50, 0b00001000},
		},
		{
			name: "RetainHandling",
			sub: &Subscribe{
				Properties: &Properties{},
				PacketID:   10,
				Subscriptions: []SubOptions{
					{
						Topic:             "Retain0",
						QoS:               0,
						RetainHandling:    0,
						NoLocal:           false,
						RetainAsPublished: false,
					},
					{
						Topic:             "Retain1",
						QoS:               0,
						RetainHandling:    1,
						NoLocal:           false,
						RetainAsPublished: false,
					},
					{
						Topic:             "Retain2",
						QoS:               0,
						RetainHandling:    2,
						NoLocal:           false,
						RetainAsPublished: false,
					},
				},
			},
			want: []byte{0x82, 0x21, 0x0, 0xa, 0x0,
				0x0, 0x7, 0x52, 0x65, 0x74, 0x61, 0x69, 0x6e, 0x30, 0b00000000,
				0x0, 0x7, 0x52, 0x65, 0x74, 0x61, 0x69, 0x6e, 0x31, 0b00010000,
				0x0, 0x7, 0x52, 0x65, 0x74, 0x61, 0x69, 0x6e, 0x32, 0b00100000},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			_, err := tt.sub.WriteTo(&b)
			require.NoError(t, err)

			require.Equal(t, tt.want, b.Bytes())

			// Decode and compare with original
			dstCp, err := ReadPacket(bytes.NewReader(b.Bytes()), 5)
			require.NoError(t, err)

			dstP, ok := dstCp.Content.(*Subscribe)
			if !ok {
				t.Fatalf("readPacket did not return expected type (got %T)", dstCp.Content)
			}
			require.Equal(t, tt.sub, dstP)
		})
	}
}
