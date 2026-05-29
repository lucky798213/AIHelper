package resilience

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"AIHelper/internal/llm"
)

const DefaultMaxOverflowCompactions = 3

type ClientFactory func(profile AuthProfile) (llm.Client, error)

type Stats struct {
	TotalAttempts    int
	TotalSuccesses   int
	TotalFailures    int
	TotalCompactions int
	TotalRotations   int
}

type ClientConfig struct {
	Profiles               *ProfileManager
	ClientFactory          ClientFactory
	FallbackModels         []string
	ContextGuard           ContextGuard
	MaxOverflowCompactions int
}

type ResilientClient struct {
	profiles               *ProfileManager //profile 指的是 LLM 认证配置档 / API Key 档案
	factory                ClientFactory
	fallbackModels         []string
	guard                  ContextGuard
	maxOverflowCompactions int

	statsMu sync.Mutex
	stats   Stats
}

func NewClient(cfg ClientConfig) (*ResilientClient, error) {
	if cfg.Profiles == nil {
		return nil, fmt.Errorf("resilience profiles are required")
	}
	if cfg.ClientFactory == nil {
		return nil, fmt.Errorf("resilience client factory is required")
	}
	maxCompactions := cfg.MaxOverflowCompactions
	if maxCompactions <= 0 {
		maxCompactions = DefaultMaxOverflowCompactions
	}
	guard := cfg.ContextGuard
	if guard.ContextSafeTokens <= 0 || guard.MaxToolOutputChars <= 0 {
		guard = NewContextGuard(guard.ContextSafeTokens, guard.MaxToolOutputChars)
	}
	return &ResilientClient{
		profiles:               cfg.Profiles,
		factory:                cfg.ClientFactory,
		fallbackModels:         append([]string(nil), cfg.FallbackModels...),
		guard:                  guard,
		maxOverflowCompactions: maxCompactions,
	}, nil
}

func (c *ResilientClient) CreateMessage(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}

	baseReq := cloneRequest(req)
	resp, err := c.runWithProfiles(ctx, baseReq)
	if err == nil {
		return resp, nil
	}
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}

	lastErr := err
	for _, fallbackModel := range c.fallbackModels {
		if fallbackModel == "" {
			continue
		}
		c.profiles.ResetCooldownsFor(ReasonRateLimit, ReasonTimeout)

		fallbackReq := cloneRequest(baseReq)
		fallbackReq.Model = fallbackModel
		resp, err := c.runWithProfiles(ctx, fallbackReq)
		if err == nil {
			return resp, nil
		}
		if err := ctx.Err(); err != nil {
			return llm.Response{}, err
		}
		lastErr = err
	}

	if lastErr == nil {
		return llm.Response{}, fmt.Errorf("resilience exhausted profiles and fallback models")
	}
	return llm.Response{}, fmt.Errorf("resilience exhausted profiles and fallback models: %w", lastErr)
}

func (c *ResilientClient) Stats() Stats {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	return c.stats
}

func (c *ResilientClient) runWithProfiles(ctx context.Context, req llm.Request) (llm.Response, error) {
	//维护已尝试过的 profile，防止同一个 auth profile 在一次请求里被重复使用
	tried := make(map[string]bool)

	//记录 本轮 profile 尝试过程中最近一次失败的真实原因。
	var lastErr error

	//记录轮换的次数
	attemptedProfiles := 0

	//循环挑选可用 profile
	for len(tried) < c.profiles.Len() {
		//从 profile 池里选一个当前可用、且没试过的 profile。没有可用 profile 就退出。
		profile, ok := c.profiles.SelectAvailable(tried)
		if !ok {
			break
		}

		//先记录他被尝试过了
		tried[profile.Name] = true

		//记录轮换次数，第一次不算轮换；从第二个 profile 开始算一次 rotation。
		if attemptedProfiles > 0 {
			c.incRotations()
		}

		//尝试+1
		attemptedProfiles++

		//用当前 profile 创建 client，如果创建失败，就认为这个 profile 失败，进入冷却，然后换下一个 profile。
		client, err := c.factory(profile)
		if err != nil {
			lastErr = err
			c.recordFailure()
			c.profiles.MarkFailure(profile.Name, ReasonUnknown, time.Duration(cooldownForReason(ReasonUnknown))*time.Second)
			continue
		}

		layerReq := cloneRequest(req)

		//固定次数的尝试调用 ai
		for compactAttempt := 0; compactAttempt < c.maxOverflowCompactions; compactAttempt++ {
			if err := ctx.Err(); err != nil {
				return llm.Response{}, err
			}

			//尝试次数+1
			c.recordAttempt()

			//对当前 profile 发请求
			resp, err := client.CreateMessage(ctx, layerReq)
			if err == nil {
				//直接返回响应。
				c.profiles.MarkSuccess(profile.Name)
				c.recordSuccess()
				return resp, nil
			}
			if err := ctx.Err(); err != nil {
				return llm.Response{}, err
			}

			lastErr = err

			//失败时分类
			reason := ClassifyFailure(err)
			c.recordFailure()

			//如果失败原因是上下文溢出
			if reason == ReasonOverflow {
				//也就是先截断工具结果，再调用 CompactHistory 压缩上下文，然后用压缩后的请求继续重试当前 profile。
				//确保没有超过最大尝试次数
				if compactAttempt < c.maxOverflowCompactions-1 {
					//计数+1
					c.recordCompaction()
					truncated := c.guard.TruncateToolResults(layerReq.Messages)
					compactReq := cloneRequest(layerReq)
					compactReq.Messages = truncated
					layerReq.Messages = c.guard.CompactHistory(ctx, client, compactReq)
					continue
				}
				c.profiles.MarkFailure(profile.Name, reason, time.Duration(cooldownForReason(reason))*time.Second)
				break
			}

			c.profiles.MarkFailure(profile.Name, reason, time.Duration(cooldownForReason(reason))*time.Second)
			break
		}
	}

	if lastErr == nil {
		return llm.Response{}, fmt.Errorf("no available auth profiles")
	}
	return llm.Response{}, fmt.Errorf("all auth profiles exhausted: %w", lastErr)
}

func (c *ResilientClient) recordAttempt() {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	c.stats.TotalAttempts++
}

func (c *ResilientClient) recordSuccess() {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	c.stats.TotalSuccesses++
}

func (c *ResilientClient) recordFailure() {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	c.stats.TotalFailures++
}

func (c *ResilientClient) recordCompaction() {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	c.stats.TotalCompactions++
}

// 总轮询次数+1
func (c *ResilientClient) incRotations() {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	c.stats.TotalRotations++
}

func cloneRequest(req llm.Request) llm.Request {
	req.Messages = cloneMessages(req.Messages)
	req.Tools = append([]llm.ToolSchema(nil), req.Tools...)
	return req
}

func cloneMessages(messages []llm.Message) []llm.Message {
	copied := make([]llm.Message, len(messages))
	for i, msg := range messages {
		copied[i] = msg
		if len(msg.ToolCalls) > 0 {
			copied[i].ToolCalls = make([]llm.ToolCall, len(msg.ToolCalls))
			for j, call := range msg.ToolCalls {
				copied[i].ToolCalls[j] = call
				if call.Input != nil {
					copied[i].ToolCalls[j].Input = append(json.RawMessage(nil), call.Input...)
				}
			}
		}
	}
	return copied
}
