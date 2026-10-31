package websearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
)

type Record struct {
	URL     string          `json:"url,omitempty"`
	Title   string          `json:"title,omitempty"`
	Content string          `json:"content,omitempty"`
	Call    json.RawMessage `json:"call,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}
type Store interface {
	Put(context.Context, string, Record) error
	Get(context.Context, string) (Record, error)
}
type RedisStore struct{ Scope string }

func (s RedisStore) key(id string) string {
	h := sha256.Sum256([]byte(s.Scope + "\x00" + id))
	return "managed-web:v1:" + hex.EncodeToString(h[:])
}
func (s RedisStore) Put(ctx context.Context, id string, r Record) error {
	if common.RDB == nil {
		return errors.New("web history storage is unavailable")
	}
	b, err := common.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > MaxBody {
		return errors.New("web history exceeds the storage limit")
	}
	return common.RDB.Set(ctx, s.key(id), b, 24*time.Hour).Err()
}
func (s RedisStore) Get(ctx context.Context, id string) (Record, error) {
	if common.RDB == nil {
		return Record{}, errors.New("web history storage is unavailable")
	}
	b, err := common.RDB.Get(ctx, s.key(id)).Bytes()
	if err != nil {
		return Record{}, errors.New("web reference expired or belongs to another caller; search again")
	}
	var r Record
	err = common.Unmarshal(b, &r)
	return r, err
}
func NewID(prefix string) string { return prefix + stringsID() }
func stringsID() string          { return strings.ReplaceAll(uuid.NewString(), "-", "") }

type Meter struct {
	Search   int `json:"search_requests"`
	Fetch    int `json:"fetch_requests"`
	Attempts int `json:"attempts"`
}
type Session struct {
	Backend    Backend
	Store      Store
	Meter      Meter
	BeforeCall func() error
}

func (s *Session) before() error {
	if s.Meter.Attempts >= MaxCalls {
		return errors.New("web tool call limit exceeded")
	}
	if s.BeforeCall != nil {
		if err := s.BeforeCall(); err != nil {
			return err
		}
	}
	s.Meter.Attempts++
	return nil
}
func (s *Session) Search(ctx context.Context, r SearchRequest) ([]Hit, error) {
	if err := s.before(); err != nil {
		return nil, err
	}
	hits, err := s.Backend.Search(ctx, r)
	if err == nil && len(hits) > 0 {
		s.Meter.Search++
	}
	return hits, err
}
func (s *Session) Fetch(ctx context.Context, target string) (Page, error) {
	if err := s.before(); err != nil {
		return Page{}, err
	}
	page, err := s.Backend.Fetch(ctx, target)
	if err == nil && len(strings.TrimSpace(page.Content)) > 0 {
		s.Meter.Fetch++
	}
	return page, err
}
