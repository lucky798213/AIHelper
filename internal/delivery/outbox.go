package delivery

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const DefaultMaxRetries = 5

var defaultBackoffSchedule = []time.Duration{
	5 * time.Second,
	25 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
}

type Item struct {
	ID          string    `json:"id"`
	Channel     string    `json:"channel"`
	To          string    `json:"to"`
	ToType      string    `json:"to_type,omitempty"`
	Text        string    `json:"text"`
	RetryCount  int       `json:"retry_count"`
	LastError   string    `json:"last_error,omitempty"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
	NextRetryAt time.Time `json:"next_retry_at,omitempty"`
}

type FileOutboxConfig struct {
	Dir        string
	MaxRetries int
	Now        func() time.Time
	Rand       *mrand.Rand
}

type FileOutbox struct {
	dir        string
	failedDir  string
	maxRetries int
	now        func() time.Time
	rand       *mrand.Rand
	mu         sync.Mutex
}

func NewFileOutbox(cfg FileOutboxConfig) (*FileOutbox, error) {
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		return nil, errors.New("delivery queue dir is required")
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	rng := cfg.Rand
	if rng == nil {
		rng = mrand.New(mrand.NewSource(time.Now().UnixNano()))
	}
	return &FileOutbox{
		dir:        filepath.Clean(dir),
		failedDir:  filepath.Join(filepath.Clean(dir), "failed"),
		maxRetries: maxRetries,
		now:        now,
		rand:       rng,
	}, nil
}

func (q *FileOutbox) Enqueue(ctx context.Context, item Item) (string, error) {
	//先检查 context 是否已取消
	if err := ctx.Err(); err != nil {
		return "", err
	}

	//校验必要字段
	if strings.TrimSpace(item.Channel) == "" {
		return "", errors.New("delivery channel is required")
	}
	if strings.TrimSpace(item.To) == "" {
		return "", errors.New("delivery recipient is required")
	}

	//如果没有 ID，就生成一个
	if item.ID == "" {
		item.ID = newDeliveryID()
	}

	//如果没有入队时间，就填当前时间
	if item.EnqueuedAt.IsZero() {
		item.EnqueuedAt = q.now()
	}

	//加锁，保证文件队列并发安全
	q.mu.Lock()
	defer q.mu.Unlock()

	//确保队列目录存在
	if err := q.ensureDirsLocked(); err != nil {
		return "", err
	}

	//把消息写成一个 outbox entry 文件
	if err := q.writeEntryLocked(item, q.entryPath(item.ID)); err != nil {
		return "", err
	}
	return item.ID, nil
}

func (q *FileOutbox) Ack(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	err := os.Remove(q.entryPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (q *FileOutbox) Fail(ctx context.Context, id string, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	item, err := q.readEntryLocked(q.entryPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	item.RetryCount++
	if cause != nil {
		item.LastError = cause.Error()
	}
	if item.RetryCount >= q.maxRetries {
		if err := q.ensureDirsLocked(); err != nil {
			return err
		}
		if err := q.writeEntryLocked(item, q.entryPath(item.ID)); err != nil {
			return err
		}
		return q.moveToFailedLocked(item.ID)
	}

	item.NextRetryAt = q.now().Add(q.backoffLocked(item.RetryCount))
	return q.writeEntryLocked(item, q.entryPath(item.ID))
}

func (q *FileOutbox) Pending(ctx context.Context) ([]Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.loadDirLocked(q.dir)
}

func (q *FileOutbox) Failed(ctx context.Context) ([]Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.loadDirLocked(q.failedDir)
}

func (q *FileOutbox) RetryFailed(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	items, err := q.loadDirLocked(q.failedDir)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}
	if err := q.ensureDirsLocked(); err != nil {
		return 0, err
	}

	count := 0
	for _, item := range items {
		item.RetryCount = 0
		item.LastError = ""
		item.NextRetryAt = time.Time{}
		if err := q.writeEntryLocked(item, q.entryPath(item.ID)); err != nil {
			return count, err
		}
		if err := os.Remove(q.failedPath(item.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return count, err
		}
		count++
	}
	return count, nil
}

func (q *FileOutbox) Cleanup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cleanupTempsLocked()
}

func (q *FileOutbox) loadDirLocked(dir string) ([]Item, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".tmp.") {
			continue
		}
		item, err := q.readEntryLocked(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].EnqueuedAt.Before(items[j].EnqueuedAt)
	})
	return items, nil
}

func (q *FileOutbox) readEntryLocked(path string) (Item, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Item{}, err
	}
	var item Item
	if err := json.Unmarshal(raw, &item); err != nil {
		return Item{}, err
	}
	if strings.TrimSpace(item.ID) == "" {
		return Item{}, errors.New("delivery item id is required")
	}
	return item, nil
}

func (q *FileOutbox) writeEntryLocked(item Item, finalPath string) error {
	//确保目录存在
	if err := q.ensureDirsLocked(); err != nil {
		return err
	}

	//在 outbox 目录下创建临时文件
	tmp, err := os.CreateTemp(q.dir, ".tmp.*.json")
	if err != nil {
		return err
	}

	//记录临时文件名，并设置失败清理，如果后面任意一步失败，committed 还是 false，临时文件会被删除。
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	//把 Item 编码成 JSON
	encoder := json.NewEncoder(tmp) //创建一个 JSON 编码器，tmp：是一个输出目标，转换成 JSON 格式，并直接写入 tmp 里
	encoder.SetIndent("", "  ")     //设置 JSON 缩进格式化
	if err := encoder.Encode(item); err != nil {
		_ = tmp.Close()
		return err
	}

	//强制刷盘，把临时文件改名成正式文件
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	//把临时文件改名成正式文件
	if err := os.Rename(tmpName, finalPath); err != nil {
		return err
	}
	committed = true
	return nil
}

func (q *FileOutbox) moveToFailedLocked(id string) error {
	if err := os.MkdirAll(q.failedDir, 0o755); err != nil {
		return err
	}
	err := os.Rename(q.entryPath(id), q.failedPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (q *FileOutbox) cleanupTempsLocked() error {
	entries, err := os.ReadDir(q.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".tmp.") {
			continue
		}
		if err := os.Remove(filepath.Join(q.dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (q *FileOutbox) ensureDirsLocked() error {
	if err := os.MkdirAll(q.dir, 0o755); err != nil {
		return err
	}
	return os.MkdirAll(q.failedDir, 0o755)
}

func (q *FileOutbox) backoffLocked(retryCount int) time.Duration {
	if retryCount <= 0 {
		return 0
	}
	idx := retryCount - 1
	if idx >= len(defaultBackoffSchedule) {
		idx = len(defaultBackoffSchedule) - 1
	}
	base := defaultBackoffSchedule[idx]
	jitterRange := int64(base / 5)
	if jitterRange <= 0 {
		return base
	}
	jitter := time.Duration(q.rand.Int63n(jitterRange*2+1) - jitterRange)
	return base + jitter
}

func (q *FileOutbox) entryPath(id string) string {
	return filepath.Join(q.dir, filepath.Base(id)+".json")
}

func (q *FileOutbox) failedPath(id string) string {
	return filepath.Join(q.failedDir, filepath.Base(id)+".json")
}

func newDeliveryID() string {
	var buf [6]byte
	if _, err := crand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}
