package fleetcharging

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Service 车队充电容量预订服务。所有公开方法都持有同一把互斥锁，
// 因此并发提交、修改、取消是可串行化的：只有仍满足全部容量约束的
// 计划组合才能提交成功。
type Service struct {
	mu      sync.Mutex
	slots   []Slot // 按 Start 升序、互不重叠
	plans   map[string]*Plan
	idem    map[string]IdemRecord
	counter int64
	store   Store
	now     func() time.Time
}

// Option 可选配置。
type Option func(*Service)

// WithClock 注入时钟，测试用。
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService 创建服务并从 store 恢复状态。
func NewService(store Store, opts ...Option) (*Service, error) {
	if store == nil {
		return nil, paramErrorf("store must not be nil")
	}
	snap, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	s := &Service{
		plans: map[string]*Plan{},
		idem:  snap.Idempotency,
		store: store,
		now:   time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	if err := s.restore(snap); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) restore(snap snapshot) error {
	if err := validateSlots(snap.Slots); err != nil {
		return fmt.Errorf("restore slots: %w", err)
	}
	s.slots = snap.Slots
	s.counter = snap.Counter
	for _, p := range snap.Plans {
		if p.ID == "" {
			return fmt.Errorf("restore: plan with empty id")
		}
		if _, dup := s.plans[p.ID]; dup {
			return fmt.Errorf("restore: duplicate plan id %q", p.ID)
		}
		s.plans[p.ID] = p
	}
	return nil
}

// ConfigureSlots 配置站点各时段可用功率。时段必须互不重叠、按时间
// 排列（传入顺序不限，内部排序）。存在生效中的计划时不允许重配，
// 以免已承诺的容量失去依据。
func (s *Service) ConfigureSlots(slots []Slot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sorted := append([]Slot(nil), slots...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start.Before(sorted[j].Start) })
	if err := validateSlots(sorted); err != nil {
		return err
	}
	for _, p := range s.plans {
		if p.State == PlanActive {
			return stateErrorf("cannot reconfigure slots while plan %s is active", p.ID)
		}
	}
	s.slots = sorted
	return s.persistLocked()
}

func validateSlots(slots []Slot) error {
	for i, sl := range slots {
		if err := checkSecondAligned(sl.Start, sl.End); err != nil {
			return err
		}
		if !sl.Start.Before(sl.End) {
			return paramErrorf("slot %d: start %s must be before end %s", i, sl.Start, sl.End)
		}
		if sl.Capacity < 0 {
			return paramErrorf("slot %d: negative capacity %d", i, sl.Capacity)
		}
		if i > 0 && sl.Start.Before(slots[i-1].End) {
			return paramErrorf("slot %d overlaps previous slot", i)
		}
	}
	return nil
}

// Submit 提交充电请求。满足全部约束时整体接受并占用容量，否则整体
// 拒绝、不留下任何占用。相同 ExternalID 且内容相同的重复提交返回原
// 计划；内容不同则返回幂等冲突。
func (s *Service) Submit(req Request) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.idem[req.ExternalID]; ok {
		if rec.Request == req {
			plan := s.plans[rec.PlanID]
			cp := *plan
			return &cp, nil
		}
		return nil, idempotencyErrorf("external id %q already used with different content", req.ExternalID)
	}

	if err := s.validateRequestLocked(req); err != nil {
		return nil, err
	}
	alloc, err := s.planLocked(req, "")
	if err != nil {
		return nil, err
	}

	s.counter++
	now := s.now()
	plan := &Plan{
		ID:          fmt.Sprintf("plan-%d", s.counter),
		Request:     req,
		Allocations: alloc,
		State:       PlanActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	for _, a := range alloc {
		plan.Energy += a.Energy()
	}

	// 先持久化再提交内存状态，失败时不留下任何占用。
	if err := s.withPlanLocked(plan, func() error {
		s.idem[req.ExternalID] = IdemRecord{Request: req, PlanID: plan.ID}
		return s.persistLocked()
	}); err != nil {
		return nil, err
	}
	cp := *plan
	return &cp, nil
}

// Modify 在车辆到达前原子替换计划的占用：新分配整体生效，旧占用整体
// 释放。任一步骤失败都保持原计划不变。
func (s *Service) Modify(planID string, upd UpdateRequest) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	plan, err := s.activePlanLocked(planID)
	if err != nil {
		return nil, err
	}
	if err := s.checkBeforeArrivalLocked(plan); err != nil {
		return nil, err
	}

	req := Request{
		ExternalID: plan.Request.ExternalID,
		VehicleID:  plan.Request.VehicleID,
		Arrival:    upd.Arrival,
		Departure:  upd.Departure,
		MinEnergy:  upd.MinEnergy,
		MaxPower:   upd.MaxPower,
	}
	if err := s.validateRequestLocked(req); err != nil {
		return nil, err
	}
	// 计算新分配时排除本计划的旧占用，实现"替换"而非"叠加"。
	alloc, err := s.planLocked(req, plan.ID)
	if err != nil {
		return nil, err
	}

	oldAlloc, oldReq, oldEnergy, oldUpdated := plan.Allocations, plan.Request, plan.Energy, plan.UpdatedAt
	plan.Allocations = alloc
	plan.Request = req
	plan.Energy = 0
	for _, a := range alloc {
		plan.Energy += a.Energy()
	}
	plan.UpdatedAt = s.now()
	if err := s.persistLocked(); err != nil {
		plan.Allocations, plan.Request, plan.Energy, plan.UpdatedAt = oldAlloc, oldReq, oldEnergy, oldUpdated
		return nil, err
	}
	cp := *plan
	return &cp, nil
}

// Cancel 在车辆到达前取消计划并释放其全部占用。重复取消返回状态
// 错误，不会重复释放。
func (s *Service) Cancel(planID string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	plan, err := s.activePlanLocked(planID)
	if err != nil {
		return nil, err
	}
	if err := s.checkBeforeArrivalLocked(plan); err != nil {
		return nil, err
	}

	plan.State = PlanCancelled
	plan.UpdatedAt = s.now()
	if err := s.persistLocked(); err != nil {
		plan.State = PlanActive
		return nil, err
	}
	cp := *plan
	return &cp, nil
}

// GetPlan 查询计划当前状态。
func (s *Service) GetPlan(planID string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.plans[planID]
	if !ok {
		return nil, paramErrorf("unknown plan %q", planID)
	}
	cp := *plan
	cp.Allocations = append([]Allocation(nil), plan.Allocations...)
	return &cp, nil
}

// Occupancy 返回与 [from, to) 相交的各时段占用情况。
func (s *Service) Occupancy(from, to time.Time) ([]SlotOccupancy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !from.Before(to) {
		return nil, paramErrorf("from %s must be before to %s", from, to)
	}
	allocated := s.allocatedBySlotLocked("")
	var out []SlotOccupancy
	for _, sl := range s.slots {
		if sl.End.After(from) && sl.Start.Before(to) {
			used := allocated[sl.Start]
			out = append(out, SlotOccupancy{
				Start:     sl.Start,
				End:       sl.End,
				Capacity:  sl.Capacity,
				Allocated: used,
				Available: sl.Capacity - used,
			})
		}
	}
	return out, nil
}

// Slots 返回当前配置的时段。
func (s *Service) Slots() []Slot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Slot(nil), s.slots...)
}

// ---- 内部方法（调用时必须已持有 s.mu） ----

func (s *Service) activePlanLocked(planID string) (*Plan, error) {
	plan, ok := s.plans[planID]
	if !ok {
		return nil, paramErrorf("unknown plan %q", planID)
	}
	if plan.State != PlanActive {
		return nil, stateErrorf("plan %s is %s", planID, plan.State)
	}
	return plan, nil
}

// checkBeforeArrivalLocked 保证修改/取消只发生在到达前。与时间越过
// 到达点的竞态由同一把锁消解：要么本操作先完成，要么先越过到达点、
// 本操作被拒绝；占用不会泄漏也不会重复释放。
func (s *Service) checkBeforeArrivalLocked(plan *Plan) error {
	if !s.now().Before(plan.Request.Arrival) {
		return timeErrorf("plan %s: vehicle already arrived at %s", plan.ID, plan.Request.Arrival)
	}
	return nil
}

func (s *Service) validateRequestLocked(req Request) error {
	if req.ExternalID == "" {
		return paramErrorf("external id must not be empty")
	}
	if req.VehicleID == "" {
		return paramErrorf("vehicle id must not be empty")
	}
	if err := checkSecondAligned(req.Arrival, req.Departure); err != nil {
		return err
	}
	if !req.Arrival.Before(req.Departure) {
		return paramErrorf("arrival %s must be before departure %s", req.Arrival, req.Departure)
	}
	if req.MinEnergy <= 0 {
		return paramErrorf("min energy must be positive, got %d", req.MinEnergy)
	}
	if req.MaxPower <= 0 {
		return paramErrorf("max power must be positive, got %d", req.MaxPower)
	}
	if !s.now().Before(req.Arrival) {
		return timeErrorf("arrival %s is not in the future", req.Arrival)
	}
	// 请求区间必须被已配置时段（可跨多个相邻时段）完整覆盖。
	if !s.coveredLocked(req.Arrival, req.Departure) {
		return paramErrorf("configured slots do not cover [%s, %s)", req.Arrival, req.Departure)
	}
	return nil
}

// coveredLocked 报告 [from, to) 是否被连续时段完整覆盖。
func (s *Service) coveredLocked(from, to time.Time) bool {
	cursor := from
	for _, sl := range s.slots {
		if !sl.End.After(cursor) {
			continue // 时段在 cursor 处或之前结束
		}
		if sl.Start.After(cursor) {
			return false // 存在空隙
		}
		cursor = sl.End
		if !cursor.Before(to) {
			return true
		}
	}
	return !cursor.Before(to)
}

// checkSecondAligned 要求时间精确到整秒，使时长换算为整秒、能量计算
// 严格精确。
func checkSecondAligned(ts ...time.Time) error {
	for _, t := range ts {
		if t != t.Truncate(time.Second) {
			return paramErrorf("time %s is not aligned to whole seconds", t)
		}
	}
	return nil
}

// allocatedBySlotLocked 统计各时段已被生效计划占用的功率；
// excludePlanID 用于修改场景排除自身旧占用。
func (s *Service) allocatedBySlotLocked(excludePlanID string) map[time.Time]Milliwatts {
	used := map[time.Time]Milliwatts{}
	for _, p := range s.plans {
		if p.State != PlanActive || p.ID == excludePlanID {
			continue
		}
		for _, a := range p.Allocations {
			used[a.SlotStart] += a.Power
		}
	}
	return used
}

// planLocked 为请求计算逐时段分配。采用最早时段优先的贪心：在每个
// 相交时段内分配 min(剩余容量, MaxPower, 达到最低电量所需功率)。
// 任一约束不满足即返回错误，调用方不得部分提交。
func (s *Service) planLocked(req Request, excludePlanID string) ([]Allocation, error) {
	used := s.allocatedBySlotLocked(excludePlanID)

	var alloc []Allocation
	remaining := req.MinEnergy
	for _, sl := range s.slots {
		if remaining <= 0 {
			break
		}
		start := maxTime(req.Arrival, sl.Start)
		end := minTime(req.Departure, sl.End)
		if !start.Before(end) {
			continue
		}
		avail := sl.Capacity - used[sl.Start]
		if avail > req.MaxPower {
			avail = req.MaxPower
		}
		if avail <= 0 {
			continue
		}
		dur := end.Sub(start)
		// 该时段内为凑足剩余电量所需的功率（向上取整到毫瓦）。
		secs := int64(dur / time.Second)
		need := Milliwatts((int64(remaining) + secs - 1) / secs)
		power := avail
		if power > need {
			power = need
		}
		a := Allocation{SlotStart: sl.Start, Start: start, End: end, Power: power}
		alloc = append(alloc, a)
		remaining -= a.Energy()
	}
	if remaining > 0 {
		return nil, capacityErrorf(
			"cannot deliver %d mJ within [%s, %s): short by %d mJ",
			req.MinEnergy, req.Arrival, req.Departure, remaining)
	}
	return alloc, nil
}

// withPlanLocked 在 fn 执行期间临时把 plan 纳入内存状态，fn 失败则
// 回滚，保证"先持久化后生效"且失败不留占用。
func (s *Service) withPlanLocked(plan *Plan, fn func() error) error {
	s.plans[plan.ID] = plan
	oldIdem, hadIdem := s.idem[plan.Request.ExternalID]
	if err := fn(); err != nil {
		delete(s.plans, plan.ID)
		if hadIdem {
			s.idem[plan.Request.ExternalID] = oldIdem
		} else {
			delete(s.idem, plan.Request.ExternalID)
		}
		return err
	}
	return nil
}

func (s *Service) persistLocked() error {
	plans := make([]*Plan, 0, len(s.plans))
	for _, p := range s.plans {
		plans = append(plans, p)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].ID < plans[j].ID })
	snap := snapshot{
		Slots:       append([]Slot(nil), s.slots...),
		Plans:       plans,
		Idempotency: s.idem,
		Counter:     s.counter,
	}
	if err := s.store.Save(snap); err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	return nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
