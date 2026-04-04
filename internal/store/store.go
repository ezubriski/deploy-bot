package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	keyPrefix      = "pending:"
	lockPrefix     = "lock:"
	sysLockPrefix  = "syslock:"
	threadPrefix   = "thread:"
	historyKey     = "history"
	// HistoryMaxLen is the maximum number of entries kept in the history list.
	HistoryMaxLen = 100
)

type Store struct {
	rdb *redis.Client
}

func New(addr, password string) *Store {
	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
	})
	return &Store{rdb: rdb}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
}

// WaitForRedis attempts to ping Redis with retries over the given timeout.
// It pings immediately, then every 5 seconds until success or timeout.
func (s *Store) WaitForRedis(ctx context.Context, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	if err := s.Ping(ctx); err == nil {
		return nil
	}

	for {
		select {
		case <-deadline:
			return fmt.Errorf("redis not available after %s", timeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.Ping(ctx); err == nil {
				return nil
			}
		}
	}
}

// Redis returns the underlying Redis client, used by components that need
// direct access (e.g. the queue worker).
func (s *Store) Redis() *redis.Client {
	return s.rdb
}

func key(prNumber int) string {
	return fmt.Sprintf("%s%d", keyPrefix, prNumber)
}

func (s *Store) Set(ctx context.Context, d *PendingDeploy, ttl time.Duration) error {
	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("marshal deploy: %w", err)
	}
	return s.rdb.Set(ctx, key(d.PRNumber), data, ttl).Err()
}

func (s *Store) Get(ctx context.Context, prNumber int) (*PendingDeploy, error) {
	data, err := s.rdb.Get(ctx, key(prNumber)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get deploy: %w", err)
	}
	var d PendingDeploy
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("unmarshal deploy: %w", err)
	}
	return &d, nil
}

func (s *Store) Delete(ctx context.Context, prNumber int) error {
	return s.rdb.Del(ctx, key(prNumber)).Err()
}

func (s *Store) UpdateState(ctx context.Context, prNumber int, state string) error {
	d, err := s.Get(ctx, prNumber)
	if err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("deploy %d not found", prNumber)
	}
	d.State = state
	ttl := time.Until(d.ExpiresAt)
	if ttl <= 0 {
		ttl = time.Minute
	}
	return s.Set(ctx, d, ttl)
}

func (s *Store) GetAll(ctx context.Context) ([]*PendingDeploy, error) {
	keys, err := s.rdb.Keys(ctx, keyPrefix+"*").Result()
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("mget deploys: %w", err)
	}
	var deploys []*PendingDeploy
	for _, v := range vals {
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		var d PendingDeploy
		if err := json.Unmarshal([]byte(s), &d); err != nil {
			continue
		}
		deploys = append(deploys, &d)
	}
	return deploys, nil
}

func (s *Store) GetExpired(ctx context.Context) ([]*PendingDeploy, error) {
	all, err := s.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var expired []*PendingDeploy
	for _, d := range all {
		if now.After(d.ExpiresAt) {
			expired = append(expired, d)
		}
	}
	return expired, nil
}

// PushHistory prepends a HistoryEntry to the history list and trims it to
// historyMaxLen entries. Both operations run in a single pipeline.
func (s *Store) PushHistory(ctx context.Context, e HistoryEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal history entry: %w", err)
	}
	pipe := s.rdb.Pipeline()
	pipe.LPush(ctx, historyKey, data)
	pipe.LTrim(ctx, historyKey, 0, HistoryMaxLen-1)
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("push history: %w", err)
	}
	return nil
}

// GetHistory returns up to limit entries from the history list, newest first.
func (s *Store) GetHistory(ctx context.Context, limit int) ([]HistoryEntry, error) {
	vals, err := s.rdb.LRange(ctx, historyKey, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, fmt.Errorf("get history: %w", err)
	}
	entries := make([]HistoryEntry, 0, len(vals))
	for _, v := range vals {
		var e HistoryEntry
		if err := json.Unmarshal([]byte(v), &e); err != nil {
			continue // skip malformed entries
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// GetByEnvApp returns the first pending deploy found for the given environment
// and app, or nil if none exists. Used to surface the existing PR link when a
// lock is contested.
func (s *Store) GetByEnvApp(ctx context.Context, env, app string) (*PendingDeploy, error) {
	all, err := s.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		if d.Environment == env && d.App == app {
			return d, nil
		}
	}
	return nil, nil
}

// deployLockKey returns the Redis key for a per-app deploy lock scoped to its
// environment, preventing false conflicts between same-named apps in different
// environments.
func deployLockKey(env, app string) string {
	return lockPrefix + env + "/" + app
}

// AcquireLock attempts to claim the per-app deploy lock. It returns true if
// the lock was acquired, false if another deploy is already in progress.
// holder is stored as the lock value (use the requester's Slack user ID so
// "who holds this?" is answerable by inspecting Redis directly).
func (s *Store) AcquireLock(ctx context.Context, env, app, holder string, ttl time.Duration) (bool, error) {
	ok, err := s.rdb.SetNX(ctx, deployLockKey(env, app), holder, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("acquire lock %s/%s: %w", env, app, err)
	}
	return ok, nil
}

// IsLocked returns true if a deploy lock is currently held for the given app.
func (s *Store) IsLocked(ctx context.Context, env, app string) (bool, error) {
	n, err := s.rdb.Exists(ctx, deployLockKey(env, app)).Result()
	if err != nil {
		return false, fmt.Errorf("check lock %s/%s: %w", env, app, err)
	}
	return n > 0, nil
}

// ReleaseLock deletes the per-app deploy lock.
func (s *Store) ReleaseLock(ctx context.Context, env, app string) error {
	return s.rdb.Del(ctx, deployLockKey(env, app)).Err()
}

// TryLock acquires a named system lock (distinct from per-app deploy locks).
// Returns true if the lock was acquired. Used for single-instance coordination
// of background processes (e.g. the sweeper) without leader election.
func (s *Store) TryLock(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	ok, err := s.rdb.SetNX(ctx, sysLockPrefix+name, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("trylock %s: %w", name, err)
	}
	return ok, nil
}

// GetThreadTS returns the Slack thread timestamp for an environment's deploy
// thread, or empty string if none exists.
func (s *Store) GetThreadTS(ctx context.Context, env string) (string, error) {
	ts, err := s.rdb.Get(ctx, threadPrefix+env).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get thread ts %s: %w", env, err)
	}
	return ts, nil
}

// SetThreadTS atomically stores the Slack thread timestamp for an environment's
// deploy thread. Uses SET NX so only the first caller wins — concurrent workers
// will not create duplicate threads. Returns true if the value was set.
func (s *Store) SetThreadTS(ctx context.Context, env, ts string, ttl time.Duration) (bool, error) {
	return s.rdb.SetNX(ctx, threadPrefix+env, ts, ttl).Result()
}

// PRNumberFromKey extracts the PR number from a Redis key like "pending:123".
func PRNumberFromKey(k string) (int, bool) {
	s := strings.TrimPrefix(k, keyPrefix)
	if s == k {
		return 0, false
	}
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err == nil
}
