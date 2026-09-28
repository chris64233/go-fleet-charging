package fleetcharging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// snapshot 是服务的全部持久化状态。
type snapshot struct {
	Stations map[string]StationConfig `json:"stations"`
	Plans    map[string]*Plan         `json:"plans"` // key: RequestID
	Counter  int64                    `json:"counter"`
}

func newSnapshot() *snapshot {
	return &snapshot{
		Stations: map[string]StationConfig{},
		Plans:    map[string]*Plan{},
	}
}

// Store 是持久化抽象：Load 返回最新快照（无数据时返回 nil, nil），
// Save 必须原子替换全部内容（不允许部分写入）。
type Store interface {
	Load(ctx context.Context) (*snapshot, error)
	Save(ctx context.Context, snap *snapshot) error
}

// MemoryStore 是进程内 Store，主要用于测试。
type MemoryStore struct {
	mu   sync.Mutex
	snap *snapshot
}

// NewMemoryStore 返回空的内存 Store。
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (m *MemoryStore) Load(context.Context) (*snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneSnapshot(m.snap), nil
}

func (m *MemoryStore) Save(_ context.Context, snap *snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snap = cloneSnapshot(snap)
	return nil
}

// FileStore 以单个 JSON 文件持久化，写入采用“临时文件 + rename”保证原子性。
type FileStore struct {
	path string
	mu   sync.Mutex
}

// NewFileStore 返回以 path 为存储文件的 Store。
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

func (f *FileStore) Load(context.Context) (*snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.path, err)
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("decode %s: %w", f.path, err)
	}
	if snap.Stations == nil {
		snap.Stations = map[string]StationConfig{}
	}
	if snap.Plans == nil {
		snap.Plans = map[string]*Plan{}
	}
	return &snap, nil
}

func (f *FileStore) Save(_ context.Context, snap *snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, f.path, err)
	}
	return nil
}

// Path 返回底层文件路径（测试用）。
func (f *FileStore) Path() string { return f.path }

// EnsureDir 创建存储文件所在目录（可选便捷方法）。
func (f *FileStore) EnsureDir() error {
	return os.MkdirAll(filepath.Dir(f.path), 0o755)
}

func cloneSnapshot(s *snapshot) *snapshot {
	if s == nil {
		return nil
	}
	out := &snapshot{
		Stations: make(map[string]StationConfig, len(s.Stations)),
		Plans:    make(map[string]*Plan, len(s.Plans)),
		Counter:  s.Counter,
	}
	for k, v := range s.Stations {
		segs := append([]Segment(nil), v.Segments...)
		out.Stations[k] = StationConfig{StationID: v.StationID, Segments: segs}
	}
	for k, v := range s.Plans {
		cp := *v
		cp.Allocation = append([]SlotAllocation(nil), v.Allocation...)
		out.Plans[k] = &cp
	}
	return out
}
