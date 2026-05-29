package delivery

import (
	"context"
	"errors"
	"sync"
	"time"

	"AIHelper/internal/channels"
)

const DefaultScanInterval = time.Second

type Outbox interface {
	Enqueue(ctx context.Context, item Item) (string, error)
	Ack(ctx context.Context, id string) error
	Fail(ctx context.Context, id string, cause error) error
	Pending(ctx context.Context) ([]Item, error)
	Failed(ctx context.Context) ([]Item, error)
	RetryFailed(ctx context.Context) (int, error)
}

type Sender func(ctx context.Context, msg channels.OutboundMessage) error

type ServiceConfig struct {
	Outbox       Outbox
	Sender       Sender
	ScanInterval time.Duration
	Now          func() time.Time
}

type Service struct {
	outbox       Outbox
	sender       Sender
	scanInterval time.Duration
	now          func() time.Time

	mu             sync.Mutex
	totalAttempted int
	totalSucceeded int
	totalFailed    int
	cancel         context.CancelFunc
	done           chan struct{}
	started        bool
	wake           chan struct{}
}

type Stats struct {
	Pending        int
	Failed         int
	TotalAttempted int
	TotalSucceeded int
	TotalFailed    int
}

func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Outbox == nil {
		return nil, errors.New("delivery outbox is required")
	}
	if cfg.Sender == nil {
		return nil, errors.New("delivery sender is required")
	}
	if cfg.ScanInterval <= 0 {
		cfg.ScanInterval = DefaultScanInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{
		outbox:       cfg.Outbox,
		sender:       cfg.Sender,
		scanInterval: cfg.ScanInterval,
		now:          cfg.Now,
		wake:         make(chan struct{}, 1),
	}, nil
}

func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.cancel = cancel
	s.done = done
	s.started = true
	interval := s.scanInterval
	s.mu.Unlock()

	go func() {
		defer close(done)
		s.cleanup(runCtx)
		_ = s.ProcessPending(runCtx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_ = s.ProcessPending(runCtx)
			case <-s.wake:
				_ = s.ProcessPending(runCtx)
			}
		}
	}()
}

func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.cancel = nil
	s.done = nil
	s.started = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *Service) Enqueue(ctx context.Context, msg channels.OutboundMessage) error {
	if s == nil {
		return errors.New("delivery service is nil")
	}

	//将 msg 的文本进行切割（过长的情况）
	for _, chunk := range ChunkMessage(msg.Text, msg.Channel) {
		if _, err := s.outbox.Enqueue(ctx, Item{
			Channel: msg.Channel,
			To:      msg.To,
			ToType:  msg.ToType,
			Text:    chunk,
		}); err != nil {
			return err
		}
	}
	s.notify()
	return nil
}

// Process：处理、执行
// Pending：等待处理的
func (s *Service) ProcessPending(ctx context.Context) error {
	//防御式判断。Service 是 nil 的时候直接不做事，避免 panic。
	if s == nil {
		return nil
	}

	//从 outbox 里读出所有待发送消息。对于文件型 outbox 来说，就是扫描 pending 目录里的 JSON 文件。
	pending, err := s.outbox.Pending(ctx)
	if err != nil {
		return err
	}

	//记录当前时间，然后逐条处理 pending item。
	now := s.now()
	for _, item := range pending {

		//每处理一条之前检查上下文是否取消。比如程序退出了，就停止继续投递。
		if err := ctx.Err(); err != nil {
			return err
		}

		//如果这条消息之前发送失败过，Fail 会给它设置 NextRetryAt。
		//如果现在还没到下一次重试时间，就跳过，等下一轮扫描再处理。
		if !item.NextRetryAt.IsZero() && item.NextRetryAt.After(now) {
			continue
		}

		//统计一次发送尝试。这个是 delivery 内部统计数据
		s.addAttempt()

		//真正发送消息。
		err := s.sender(ctx, channels.OutboundMessage{
			Channel: item.Channel,
			To:      item.To,
			ToType:  item.ToType,
			Text:    item.Text,
		})

		//如果失败是因为 ctx 被取消，比如程序正在退出，那就直接返回，不把它记成业务失败。
		//如果是普通发送失败，比如飞书 API 报错、网络失败、channel 不存在，就调用 outbox.Fail。
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if failErr := s.outbox.Fail(ctx, item.ID, err); failErr != nil {
				return failErr
			}
			s.addFailure()
			continue
		}

		//Ack 表示确认投递成功。
		//对于文件型 outbox，就是删除 pending 文件：
		if err := s.outbox.Ack(ctx, item.ID); err != nil {
			return err
		}
		s.addSuccess()
	}
	return nil
}

func (s *Service) Pending(ctx context.Context) ([]Item, error) {
	if s == nil {
		return nil, nil
	}
	return s.outbox.Pending(ctx)
}

func (s *Service) Failed(ctx context.Context) ([]Item, error) {
	if s == nil {
		return nil, nil
	}
	return s.outbox.Failed(ctx)
}

func (s *Service) RetryFailed(ctx context.Context) (int, error) {
	if s == nil {
		return 0, nil
	}
	count, err := s.outbox.RetryFailed(ctx)
	if err != nil {
		return count, err
	}
	if count > 0 {
		s.notify()
	}
	return count, nil
}

func (s *Service) Stats(ctx context.Context) (Stats, error) {
	if s == nil {
		return Stats{}, nil
	}
	pending, err := s.outbox.Pending(ctx)
	if err != nil {
		return Stats{}, err
	}
	failed, err := s.outbox.Failed(ctx)
	if err != nil {
		return Stats{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Pending:        len(pending),
		Failed:         len(failed),
		TotalAttempted: s.totalAttempted,
		TotalSucceeded: s.totalSucceeded,
		TotalFailed:    s.totalFailed,
	}, nil
}

func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) cleanup(ctx context.Context) {
	type cleaner interface {
		Cleanup(context.Context) error
	}
	if outbox, ok := s.outbox.(cleaner); ok {
		_ = outbox.Cleanup(ctx)
	}
}

func (s *Service) addAttempt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalAttempted++
}

func (s *Service) addSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalSucceeded++
}

func (s *Service) addFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalFailed++
}
