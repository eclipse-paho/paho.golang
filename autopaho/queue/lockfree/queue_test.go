package lockfree

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho/queue"
)

// TestLockFree some basic tests of the queue
func TestLockFree(t *testing.T) {
	q := New()

	if _, err := q.Peek(); !errors.Is(err, queue.ErrEmpty) {
		t.Errorf("expected ErrEmpty, got %s", err)
	}

	if err := q.Dequeue(); !errors.Is(err, queue.ErrEmpty) {
		t.Errorf("expected ErrEmpty, got %s", err)
	}

	queueNotEmpty := make(chan struct{})
	go func() {
		<-q.Wait()
		close(queueNotEmpty)
	}()
	time.Sleep(time.Nanosecond) // let go routine run
	select {
	case <-queueNotEmpty:
		t.Fatalf("Wait should not return until something is in queue")
	default:
	}
	testEntry := []byte("This is a test")
	if err := q.Enqueue(bytes.NewReader(testEntry)); err != nil {
		t.Fatalf("error adding to queue: %s", err)
	}
	select {
	case <-queueNotEmpty:
	case <-time.After(time.Second):
		t.Fatalf("Wait should return when something is in queue")
	}

	const entryFormat = "Queue entry %d for testing"
	for i := 0; i < 10; i++ {
		if err := q.Enqueue(bytes.NewReader([]byte(fmt.Sprintf(entryFormat, i)))); err != nil {
			t.Fatalf("error adding entry %d: %s", i, err)
		}
	}
	if err := q.Dequeue(); err != nil {
		t.Fatalf("error dequeue entry: %s", err)
	}

	for i := 0; i < 10; i++ {
		r, err := q.Peek()
		if err != nil {
			t.Fatalf("error peeking entry %d: %s", i, err)
		}
		buf := &bytes.Buffer{}
		if _, err = buf.ReadFrom(r); err != nil {
			t.Fatalf("error reading entry %d: %s", i, err)
		}
		if err = r.Close(); err != nil {
			t.Fatalf("error closing queue entry %d: %s", i, err)
		}

		expected := []byte(fmt.Sprintf(entryFormat, i))
		if bytes.Compare(expected, buf.Bytes()) != 0 {
			t.Fatalf("expected \"%s\", got \"%s\"", expected, buf.Bytes())
		}
		if err = q.Dequeue(); err != nil {
			t.Fatalf("error dequeue entry %d: %s", i, err)
		}
	}

	if _, err := q.Peek(); !errors.Is(err, queue.ErrEmpty) {
		t.Errorf("expected ErrEmpty, got %s", err)
	}

	if err := q.Dequeue(); !errors.Is(err, queue.ErrEmpty) {
		t.Errorf("expected ErrEmpty, got %s", err)
	}
}

// TestMultipleWait ensures that multiple goroutines waiting on the queue
// all receive the signal when a new item is enqueued.
func TestMultipleWait(t *testing.T) {
	q := New()
	var wg sync.WaitGroup
	waiters := 5

	// Start multiple goroutines that will wait for the signal
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			waitCh := q.Wait()
			<-waitCh // Wait for the signal
		}()
	}

	// Give the goroutines time to start and call Wait()
	time.Sleep(100 * time.Millisecond)

	// Enqueue an item, which should close the wait channel and signal all waiting goroutines
	err := q.Enqueue(strings.NewReader("data"))
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait() // Ensure all waiting goroutines have finished
	}()

	select {
	case <-done:
		// Test passed, all goroutines received the signal
	case <-time.After(30 * time.Second):
		t.Fatal("Test timed out: not all goroutines received the signal")
	}
}

// TestMultiplePeek ensures that multiple Peek calls return the correct value
// and do not remove the item from the queue.
func TestMultiplePeek(t *testing.T) {
	q := New()
	input := "data"

	// Enqueue an item
	err := q.Enqueue(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	// Start multiple Peek calls
	var wg sync.WaitGroup
	peekers := 5
	for i := 0; i < peekers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reader, err := q.Peek()
			if err != nil {
				t.Errorf("Peek failed: %v", err)
				return
			}
			data, err := ioutil.ReadAll(reader)
			if err != nil {
				t.Errorf("ReadAll failed: %v", err)
				return
			}
			if string(data) != input {
				t.Errorf("Peek returned incorrect data: got %v, want %v", string(data), input)
			}
		}()
	}

	wg.Wait() // Ensure all Peek calls have finished

	// The item should still be in the queue after multiple Peek calls
	_, err = q.Peek()
	if err != nil {
		t.Errorf("Item was removed from the queue after Peek: %v", err)
	}
}

// TestHighConcurrency tests the queue with a high number of concurrent Enqueue and Dequeue operations.
func TestHighConcurrency(t *testing.T) {
	q := New()
	var wg sync.WaitGroup
	workers := 100
	itemsPerWorker := 1000

	// Enqueue items
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < itemsPerWorker; j++ {
				data := strings.NewReader(fmt.Sprintf("%d-%d", workerID, j))
				err := q.Enqueue(data)
				if err != nil {
					t.Errorf("Enqueue failed: %v", err)
				}
			}
		}(i)
	}

	// Dequeue items
	var dequeueCount int32
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				err := q.Dequeue()
				if err == queue.ErrEmpty {
					if atomic.LoadInt32(&dequeueCount) == int32(workers*itemsPerWorker) {
						return
					}
					continue
				} else if err != nil {
					t.Errorf("Dequeue failed: %v", err)
					return
				}
				atomic.AddInt32(&dequeueCount, 1)
			}
		}()
	}

	wg.Wait() // Wait for all operations to complete

	// Use atomic read to get the final value of dequeueCount
	finalDequeueCount := atomic.LoadInt32(&dequeueCount)
	if finalDequeueCount != int32(workers*itemsPerWorker) {
		t.Errorf("Dequeue count mismatch: got %v, want %v", finalDequeueCount, workers*itemsPerWorker)
	}
}
