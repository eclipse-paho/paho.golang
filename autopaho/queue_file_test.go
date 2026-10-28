/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
 *
 * This program and the accompanying materials are made available under the
 * Eclipse Public License v2.0 (https://www.eclipse.org/legal/epl-2.0/) and
 * Eclipse Distribution License v1.0 (http://www.eclipse.org/org/documents/edl-v10.php).
 * AI Disclosure: This file was generated with OpenAI GPT-5 via Codex.
 * AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-5
 */

package autopaho

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho/queue"
	filequeue "github.com/eclipse/paho.golang/autopaho/queue/file"
	"github.com/eclipse/paho.golang/internal/testserver"
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	paholog "github.com/eclipse/paho.golang/paho/log"
)

// TestFileQueueQuarantinesTruncatedPacketsAfterRestart simulates partial files
// left by interrupted direct writes. AutoPaho must quarantine them without sending
// them, publish the valid entry behind them, and ignore the quarantined files on reopen.
func TestFileQueueQuarantinesTruncatedPacketsAfterRestart(t *testing.T) {
	dir := t.TempDir()
	pub := packets.Publish{Topic: "queue/recovery", Payload: bytes.Repeat([]byte("x"), 256)}
	var encoded bytes.Buffer
	if _, err := pub.WriteTo(&encoded); err != nil {
		t.Fatal(err)
	}
	// Simulate interrupted direct writes, including an empty file, incomplete
	// fixed header/remaining length, missing variable header, and short payload.
	cuts := []int{0, 1, 2, 3, encoded.Len() / 2, encoded.Len() - 1}
	var partialNames []string
	for i, cut := range cuts {
		name := filepath.Join(dir, fmt.Sprintf("q-s%020d.que", i+1))
		if err := os.WriteFile(name, encoded.Bytes()[:cut], 0600); err != nil {
			t.Fatal(err)
		}
		partialNames = append(partialNames, name)
	}
	q, err := filequeue.New(dir, "q-", ".que")
	if err != nil {
		t.Fatal(err)
	}
	// A valid entry behind the corrupt ones must still reach the server.
	if err := q.Enqueue(bytes.NewReader(encoded.Bytes())); err != nil {
		t.Fatal(err)
	}
	empty := q.WaitForEmpty()
	ts := testserver.New(paholog.NewTestLogger(t, "testServer:"))
	received := make(chan *packets.Publish, len(cuts)+1)
	ts.SetPacketReceivedCallback(func(cp *packets.ControlPacket) error {
		if p, ok := cp.Content.(*packets.Publish); ok {
			select {
			case received <- p:
			default:
				return errors.New("unexpected extra publication")
			}
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), longestDelay)
	defer cancel()
	conn, serverDone, err := ts.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		select {
		case <-serverDone:
		case <-time.After(longestDelay):
			t.Error("test server did not stop")
		}
	})
	available := make(chan net.Conn, 1)
	available <- conn
	server, _ := url.Parse(dummyURL)
	cm, err := NewConnection(ctx, ClientConfig{
		ServerUrls:     []*url.URL{server},
		KeepAlive:      60,
		ConnectTimeout: shortDelay,
		Queue:          q,
		Errors:         paholog.NewTestLogger(t, "queue:"),
		ClientConfig:   paho.ClientConfig{ClientID: "queue-recovery"},
		AttemptConnection: func(context.Context, ClientConfig, *url.URL) (net.Conn, error) {
			select {
			case conn := <-available:
				return conn, nil
			default:
				return nil, errors.New("unexpected reconnection")
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := func() {
		t.Helper()
		cancel()
		select {
		case <-cm.Done():
		case <-time.After(longestDelay):
			t.Fatal("connection manager did not stop")
		}
	}
	t.Cleanup(stop)
	select {
	case <-empty:
	case <-ctx.Done():
		t.Fatalf("queue did not advance past truncated entries: %v", ctx.Err())
	}
	select {
	case got := <-received:
		if got.Topic != pub.Topic || !bytes.Equal(got.Payload, pub.Payload) {
			t.Fatalf("unexpected publication: %v", got)
		}
	case <-ctx.Done():
		t.Fatalf("valid publication was not received: %v", ctx.Err())
	}
	stop() // Release the old queue before reopening the same namespace.
	select {
	case <-serverDone:
	case <-time.After(longestDelay):
		t.Fatal("test server did not stop")
	}
	if len(received) != 0 {
		t.Fatalf("sent %d unexpected extra publications", len(received))
	}
	q, err = filequeue.New(dir, "q-", ".que")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Peek(); !errors.Is(err, queue.ErrEmpty) {
		t.Fatalf("quarantined entries reappeared after restart: %v", err)
	}
	for i, name := range partialNames {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("truncated entry still has its queue filename: %v", err)
		}
		got, err := os.ReadFile(name + ".CORRUPT")
		if err != nil || !bytes.Equal(got, encoded.Bytes()[:cuts[i]]) {
			t.Fatalf("quarantined contents changed: %q, %v", got, err)
		}
	}
}
