package moderation

import (
	"encoding/json"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"
)

// Sanction is one ban/timeout/mute record, keyed by client IP (the only
// semi-stable identifier we have — there are no accounts).
type Sanction struct {
	IP     string    `json:"ip"`
	Reason string    `json:"reason"`
	By     string    `json:"by"`    // moderator name
	Set    time.Time `json:"set"`   // when applied
	Until  time.Time `json:"until"` // zero value = permanent
}

// expired reports whether a non-permanent sanction has elapsed.
func (s Sanction) expired(now time.Time) bool {
	return !s.Until.IsZero() && now.After(s.Until)
}

// Manager owns the filter, admin credentials, and the ban/mute state. Bans
// (including timeouts) persist to disk; mutes are in-memory only.
type Manager struct {
	filter *Filter
	admins *AdminStore

	mu      sync.Mutex
	bans    map[string]Sanction // ip -> ban/timeout (persisted)
	mutes   map[string]Sanction // ip -> mute (in-memory)
	banPath string
}

// NewManager wires the filter and admin store together and loads any persisted
// ban list from banPath (missing file is fine).
func NewManager(filter *Filter, admins *AdminStore, banPath string) *Manager {
	m := &Manager{
		filter:  filter,
		admins:  admins,
		bans:    make(map[string]Sanction),
		mutes:   make(map[string]Sanction),
		banPath: banPath,
	}
	m.loadBans()
	return m
}

func (m *Manager) FilterMessage(msg string) (string, bool) {
	return m.filter.Check(msg)
}

func (m *Manager) AuthenticateAdmin(password string) (string, bool) {
	return m.admins.Authenticate(password)
}

func (m *Manager) FilterSize() int { return m.filter.Size() }
func (m *Manager) AdminCount() int { return m.admins.Count() }

// IsBanned reports whether an IP is currently banned, pruning the entry if its
// timeout has elapsed.
func (m *Manager) IsBanned(ip string) (Sanction, bool) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.bans[ip]
	if !ok {
		return Sanction{}, false
	}
	if s.expired(now) {
		delete(m.bans, ip)
		m.saveBansLocked()
		return Sanction{}, false
	}
	return s, true
}

// IsMuted reports whether an IP is currently muted, pruning expired mutes.
func (m *Manager) IsMuted(ip string) (Sanction, bool) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.mutes[ip]
	if !ok {
		return Sanction{}, false
	}
	if s.expired(now) {
		delete(m.mutes, ip)
		return Sanction{}, false
	}
	return s, true
}

// Ban applies a ban (dur == 0 => permanent; dur > 0 => timeout that expires).
func (m *Manager) Ban(ip, by, reason string, dur time.Duration) {
	now := time.Now()
	s := Sanction{IP: ip, Reason: reason, By: by, Set: now}
	if dur > 0 {
		s.Until = now.Add(dur)
	}
	m.mu.Lock()
	m.bans[ip] = s
	m.saveBansLocked()
	m.mu.Unlock()
	slog.Info("mod ban", "ip", ip, "by", by, "reason", reason, "until", s.Until)
}

// Mute silences an IP's chat for dur (dur == 0 => until manually unmuted).
func (m *Manager) Mute(ip, by, reason string, dur time.Duration) {
	now := time.Now()
	s := Sanction{IP: ip, Reason: reason, By: by, Set: now}
	if dur > 0 {
		s.Until = now.Add(dur)
	}
	m.mu.Lock()
	m.mutes[ip] = s
	m.mu.Unlock()
	slog.Info("mod mute", "ip", ip, "by", by, "reason", reason, "until", s.Until)
}

// Unban removes a ban/timeout. Returns whether something was removed.
func (m *Manager) Unban(ip, by string) bool {
	m.mu.Lock()
	_, ok := m.bans[ip]
	if ok {
		delete(m.bans, ip)
		m.saveBansLocked()
	}
	m.mu.Unlock()
	if ok {
		slog.Info("mod unban", "ip", ip, "by", by)
	}
	return ok
}

// Unmute removes a mute. Returns whether something was removed.
func (m *Manager) Unmute(ip, by string) bool {
	m.mu.Lock()
	_, ok := m.mutes[ip]
	if ok {
		delete(m.mutes, ip)
	}
	m.mu.Unlock()
	if ok {
		slog.Info("mod unmute", "ip", ip, "by", by)
	}
	return ok
}

// ListSanctions returns current bans and mutes (expired entries pruned), sorted
// by IP, for an admin's MOD_LIST query.
func (m *Manager) ListSanctions() (bans, mutes []Sanction) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := false
	for ip, s := range m.bans {
		if s.expired(now) {
			delete(m.bans, ip)
			changed = true
			continue
		}
		bans = append(bans, s)
	}
	if changed {
		m.saveBansLocked()
	}
	for ip, s := range m.mutes {
		if s.expired(now) {
			delete(m.mutes, ip)
			continue
		}
		mutes = append(mutes, s)
	}
	sort.Slice(bans, func(i, j int) bool { return bans[i].IP < bans[j].IP })
	sort.Slice(mutes, func(i, j int) bool { return mutes[i].IP < mutes[j].IP })
	return bans, mutes
}

// --- persistence (bans only) ---

func (m *Manager) loadBans() {
	if m.banPath == "" {
		return
	}
	data, err := os.ReadFile(m.banPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("ban list read failed", "path", m.banPath, "err", err)
		}
		return
	}
	var list []Sanction
	if err := json.Unmarshal(data, &list); err != nil {
		slog.Warn("ban list parse failed", "path", m.banPath, "err", err)
		return
	}
	now := time.Now()
	for _, s := range list {
		if s.IP == "" || s.expired(now) {
			continue
		}
		m.bans[s.IP] = s
	}
	slog.Info("ban list loaded", "path", m.banPath, "count", len(m.bans))
}

// saveBansLocked writes the ban map to disk. Caller must hold m.mu.
func (m *Manager) saveBansLocked() {
	if m.banPath == "" {
		return
	}
	list := make([]Sanction, 0, len(m.bans))
	for _, s := range m.bans {
		list = append(list, s)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		slog.Warn("ban list marshal failed", "err", err)
		return
	}
	tmp := m.banPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("ban list write failed", "path", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, m.banPath); err != nil {
		slog.Warn("ban list rename failed", "path", m.banPath, "err", err)
	}
}
