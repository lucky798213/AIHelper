package concurrency

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestLaneRunsFIFOWithDefaultConcurrency(t *testing.T) {
	queue := NewCommandQueue()
	var mu sync.Mutex
	var order []int

	futures := []Future{
		queue.Enqueue(context.Background(), "main", recordTask(&mu, &order, 1)),
		queue.Enqueue(context.Background(), "main", recordTask(&mu, &order, 2)),
		queue.Enqueue(context.Background(), "main", recordTask(&mu, &order, 3)),
	}
	for _, future := range futures {
		if _, err := future.Result(context.Background()); err != nil {
			t.Fatalf("future result: %v", err)
		}
	}

	if !reflect.DeepEqual(order, []int{1, 2, 3}) {
		t.Fatalf("order = %#v", order)
	}
}

func TestDifferentLanesRunConcurrently(t *testing.T) {
	queue := NewCommandQueue()
	release := make(chan struct{})
	startedA := make(chan struct{})
	startedB := make(chan struct{})

	futureA := queue.Enqueue(context.Background(), "main", func(ctx context.Context) (any, error) {
		close(startedA)
		<-release
		return "a", nil
	})
	futureB := queue.Enqueue(context.Background(), "heartbeat", func(ctx context.Context) (any, error) {
		close(startedB)
		<-release
		return "b", nil
	})

	waitClosed(t, startedA)
	waitClosed(t, startedB)
	close(release)
	mustResult(t, futureA)
	mustResult(t, futureB)
}

func TestSetConcurrencyAllowsParallelTasksInSameLane(t *testing.T) {
	queue := NewCommandQueue()
	queue.SetConcurrency("research", 2)
	release := make(chan struct{})
	startedA := make(chan struct{})
	startedB := make(chan struct{})

	futureA := queue.Enqueue(context.Background(), "research", func(ctx context.Context) (any, error) {
		close(startedA)
		<-release
		return "a", nil
	})
	futureB := queue.Enqueue(context.Background(), "research", func(ctx context.Context) (any, error) {
		close(startedB)
		<-release
		return "b", nil
	})

	waitClosed(t, startedA)
	waitClosed(t, startedB)
	close(release)
	mustResult(t, futureA)
	mustResult(t, futureB)
}

func TestFutureReturnsTaskResultErrorAndContextTimeout(t *testing.T) {
	queue := NewCommandQueue()
	wantErr := errors.New("boom")
	future := queue.Enqueue(context.Background(), "main", func(ctx context.Context) (any, error) {
		return "value", wantErr
	})
	got, err := future.Result(context.Background())
	if !errors.Is(err, wantErr) || got != "value" {
		t.Fatalf("result=%#v err=%v", got, err)
	}

	release := make(chan struct{})
	blocked := queue.Enqueue(context.Background(), "main", func(ctx context.Context) (any, error) {
		<-release
		return nil, nil
	})
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := blocked.Result(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err = %v", err)
	}
	close(release)
	mustResult(t, blocked)
}

func TestResetPreventsStaleTaskFromPumpingQueuedWork(t *testing.T) {
	queue := NewCommandQueue()
	release := make(chan struct{})
	startedFirst := make(chan struct{})
	startedSecond := make(chan struct{})

	first := queue.Enqueue(context.Background(), "main", func(ctx context.Context) (any, error) {
		close(startedFirst)
		<-release
		return nil, nil
	})
	second := queue.Enqueue(context.Background(), "main", func(ctx context.Context) (any, error) {
		close(startedSecond)
		return nil, nil
	})

	waitClosed(t, startedFirst)
	generations := queue.ResetAll()
	if generations["main"] != 1 {
		t.Fatalf("generation = %#v", generations)
	}
	close(release)
	mustResult(t, first)
	if _, err := second.Result(context.Background()); !errors.Is(err, ErrQueueReset) {
		t.Fatalf("second future err = %v, want ErrQueueReset", err)
	}

	select {
	case <-startedSecond:
		t.Fatal("stale queued task should not be pumped after reset")
	case <-time.After(30 * time.Millisecond):
	}
	stats := queue.Stats()
	if len(stats) != 1 || stats[0].Generation != 1 || stats[0].QueueDepth != 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func recordTask(mu *sync.Mutex, order *[]int, value int) Task {
	return func(ctx context.Context) (any, error) {
		mu.Lock()
		*order = append(*order, value)
		mu.Unlock()
		return value, nil
	}
}

func mustResult(t *testing.T, future Future) any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := future.Result(ctx)
	if err != nil {
		t.Fatalf("future result: %v", err)
	}
	return result
}

func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for channel")
	}
}
