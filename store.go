package fleetcharging

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// IdemRecord 记录某个外部请求号已受理的内容与对应计划，
// 用于幂等重放：同号同内容返回原计划，同号不同内容报冲突。
type IdemRecord struct {
	Request Request `json:"request"`
	PlanID  string  `json:"plan_id"`
}

// snapshot 是服务的完整持久化状态。
type snapshot struct {
	Slots       []Slot                `json:"slots"`
	Plans       []*Plan               `json:"plans"`
	Idempotency map[string]IdemRecord `json:"idempotency"`
	Counter     int64                 `json:"counter"`
}

// Store 是持久化抽象。所有方法都在 Service 的全局锁内被调用，
// 实现自身无需关心并发。
type Store interface {
	Load() (snapshot, error)
	Save(s snapshot) error
}

// MemoryStore 仅保存在内存中，供测试使用。
type MemoryStore struct {
	Data snapshot
	Had  bool
}

func (m *MemoryStore) Load() (snapshot, error) {
	if !m.Had {
		return emptySnapshot(), nil
	}
	return cloneSnapshot(m.Data), nil
}

func (m *MemoryStore) Save(s snapshot) error {
	m.Data = cloneSnapshot(s)
	m.Had = true
	return nil
}

// FileStore 将状态以 JSON 快照形式落盘。写入采用临时文件 + rename，
// 保证磁盘上要么是完整的旧状态、要么是完整的新状态。
type FileStore struct {
	Path string

	mu sync.Mutex // 防御同一进程内多个实例写同一路径
}

// NewFileStore 创建落盘到 path 的存储。
func NewFileStore(path string) *FileStore { return &FileStore{Path: path} }

func (f *FileStore) Load() (snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return emptySnapshot(), nil
	}
	if err != nil {
		return snapshot{}, err
	}
	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return snapshot{}, err
	}
	if s.Idempotency == nil {
		s.Idempotency = map[string]IdemRecord{}
	}
	return s, nil
}

func (f *FileStore) Save(s snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}

func emptySnapshot() snapshot {
	return snapshot{Idempotency: map[string]IdemRecord{}}
}

func cloneSnapshot(s snapshot) snapshot {
	cp := s
	cp.Slots = append([]Slot(nil), s.Slots...)
	cp.Plans = make([]*Plan, len(s.Plans))
	for i, p := range s.Plans {
		pc := *p
		pc.Allocations = append([]Allocation(nil), p.Allocations...)
		cp.Plans[i] = &pc
	}
	cp.Idempotency = make(map[string]IdemRecord, len(s.Idempotency))
	for k, v := range s.Idempotency {
		cp.Idempotency[k] = v
	}
	return cp
}
