package benchmarks

import (
	"runtime"
	"strings"
	"testing"

	"github.com/eclipse/paho.golang/autopaho/queue"
	"github.com/eclipse/paho.golang/autopaho/queue/lockfree"
	"github.com/eclipse/paho.golang/autopaho/queue/memory"
)

func benchmarkConcurrentEnqueueDequeue(b *testing.B, q queue.Queue) {
	data := strings.NewReader("test data")
	workers := runtime.GOMAXPROCS(0) // Use as many goroutines as there are available CPUs

	b.ResetTimer()
	b.ReportAllocs()
	b.SetParallelism(workers) // Set the number of goroutines to use in parallel benchmarks

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := q.Enqueue(data); err != nil {
				b.Fatal(err)
			}
			if err := q.Dequeue(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func seedQueue(q queue.Queue, count int) queue.Queue {
	for i := 0; i < count; i++ {
		data := strings.NewReader("test data")
		q.Enqueue(data)
	}
	return q
}

func benchmarkEnqueue(b *testing.B, q queue.Queue) {
	data := strings.NewReader("test data")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := q.Enqueue(data); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkPeek(b *testing.B, q queue.Queue) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := q.Peek(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkDequeue(b *testing.B, q queue.Queue) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		q.Dequeue()
	}
}

func BenchmarkEnqueueMemory(b *testing.B) {
	queue := memory.New()
	benchmarkEnqueue(b, queue)
}

func BenchmarkEnqueueLockFree(b *testing.B) {
	queue := lockfree.New()
	benchmarkEnqueue(b, queue)
}

func BenchmarkDequeueMemory(b *testing.B) {
	q := memory.New()
	seedQueue(q, 1000)
	benchmarkEnqueue(b, q)
}

func BenchmarkDequeueLockFree(b *testing.B) {
	q := lockfree.New()
	seedQueue(q, 1000)
	benchmarkDequeue(b, q)
}

func BenchmarkPeakMemory(b *testing.B) {
	q := memory.New()
	seedQueue(q, 1000)
	benchmarkPeek(b, q)
}

func BenchmarkPeakLockFree(b *testing.B) {
	q := lockfree.New()
	seedQueue(q, 1000)
	benchmarkPeek(b, q)
}

func BenchmarkConcurrentMemory(b *testing.B) {
	q := memory.New()
	benchmarkConcurrentEnqueueDequeue(b, q)
}

func BenchmarkConcurrentLockFree(b *testing.B) {
	q := lockfree.New()
	benchmarkConcurrentEnqueueDequeue(b, q)
}
