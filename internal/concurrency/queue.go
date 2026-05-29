package concurrency

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrQueueReset = errors.New("queue reset")

const (
	LaneMain      = "main"
	LaneHeartbeat = "heartbeat"
	LaneCron      = "cron"
)

type Task func(ctx context.Context) (any, error)

type Future interface {
	Result(ctx context.Context) (any, error)
}

type LaneStats struct {
	Name           string
	QueueDepth     int
	Active         int
	MaxConcurrency int
	Generation     int
}

type CommandQueue struct {
	mu    sync.Mutex
	lanes map[string]*LaneQueue
}

func NewCommandQueue() *CommandQueue {
	return &CommandQueue{lanes: make(map[string]*LaneQueue)}
}

func (q *CommandQueue) GetOrCreateLane(name string, maxConcurrency int) *LaneQueue {
	if q == nil {
		return nil
	}
	name = normalizeLaneName(name)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.lanes == nil {
		q.lanes = make(map[string]*LaneQueue)
	}
	lane, ok := q.lanes[name]
	if !ok {
		lane = newLaneQueue(name, maxConcurrency)
		q.lanes[name] = lane
	}
	return lane
}

func (q *CommandQueue) Enqueue(ctx context.Context, laneName string, task Task) Future {
	lane := q.GetOrCreateLane(laneName, 1)
	if lane == nil {
		f := newFuture()
		f.complete(nil, errors.New("command queue is nil"))
		return f
	}
	return lane.Enqueue(ctx, task)
}

func (q *CommandQueue) SetConcurrency(laneName string, maxConcurrency int) {
	lane := q.GetOrCreateLane(laneName, maxConcurrency)
	if lane != nil {
		lane.SetConcurrency(maxConcurrency)
	}
}

func (q *CommandQueue) ResetAll() map[string]int {
	result := make(map[string]int)
	if q == nil {
		return result
	}
	q.mu.Lock()
	lanes := make([]*LaneQueue, 0, len(q.lanes))
	for _, lane := range q.lanes {
		lanes = append(lanes, lane)
	}
	q.mu.Unlock()

	for _, lane := range lanes {
		name, generation := lane.Reset()
		result[name] = generation
	}
	return result
}

func (q *CommandQueue) WaitForAll(timeout time.Duration) bool {
	if q == nil {
		return true
	}
	deadline := time.Now().Add(timeout)
	q.mu.Lock()
	lanes := make([]*LaneQueue, 0, len(q.lanes))
	for _, lane := range q.lanes {
		lanes = append(lanes, lane)
	}
	q.mu.Unlock()

	for _, lane := range lanes {
		remaining := time.Until(deadline)
		if timeout <= 0 {
			remaining = 0
		}
		if timeout > 0 && remaining <= 0 {
			return false
		}
		if !lane.WaitForIdle(remaining) {
			return false
		}
	}
	return true
}

func (q *CommandQueue) Stats() []LaneStats {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	lanes := make([]*LaneQueue, 0, len(q.lanes))
	for _, lane := range q.lanes {
		lanes = append(lanes, lane)
	}
	q.mu.Unlock()

	stats := make([]LaneStats, 0, len(lanes))
	for _, lane := range lanes {
		stats = append(stats, lane.Stats())
	}
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Name < stats[j].Name
	})
	return stats
}

type LaneQueue struct {
	name           string
	maxConcurrency int
	queue          []queuedTask
	active         int
	generation     int
	mu             sync.Mutex
	cond           *sync.Cond
}

type queuedTask struct {
	ctx        context.Context
	task       Task
	future     *future
	generation int
}

func newLaneQueue(name string, maxConcurrency int) *LaneQueue {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	lane := &LaneQueue{
		name:           normalizeLaneName(name),
		maxConcurrency: maxConcurrency,
	}
	lane.cond = sync.NewCond(&lane.mu)
	return lane
}

func (l *LaneQueue) Enqueue(ctx context.Context, task Task) Future {
	//先创建 future，每次入队都会创建一个 future。这个 future 和这次任务绑定。
	f := newFuture()

	if l == nil {
		f.complete(nil, errors.New("lane is nil"))
		return f
	}
	if task == nil {
		f.complete(nil, errors.New("task is nil"))
		return f
	}
	if ctx == nil {
		ctx = context.Background()
	}

	//加锁，修改 lane 内部队列
	l.mu.Lock()
	l.queue = append(l.queue, queuedTask{
		ctx:        ctx,
		task:       task,
		future:     f,
		generation: l.generation,
	})

	// 尝试启动任务，
	l.pumpLocked()
	l.mu.Unlock()
	return f
}

func (l *LaneQueue) SetConcurrency(maxConcurrency int) {
	if l == nil {
		return
	}
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	l.mu.Lock()
	l.maxConcurrency = maxConcurrency
	l.pumpLocked()
	l.cond.Broadcast()
	l.mu.Unlock()
}

func (l *LaneQueue) Reset() (string, int) {
	if l == nil {
		return "", 0
	}
	l.mu.Lock()
	l.generation++
	queued := append([]queuedTask(nil), l.queue...)
	for i := range l.queue {
		l.queue[i] = queuedTask{}
	}
	l.queue = nil
	name := l.name
	generation := l.generation
	l.cond.Broadcast()
	l.mu.Unlock()
	for _, item := range queued {
		item.future.complete(nil, ErrQueueReset)
	}
	return name, generation
}

func (l *LaneQueue) Stats() LaneStats {
	if l == nil {
		return LaneStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return LaneStats{
		Name:           l.name,
		QueueDepth:     len(l.queue),
		Active:         l.active,
		MaxConcurrency: l.maxConcurrency,
		Generation:     l.generation,
	}
}

func (l *LaneQueue) WaitForIdle(timeout time.Duration) bool {
	if l == nil {
		return true
	}
	deadline := time.Now().Add(timeout)
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.active > 0 || len(l.queue) > 0 {
		if timeout > 0 {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return false
			}
			timer := time.AfterFunc(remaining, func() {
				l.mu.Lock()
				l.cond.Broadcast()
				l.mu.Unlock()
			})
			l.cond.Wait()
			timer.Stop()
			continue
		}
		l.cond.Wait()
	}
	return true
}

// pump = 把队列里的任务 “抽出来、送出去、执行掉” 的循环动作
func (l *LaneQueue) pumpLocked() {
	//表示当前正在跑的任务数还没满。且还有任务，l.queue这个 queue 是[]queuedTask
	for l.active < l.maxConcurrency && len(l.queue) > 0 {
		//然后取队首
		item := l.queue[0]
		//然后把队列往前挪，移除第一个任务：
		copy(l.queue, l.queue[1:])
		l.queue[len(l.queue)-1] = queuedTask{}
		l.queue = l.queue[:len(l.queue)-1]
		//然后标记正在运行的任务数加一：
		l.active++
		//最后启动 goroutine：
		go l.runTask(item)
	}
}

func (l *LaneQueue) runTask(item queuedTask) {
	//执行完任务，得到结果
	result, err := runTask(item.ctx, item.task)

	//会把结果保存起来：
	item.future.complete(result, err)

	//标记这个任务已完成
	l.taskDone(item.generation)
}

func (l *LaneQueue) taskDone(generation int) {
	l.mu.Lock()
	if l.active > 0 {
		l.active--
	}
	if generation == l.generation {
		l.pumpLocked()
	}

	// 是通知所有等待者；如果只通知一个，可以用 Signal。这里用 Broadcast 更稳。
	l.cond.Broadcast()
	l.mu.Unlock()
}

// 安全地执行一个任务函数
func runTask(ctx context.Context, task Task) (result any, err error) {
	defer func() {
		// recover() 是 Go 里专门用来捕获 panic 的函数。
		if recovered := recover(); recovered != nil {
			err = errors.New("task panicked")
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return task(ctx)
	}
}

func normalizeLaneName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "default"
	}
	return name
}

type future struct {
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	result any
	err    error
}

func newFuture() *future {
	return &future{done: make(chan struct{})}
}

func (f *future) Result(ctx context.Context) (any, error) {
	if f == nil {
		return nil, errors.New("future is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-f.done:
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.result, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *future) complete(result any, err error) {
	f.once.Do(func() {
		f.mu.Lock()
		f.result = result
		f.err = err
		f.mu.Unlock()
		close(f.done)
	})
}
