package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "pending:"

type Store struct {
	rdb *redis.Client
}

func New(addr string) *Store {
	rdb := redis.NewClient(&redis.Options{
		Addr: addr,
	})
	return &Store{rdb: rdb}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
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
		var d PendingDeploy
		if err := json.Unmarshal([]byte(v.(string)), &d); err != nil {
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
