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
	"bufio"
	"bytes"
	"io"
	"net"
	"reflect"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeVBI127(t *testing.T) {
	b := encodeVBI(127)

	require.Len(t, b, 1)
	assert.Equal(t, byte(127), b[0])
}

func TestEncodeVBI128(t *testing.T) {
	b := encodeVBI(128)

	require.Len(t, b, 2)
	assert.Equal(t, byte(0x80), b[0])
	assert.Equal(t, byte(0x01), b[1])
}

func TestEncodeVBI16383(t *testing.T) {
	b := encodeVBI(16383)

	require.Len(t, b, 2)
	assert.Equal(t, byte(0xff), b[0])
	assert.Equal(t, byte(0x7f), b[1])
}

func TestEncodeVBI16384(t *testing.T) {
	b := encodeVBI(16384)

	require.Len(t, b, 3)
	assert.Equal(t, byte(0x80), b[0])
	assert.Equal(t, byte(0x80), b[1])
	assert.Equal(t, byte(0x01), b[2])
}

func TestEncodeVBI2097151(t *testing.T) {
	b := encodeVBI(2097151)

	require.Len(t, b, 3)
	assert.Equal(t, byte(0xff), b[0])
	assert.Equal(t, byte(0xff), b[1])
	assert.Equal(t, byte(0x7f), b[2])
}

func TestEncodeVBI2097152(t *testing.T) {
	b := encodeVBI(2097152)

	require.Len(t, b, 4)
	assert.Equal(t, byte(0x80), b[0])
	assert.Equal(t, byte(0x80), b[1])
	assert.Equal(t, byte(0x80), b[2])
	assert.Equal(t, byte(0x01), b[3])
}

func TestEncodeVBIMax(t *testing.T) {
	b := encodeVBI(268435455)

	require.Len(t, b, 4)
	assert.Equal(t, byte(0xff), b[0])
	assert.Equal(t, byte(0xff), b[1])
	assert.Equal(t, byte(0xff), b[2])
	assert.Equal(t, byte(0x7f), b[3])
}

func TestDecodeVBI12(t *testing.T) {
	x, err := decodeVBI(bytes.NewBuffer([]byte{0x0C}))

	require.Nil(t, err)
	assert.Equal(t, 12, x)
}

func TestDecodeVBI127(t *testing.T) {
	x, err := decodeVBI(bytes.NewBuffer([]byte{0xff}))

	require.Nil(t, err)
	assert.Equal(t, 127, x)
}
func TestDecodeVBI128(t *testing.T) {
	x, err := decodeVBI(bytes.NewBuffer([]byte{0x80, 0x01}))

	require.Nil(t, err)
	assert.Equal(t, 128, x)
}
func TestDecodeVBI16384(t *testing.T) {
	x, err := decodeVBI(bytes.NewBuffer([]byte{0x80, 0x80, 0x01}))

	require.Nil(t, err)
	assert.Equal(t, 16384, x)
}
func TestDecodeVBIMax(t *testing.T) {
	x, err := decodeVBI(bytes.NewBuffer([]byte{0xff, 0xff, 0xff, 0x7f}))

	require.Nil(t, err)
	assert.Equal(t, 268435455, x)
}

func TestNewControlPacketConnect(t *testing.T) {
	var b bytes.Buffer
	x := NewControlPacket(CONNECT)

	require.Equal(t, CONNECT, x.Type)

	x.Content.(*Connect).KeepAlive = 30
	x.Content.(*Connect).ClientID = "testClient"
	x.Content.(*Connect).UsernameFlag = true
	x.Content.(*Connect).Username = "testUser"
	sExpiryInterval := uint32(30)
	x.Content.(*Connect).Properties.SessionExpiryInterval = &sExpiryInterval

	_, err := x.WriteTo(&b)

	require.Nil(t, err)
	assert.Len(t, b.Bytes(), 40)
}

func TestReadPacketConnect(t *testing.T) {
	p := []byte{16, 38, 0, 4, 77, 81, 84, 84, 5, 128, 0, 30, 5, 17, 0, 0, 0, 30, 0, 10, 116, 101, 115, 116, 67, 108, 105, 101, 110, 116, 0, 8, 116, 101, 115, 116, 85, 115, 101, 114}

	c, err := ReadPacket(bufio.NewReader(bytes.NewReader(p)), 5)

	require.Nil(t, err)
	assert.Equal(t, uint16(30), c.Content.(*Connect).KeepAlive)
	assert.Equal(t, "testClient", c.Content.(*Connect).ClientID)
	assert.Equal(t, true, c.Content.(*Connect).UsernameFlag)
	assert.Equal(t, "testUser", c.Content.(*Connect).Username)
	assert.Equal(t, uint32(30), *c.Content.(*Connect).Properties.SessionExpiryInterval)
}

func TestReadStringWriteString(t *testing.T) {
	var b bytes.Buffer
	writeString("Test string", &b)

	s, err := readString(&b)
	require.Nil(t, err)
	assert.Equal(t, "Test string", s)
}

func TestNewControlPacket(t *testing.T) {
	tests := []struct {
		name string
		args byte
		want *ControlPacket
	}{
		{
			name: "connect",
			args: CONNECT,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: CONNECT},
				Content: &Connect{
					ProtocolName:    "MQTT",
					ProtocolVersion: 5,
					Properties:      &Properties{},
				},
			},
		},
		{
			name: "connack",
			args: CONNACK,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: CONNACK},
				Content:     &Connack{Properties: &Properties{}},
			},
		},
		{
			name: "publish",
			args: PUBLISH,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: PUBLISH},
				Content:     &Publish{Properties: &Properties{}},
			},
		},
		{
			name: "puback",
			args: PUBACK,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: PUBACK},
				Content:     &Puback{Properties: &Properties{}},
			},
		},
		{
			name: "pubrec",
			args: PUBREC,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: PUBREC},
				Content:     &Pubrec{Properties: &Properties{}},
			},
		},
		{
			name: "pubrel",
			args: PUBREL,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: PUBREL, Flags: 2},
				Content:     &Pubrel{Properties: &Properties{}},
			},
		},
		{
			name: "pubcomp",
			args: PUBCOMP,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: PUBCOMP},
				Content:     &Pubcomp{Properties: &Properties{}},
			},
		},
		{
			name: "subscribe",
			args: SUBSCRIBE,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: SUBSCRIBE, Flags: 2},
				Content:     &Subscribe{Properties: &Properties{}},
			},
		},
		{
			name: "suback",
			args: SUBACK,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: SUBACK},
				Content:     &Suback{Properties: &Properties{}},
			},
		},
		{
			name: "unsubscribe",
			args: UNSUBSCRIBE,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: UNSUBSCRIBE, Flags: 2},
				Content:     &Unsubscribe{Properties: &Properties{}},
			},
		},
		{
			name: "unsuback",
			args: UNSUBACK,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: UNSUBACK},
				Content:     &Unsuback{Properties: &Properties{}},
			},
		},
		{
			name: "pingreq",
			args: PINGREQ,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: PINGREQ},
				Content:     &Pingreq{},
			},
		},
		{
			name: "pingresp",
			args: PINGRESP,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: PINGRESP},
				Content:     &Pingresp{},
			},
		},
		{
			name: "disconnect",
			args: DISCONNECT,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: DISCONNECT},
				Content:     &Disconnect{Properties: &Properties{}},
			},
		},
		{
			name: "auth",
			args: AUTH,
			want: &ControlPacket{
				FixedHeader: FixedHeader{Type: AUTH, Flags: 1},
				Content:     &Auth{Properties: &Properties{}},
			},
		},
		{
			name: "dummy",
			args: 20,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewControlPacket(tt.args); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewControlPacket() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestControlPacket_PacketID(t *testing.T) {
	type fields struct {
		Content     Packet
		FixedHeader FixedHeader
	}
	tests := []struct {
		name   string
		fields fields
		want   uint16
	}{
		{
			name: "publish",
			fields: fields{
				FixedHeader: FixedHeader{Type: PUBLISH},
				Content:     &Publish{PacketID: 123},
			},
			want: 123,
		},
		{
			name: "puback",
			fields: fields{
				FixedHeader: FixedHeader{Type: PUBACK},
				Content:     &Puback{PacketID: 123},
			},
			want: 123,
		},
		{
			name: "pubrel",
			fields: fields{
				FixedHeader: FixedHeader{Type: PUBREL},
				Content:     &Pubrel{PacketID: 123},
			},
			want: 123,
		},
		{
			name: "pubrec",
			fields: fields{
				FixedHeader: FixedHeader{Type: PUBREC},
				Content:     &Pubrec{PacketID: 123},
			},
			want: 123,
		},
		{
			name: "pubcomp",
			fields: fields{
				FixedHeader: FixedHeader{Type: PUBCOMP},
				Content:     &Pubcomp{PacketID: 123},
			},
			want: 123,
		},
		{
			name: "subscribe",
			fields: fields{
				FixedHeader: FixedHeader{Type: SUBSCRIBE},
				Content:     &Subscribe{PacketID: 123},
			},
			want: 123,
		},
		{
			name: "suback",
			fields: fields{
				FixedHeader: FixedHeader{Type: SUBACK},
				Content:     &Suback{PacketID: 123},
			},
			want: 123,
		},
		{
			name: "unsubscribe",
			fields: fields{
				FixedHeader: FixedHeader{Type: UNSUBSCRIBE},
				Content:     &Unsubscribe{PacketID: 123},
			},
			want: 123,
		},
		{
			name: "unsuback",
			fields: fields{
				FixedHeader: FixedHeader{Type: UNSUBACK},
				Content:     &Unsuback{PacketID: 123},
			},
			want: 123,
		}, {
			name: "connect",
			fields: fields{
				FixedHeader: FixedHeader{Type: CONNECT},
				Content:     &Connect{},
			},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ControlPacket{
				Content:     tt.fields.Content,
				FixedHeader: tt.fields.FixedHeader,
			}
			if got := c.PacketID(); got != tt.want {
				t.Errorf("ControlPacket.PacketID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func BenchmarkConnect_Buffers(b *testing.B) {
	x := NewControlPacket(CONNECT)
	x.Content.(*Connect).KeepAlive = 30
	x.Content.(*Connect).ClientID = "testClient"
	x.Content.(*Connect).UsernameFlag = true
	x.Content.(*Connect).Username = "testUser"
	sExpiryInterval := uint32(30)
	x.Content.(*Connect).Properties.SessionExpiryInterval = &sExpiryInterval
	cp := x.Content.(*Connect)

	for n := 0; n < b.N; n++ {
		cp.Buffers()
	}
}

func BenchmarkPublish_Buffers(b *testing.B) {
	x := NewControlPacket(PUBLISH)
	x.Content.(*Publish).QoS = 0
	x.Content.(*Publish).Topic = "testTopic"
	x.Content.(*Publish).PacketID = uint16(100)
	x.Content.(*Publish).Payload = []byte("testPayload")
	pp := x.Content.(*Publish)

	for n := 0; n < b.N; n++ {
		pp.Buffers()
	}
}

func BenchmarkWriteTo(b *testing.B) {
	r, w := io.Pipe()
	done := make(chan int, 1)

	go func(r io.Reader, done chan int) {
		for {
			_, err := ReadPacket(r, 5)
			if err != nil {
				b.Error(err)
			}

			select {
			case <-done:
				return
			default:
			}
		}
	}(r, done)

	// Wrap Writer with Locker to ensure WriteTo will be thread-safe.
	type wrap struct {
		io.Writer
		sync.Locker
	}
	safeWriter := wrap{
		Writer: w,
		Locker: &sync.Mutex{},
	}

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			x := NewControlPacket(PUBLISH)
			x.Content.(*Publish).QoS = 0
			x.Content.(*Publish).Topic = "testTopic"
			x.Content.(*Publish).PacketID = uint16(100)
			x.Content.(*Publish).Payload = []byte("testPayload")
			pp := x.Content.(*Publish)
			for n := 0; n < b.N; n++ {
				pp.WriteTo(safeWriter)
			}
		}
	})

	done <- 1
}

func TestNewThreadSafeWrapper(t *testing.T) {
	var conn net.Conn
	ts := NewThreadSafeConn(conn)

	if _, ok := ts.(sync.Locker); !ok {
		t.Error("NewThreadSafeConn does not implement sync.Locker")
	}
}
