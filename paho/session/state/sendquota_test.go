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

package state

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestQuotaBasics performs basic testing of allocating/releasing message ids in inflight
func TestQuotaBasics(t *testing.T) {
	t.Parallel()

	it := newSendQuota(5)

	// We should be able to acquire until the quota is hit
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	for i := uint32(0); i < 5; i++ {
		if err := it.Acquire(ctx); err != nil {
			t.Fatalf("failed to get slot: %v", err)
		}
	}
	cancel()

	// subsequent waiters should block until Release is called
	for i := uint32(1); i < 5; i++ {
		release := make(chan struct{})
		released := make(chan error, 1)
		ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
		go func() {
			<-release
			it.Release()
			close(released)
		}()

		aResult := make(chan error, 1)
		go func() {
			aResult <- it.Acquire(ctx)
		}()
		select {
		case err := <-aResult:
			t.Fatalf("slot should not be gained before previous one released: %d, %v", i, err)
		case <-time.After(10 * time.Millisecond):
		}

		close(release)
		select {
		case <-released:
		case <-time.After(10 * time.Millisecond):
			t.Fatalf("slot should have been released")
		}
		cancel()
	}

	// Waiters should be released in order
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	aResult := make(chan struct {
		uint32
		error
	})
	for i := uint32(0); i < 5; i++ {
		go func(i uint32) {
			aResult <- struct {
				uint32
				error
			}{i, it.Acquire(ctx)}
		}(i)
		time.Sleep(time.Millisecond) // we want slots to be requested in known order
	}
	select {
	case result := <-aResult:
		t.Fatalf("await should not complete before slot is available: %v", result.error)
	default:
	}
	for i := uint32(0); i < 5; i++ {
		it.Release()
		time.Sleep(time.Millisecond) // provide time for release to be processed
	}
	time.Sleep(time.Millisecond) // allow time for slots to be allocated
	for i := uint32(0); i < 5; i++ {
		select {
		case result := <-aResult:
			if result.error != nil {
				t.Fatalf("unexpected error: %v", result.error)
			}
			if result.uint32 != i {
				t.Fatalf("release out of order; expected %d, got %d", i, result.uint32)
			}
		default:
			t.Fatalf("await should have compelted")
		}
	}

	// Check internal state
	if it.quota != 0 {
		t.Fatalf("expected 0 quota: %v", it.quota)
	}
	if len(it.waiters) != 0 {
		t.Fatalf("expected waiters to be empty: %v", it.waiters)
	}

	// Empty waiters
	for i := uint32(0); i < 5; i++ {
		it.Release()
	}
	// Check internal state
	if it.quota != 5 {
		t.Fatalf("expected 5 quota: %v", it.quota)
	}
	if len(it.waiters) != 0 {
		t.Fatalf("expected waiters to be empty: %v", it.waiters)
	}

	// Releasing additional slots should have no impact
	it.Release()
	if it.quota != 5 {
		t.Fatalf("expected 5 quota: %v", it.quota)
	}
}

// TestQuotaContext checks that we can cancel waiters
func TestQuotaContext(t *testing.T) {
	t.Parallel()

	it := newSendQuota(5)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Even with cancelled context Await should return a slot if it's free
	for i := uint32(0); i < 5; i++ {
		if err := it.Acquire(ctx); err != nil {
			t.Fatalf("failed to get slot: %v", err)
		}
	}
	// We should not wait if there is not a free slot
	if err := it.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("should fail if context cancelled: %v", err)
	}

	// Slots freed due to the context expiring should be removed
	// from `waiters` meaning that if we free ons slot we should
	// be able to acquire another
	if err := it.Release(); err != nil {
		t.Fatalf("failed to release slot: %v", err)
	}

	if err := it.Acquire(ctx); err != nil {
		t.Fatalf("failed to get slot after one was released: %v", err)
	}
}

// TestQuotaLoad tests send quota under load
func TestQuotaLoad(t *testing.T) {
	t.Parallel()

	const receiveMax = 20
	const messageCount = 20000
	var inFlight atomic.Int64

	it := newSendQuota(receiveMax)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	countErr := make(chan int64)
	awaitErr := make(chan error)

	var wg sync.WaitGroup
	wg.Add(messageCount)
	for i := uint32(0); i < messageCount; i++ {
		go func(messageID uint32) {
			defer wg.Done()
			if err := it.Acquire(ctx); err != nil {
				awaitErr <- err
			}
			curr := inFlight.Add(1)
			if curr > receiveMax {
				countErr <- curr
			}
			time.Sleep(time.Duration(rand.Intn(100000)))
			inFlight.Add(-1)
			it.Release()
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	for {
		select {
		case cnt := <-countErr:
			t.Fatalf("messages in flight exceeded receive max: %d", cnt)
		case err := <-awaitErr:
			t.Fatalf("error awaiting: %v", err)
		case <-done:
			return // all good
		}
	}

	// Check internal state
	if it.quota != receiveMax {
		t.Fatalf("expected quota of %d, got %d", receiveMax, it.quota)
	}
	if len(it.waiters) != 0 {
		t.Fatalf("expected waiters to be empty: %v", it.waiters)
	}
}

// TestQuotaRetransmitDoesNotUnderflow is a regression test: on reconnect the session
// retransmits each stored (unacknowledged) message via Retransmit, which decrements the
// quota without blocking. If more messages are stored than the receive maximum, the quota
// must go negative (over-allocated) rather than wrapping around. With the previous uint16
// quota it underflowed to a huge value, so Release returned ErrUnexpectedRelease and the
// receive-maximum bound was permanently lost.
func TestQuotaRetransmitDoesNotUnderflow(t *testing.T) {
	t.Parallel()

	const receiveMax = 2
	it := newSendQuota(receiveMax)

	// Retransmit one more message than the receive maximum allows.
	for i := 0; i < receiveMax+1; i++ {
		if err := it.Retransmit(); err != nil {
			t.Fatalf("Retransmit %d: %v", i, err)
		}
	}

	// Releasing the (over-allocated) slots must succeed and bring the quota back to its
	// initial value. If the quota had underflowed, Release would return ErrUnexpectedRelease.
	for i := 0; i < receiveMax+1; i++ {
		if err := it.Release(); err != nil {
			t.Fatalf("Release %d returned %v; quota underflowed instead of going negative", i, err)
		}
	}

	if it.quota != receiveMax {
		t.Fatalf("expected quota restored to %d, got %d", receiveMax, it.quota)
	}

	// Back at the initial value: a further Release is unexpected.
	if err := it.Release(); err != ErrUnexpectedRelease {
		t.Fatalf("expected ErrUnexpectedRelease at initial quota, got %v", err)
	}
}
