package resilience

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type AuthProfile struct {
	Name          string
	Provider      string
	BaseURL       string
	APIKey        string
	CooldownUntil time.Time
	FailureReason FailoverReason
	LastGoodAt    time.Time
}

type ProfileSnapshot struct {
	Name              string
	Provider          string
	Available         bool
	CooldownRemaining time.Duration
	FailureReason     FailoverReason
	LastGoodAt        time.Time
}

type ProfileManager struct {
	mu       sync.Mutex
	profiles []AuthProfile
	now      func() time.Time
}

func NewProfileManager(profiles []AuthProfile) (*ProfileManager, error) {
	if len(profiles) == 0 {
		return nil, fmt.Errorf("at least one auth profile is required")
	}

	copied := make([]AuthProfile, len(profiles))
	seen := make(map[string]struct{}, len(profiles))
	for i, profile := range profiles {
		profile.Name = strings.TrimSpace(profile.Name)
		if profile.Name == "" {
			profile.Name = fmt.Sprintf("profile-%d", i+1)
		}
		if _, ok := seen[profile.Name]; ok {
			return nil, fmt.Errorf("duplicate auth profile %q", profile.Name)
		}
		seen[profile.Name] = struct{}{}
		copied[i] = profile
	}

	return &ProfileManager{
		profiles: copied,
		now:      time.Now,
	}, nil
}

func (m *ProfileManager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.profiles)
}

func (m *ProfileManager) SelectAvailable(tried map[string]bool) (AuthProfile, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	//获取当前时间
	now := m.now()
	for _, profile := range m.profiles {
		//按顺序选出一个没有尝试过的 profile
		if tried != nil && tried[profile.Name] {
			continue
		}

		//CooldownUntil（冷却） 是否在 now 之后
		if !profile.CooldownUntil.After(now) {
			return profile, true
		}
	}
	return AuthProfile{}, false
}

func (m *ProfileManager) MarkFailure(name string, reason FailoverReason, cooldown time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	for i := range m.profiles {
		if m.profiles[i].Name != name {
			continue
		}
		m.profiles[i].CooldownUntil = now.Add(cooldown)
		m.profiles[i].FailureReason = reason
		return
	}
}

func (m *ProfileManager) MarkSuccess(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	for i := range m.profiles {
		if m.profiles[i].Name != name {
			continue
		}
		m.profiles[i].CooldownUntil = time.Time{}
		m.profiles[i].FailureReason = ""
		m.profiles[i].LastGoodAt = now
		return
	}
}

func (m *ProfileManager) ResetCooldownsFor(reasons ...FailoverReason) {
	m.mu.Lock()
	defer m.mu.Unlock()

	allowed := make(map[FailoverReason]struct{}, len(reasons))
	for _, reason := range reasons {
		allowed[reason] = struct{}{}
	}
	for i := range m.profiles {
		if _, ok := allowed[m.profiles[i].FailureReason]; ok {
			m.profiles[i].CooldownUntil = time.Time{}
		}
	}
}

func (m *ProfileManager) Snapshot() []ProfileSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	result := make([]ProfileSnapshot, 0, len(m.profiles))
	for _, profile := range m.profiles {
		remaining := time.Duration(0)
		if profile.CooldownUntil.After(now) {
			remaining = profile.CooldownUntil.Sub(now)
		}
		result = append(result, ProfileSnapshot{
			Name:              profile.Name,
			Provider:          profile.Provider,
			Available:         remaining == 0,
			CooldownRemaining: remaining,
			FailureReason:     profile.FailureReason,
			LastGoodAt:        profile.LastGoodAt,
		})
	}
	return result
}

func (m *ProfileManager) setNowForTest(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}
