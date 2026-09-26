package fleetcharging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Clock 返回“当前时间”，默认为 time.Now；测试可注入以精确控制抵达边界。
type Clock func() time.Time

// Service 是充电容量预订服务。
// 所有公开方法都在同一把互斥锁内完成“检查 + 落库”，
// 因此并发提交/修改/取消之间天然串行化，不会出现部分占用或重复释放。
type Service struct {
	mu    sync.Mutex
	store Store
	now   Clock
	snap  *snapshot
}

func (s *Service) lock()   { s.mu.Lock() }
func (s *Service) unlock() { s.mu.Unlock() }

// Option 配置 Service。
type Option func(*Service)

// WithClock 注入时钟（主要用于测试抵达时间边界）。
func WithClock(c Clock) Option {
	return func(s *Service) { s.now = c }
}

// NewService 从 store 装载持久化数据并返回服务。store 中数据损坏时返回错误。
func NewService(store Store, opts ...Option) (*Service, error) {
	s := &Service{store: store, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	snap, err := store.Load(context.Background())
	if err != nil {
		return nil, fmt.Errorf("NewService: load persisted state: %w", err)
	}
	if snap == nil {
		snap = newSnapshot()
	}
	s.snap = snap
	return s, nil
}

// ConfigureStation 创建或整体替换站点的时段功率配置。
// 若新配置会使任何既有有效计划超出容量，返回 KindCapacity 且旧配置保持不变。
func (s *Service) ConfigureStation(ctx context.Context, cfg StationConfig) error {
	if err := validateStationConfig(cfg); err != nil {
		return err
	}
	s.lock()
	defer s.unlock()
	now := s.now()
	if s.advanceLocked(now) {
		if err := s.persistLocked(ctx, "ConfigureStation:advance"); err != nil {
			return err
		}
	}

	segs := append([]Segment(nil), cfg.Segments...)
	if err := checkPlansFitConfig(cfg.StationID, segs, s.snap); err != nil {
		return err
	}
	s.snap.Stations[cfg.StationID] = StationConfig{StationID: cfg.StationID, Segments: segs}
	return s.persistLocked(ctx, "ConfigureStation")
}

// SubmitPlan 提交一个新充电计划。
//   - 同号同内容：返回原计划（无论其当前处于 active/cancelled/arrived）。
//   - 同号不同内容：返回 KindIdempotency。
//   - 容量或时间窗口不可行：整体拒绝，不产生任何占用（KindCapacity/KingTime）。
func (s *Service) SubmitPlan(ctx context.Context, req ChargeRequest) (*Plan, error) {
	if err := validateChargeRequest(req); err != nil {
		return nil, err
	}
	s.lock()
	defer s.unlock()
	now := s.now()
	if s.advanceLocked(now) {
		if err := s.persistLocked(ctx, "SubmitPlan:advance"); err != nil {
			return nil, err
		}
	}

	if existing, ok := s.snap.Plans[req.RequestID]; ok {
		if !sameRequestContent(existing, req) {
			return nil, fail(KindIdempotency, "SubmitPlan",
				"request_id %q 已用于内容不同的请求", req.RequestID)
		}
		cp := *existing
		return &cp, nil
	}

	if err := s.checkWindowLocked(req.StationID, req.Arrival, req.Departure); err != nil {
		return nil, err
	}
	if !req.Arrival.After(now) {
		return nil, fail(KindTime, "SubmitPlan",
			"到达时间 %s 必须晚于当前时间 %s", req.Arrival.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	}

	plan, err := s.buildPlanLocked(req, now)
	if err != nil {
		return nil, err
	}
	s.snap.Plans[req.RequestID] = plan
	if err := s.persistLocked(ctx, "SubmitPlan"); err != nil {
		delete(s.snap.Plans, req.RequestID)
		return nil, err
	}
	cp := *plan
	return &cp, nil
}

// PlanModification 是对既有计划的修改内容（不含外部请求号，号保持不变）。
type PlanModification struct {
	StationID    string
	VehicleID    string
	Arrival      time.Time
	Departure    time.Time
	MinEnergyUWh int64
	MaxPowerW    int64
}

// ModifyPlan 在车辆抵达前原子修改计划：旧占用与新占用在同一次落库中替换，
// 容量不可行或车辆已抵达都会整体失败，旧计划保持不变。
// requestID 为提交时使用的外部请求号。
func (s *Service) ModifyPlan(ctx context.Context, requestID string, mod PlanModification) (*Plan, error) {
	newReq := ChargeRequest{
		RequestID:    requestID,
		StationID:    mod.StationID,
		VehicleID:    mod.VehicleID,
		Arrival:      mod.Arrival,
		Departure:    mod.Departure,
		MinEnergyUWh: mod.MinEnergyUWh,
		MaxPowerW:    mod.MaxPowerW,
	}
	if err := validateChargeRequest(newReq); err != nil {
		return nil, err
	}
	s.lock()
	defer s.unlock()
	now := s.now()
	if s.advanceLocked(now) {
		if err := s.persistLocked(ctx, "ModifyPlan:advance"); err != nil {
			return nil, err
		}
	}

	plan, ok := s.snap.Plans[requestID]
	if !ok {
		return nil, fail(KindState, "ModifyPlan", "request_id %q 对应的计划不存在", requestID)
	}
	if plan.Status == StatusCancelled {
		return nil, fail(KindState, "ModifyPlan", "计划 %q 已取消，不能修改", requestID)
	}
	if !plan.Arrival.After(now) {
		return nil, fail(KindState, "ModifyPlan",
			"车辆已在 %s 抵达，计划 %q 已锁定", plan.Arrival.Format(time.RFC3339Nano), requestID)
	}

	if err := s.checkWindowLocked(newReq.StationID, newReq.Arrival, newReq.Departure); err != nil {
		return nil, err
	}
	if !newReq.Arrival.After(now) {
		return nil, fail(KindTime, "ModifyPlan",
			"新到达时间 %s 必须晚于当前时间 %s", newReq.Arrival.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	}

	// 先在副本上计算新分配；失败时原计划原封不动。
	candidate := *plan
	candidate.StationID = newReq.StationID
	candidate.VehicleID = newReq.VehicleID
	candidate.Arrival = newReq.Arrival
	candidate.Departure = newReq.Departure
	candidate.MinEnergy = newReq.MinEnergyUWh
	candidate.MaxPowerW = newReq.MaxPowerW

	allocs, err := s.computeAllocationLocked(&candidate, now)
	if err != nil {
		return nil, err
	}

	old := *plan
	plan.StationID = mod.StationID
	plan.VehicleID = mod.VehicleID
	plan.Arrival = mod.Arrival
	plan.Departure = mod.Departure
	plan.MinEnergy = mod.MinEnergyUWh
	plan.MaxPowerW = mod.MaxPowerW
	plan.Allocation = allocs
	plan.Revision++
	plan.UpdatedAt = now

	if err := s.persistLocked(ctx, "ModifyPlan"); err != nil {
		*plan = old // 回滚内存态
		return nil, err
	}
	cp := *plan
	return &cp, nil
}

// CancelPlan 在车辆抵达前取消计划并释放其全部占用。
// 计划不存在、已取消或车辆已抵达分别返回 KindState。
func (s *Service) CancelPlan(ctx context.Context, requestID string) error {
	if requestID == "" {
		return fail(KindParameter, "CancelPlan", "request_id 不能为空")
	}
	s.lock()
	defer s.unlock()
	now := s.now()
	if s.advanceLocked(now) {
		if err := s.persistLocked(ctx, "CancelPlan:advance"); err != nil {
			return err
		}
	}

	plan, ok := s.snap.Plans[requestID]
	if !ok {
		return fail(KindState, "CancelPlan", "request_id %q 对应的计划不存在", requestID)
	}
	switch plan.Status {
	case StatusCancelled:
		return fail(KindState, "CancelPlan", "计划 %q 已取消", requestID)
	case StatusArrived:
		return fail(KindState, "CancelPlan", "车辆已抵达，计划 %q 已锁定", requestID)
	}
	old := *plan
	plan.Status = StatusCancelled
	plan.UpdatedAt = now
	plan.Revision++
	if err := s.persistLocked(ctx, "CancelPlan"); err != nil {
		*plan = old
		return err
	}
	return nil
}

// GetPlan 按外部请求号查询计划；不存在返回 KindState。
func (s *Service) GetPlan(ctx context.Context, requestID string) (*Plan, error) {
	if requestID == "" {
		return nil, fail(KindParameter, "GetPlan", "request_id 不能为空")
	}
	s.lock()
	defer s.unlock()
	if s.advanceLocked(s.now()) {
		if err := s.persistLocked(ctx, "GetPlan:advance"); err != nil {
			return nil, err
		}
	}
	plan, ok := s.snap.Plans[requestID]
	if !ok {
		return nil, fail(KindState, "GetPlan", "request_id %q 对应的计划不存在", requestID)
	}
	cp := *plan
	return &cp, nil
}

// Occupancy 返回 [from,to) 内的逐时段占用：站点容量、已占用功率、剩余功率。
// 未被配置覆盖的时间容量按 0 返回。状态迁移（抵达锁定）也会在查询时顺带落库。
func (s *Service) Occupancy(ctx context.Context, stationID string, from, to time.Time) ([]SlotOccupancy, error) {
	if stationID == "" {
		return nil, fail(KindParameter, "Occupancy", "station_id 不能为空")
	}
	if !from.Before(to) {
		return nil, fail(KindParameter, "Occupancy", "查询区间起点必须早于终点")
	}
	s.lock()
	defer s.unlock()
	now := s.now()
	changed := s.advanceLocked(now)

	cfg, ok := s.snap.Stations[stationID]
	if !ok {
		return nil, fail(KindParameter, "Occupancy", "站点 %q 未配置", stationID)
	}
	plans := make([]*Plan, 0, len(s.snap.Plans))
	for _, p := range s.snap.Plans {
		if p.StationID == stationID {
			plans = append(plans, p)
		}
	}
	ps := buildPieces(cfg.Segments, from, to, plans, "")
	out := make([]SlotOccupancy, 0, len(ps))
	for _, p := range ps {
		out = append(out, SlotOccupancy{
			Start:      p.start,
			End:        p.end,
			CapacityW:  p.capacity,
			ReservedW:  p.reserved,
			AvailableW: p.capacity - p.reserved,
		})
	}
	if changed {
		if err := s.persistLocked(ctx, "Occupancy"); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- 内部辅助 ----

// advanceLocked 把所有到达时间已到的 active 计划迁移到 arrived（占用保持不变）。
// 状态迁移与修改/取消在同一把锁下竞争，因此“到达点”只会被越过一次，
// 取消不可能发生在抵达之后，容量不会泄漏也不会被重复释放。
func (s *Service) advanceLocked(now time.Time) bool {
	changed := false
	for _, p := range s.snap.Plans {
		if p.Status == StatusActive && !p.Arrival.After(now) {
			p.Status = StatusArrived
			p.UpdatedAt = now
			changed = true
		}
	}
	return changed
}

func (s *Service) checkWindowLocked(stationID string, from, to time.Time) error {
	cfg, ok := s.snap.Stations[stationID]
	if !ok {
		return fail(KindParameter, "checkWindow", "站点 %q 未配置", stationID)
	}
	ps := buildPieces(cfg.Segments, from, to, nil, "")
	if !fullyConfigured(ps) {
		return fail(KindTime, "checkWindow",
			"区间 [%s, %s) 未完全落在站点 %s 的已配置供电时段内",
			from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), stationID)
	}
	return nil
}

// buildPlanLocked 校验容量可行性并构造尚未入库的计划。
func (s *Service) buildPlanLocked(req ChargeRequest, now time.Time) (*Plan, error) {
	plan := &Plan{
		ID:        s.nextIDLocked(),
		RequestID: req.RequestID,
		StationID: req.StationID,
		VehicleID: req.VehicleID,
		Arrival:   req.Arrival,
		Departure: req.Departure,
		MinEnergy: req.MinEnergyUWh,
		MaxPowerW: req.MaxPowerW,
		Status:    StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
		Revision:  1,
	}
	allocs, err := s.computeAllocationLocked(plan, now)
	if err != nil {
		return nil, err
	}
	plan.Allocation = allocs
	return plan, nil
}

// computeAllocationLocked 在考虑全部其他有效计划占用的前提下贪心分配功率，
// 能量不足返回 KindCapacity，且不会修改任何既有状态。
func (s *Service) computeAllocationLocked(plan *Plan, _ time.Time) ([]SlotAllocation, error) {
	cfg := s.snap.Stations[plan.StationID]
	others := make([]*Plan, 0, len(s.snap.Plans))
	for _, p := range s.snap.Plans {
		if p.RequestID == plan.RequestID || p.Status == StatusCancelled {
			continue
		}
		others = append(others, p)
	}
	ps := buildPieces(cfg.Segments, plan.Arrival, plan.Departure, others, "")
	target := Mul128(plan.MinEnergy, nanoWorkPerMicroWh)
	allocs, total := scheduleAllocation(ps, plan.MaxPowerW, target)
	if Cmp128(total, target) < 0 {
		return nil, fail(KindCapacity, "computeAllocation",
			"站点 %s 在 [%s,%s) 的剩余容量无法提供最低所需电量 %d µWh",
			plan.StationID, plan.Arrival.Format(time.RFC3339Nano),
			plan.Departure.Format(time.RFC3339Nano), plan.MinEnergy)
	}
	return allocs, nil
}

func (s *Service) nextIDLocked() string {
	s.snap.Counter++
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "plan-" + itoaPad(s.snap.Counter) + "-" + hex.EncodeToString(b)
}

func (s *Service) persistLocked(ctx context.Context, op string) error {
	if err := s.store.Save(ctx, s.snap); err != nil {
		return fmt.Errorf("%s: persist state: %w", op, err)
	}
	return nil
}

// sameRequestContent 比较幂等键之外的请求内容是否完全一致。
func sameRequestContent(p *Plan, req ChargeRequest) bool {
	return p.StationID == req.StationID &&
		p.VehicleID == req.VehicleID &&
		p.Arrival.Equal(req.Arrival) &&
		p.Departure.Equal(req.Departure) &&
		p.MinEnergy == req.MinEnergyUWh &&
		p.MaxPowerW == req.MaxPowerW
}

// checkPlansFitConfig 判断新配置下所有有效计划是否仍满足逐片容量约束。
func checkPlansFitConfig(stationID string, segs []Segment, snap *snapshot) error {
	var lo, hi time.Time
	plans := make([]*Plan, 0)
	first := true
	for _, p := range snap.Plans {
		if p.StationID != stationID || p.Status == StatusCancelled {
			continue
		}
		for i := range p.Allocation {
			a := &p.Allocation[i]
			if a.PowerW <= 0 {
				continue
			}
			if first || a.Start.Before(lo) {
				lo = a.Start
			}
			if first || a.End.After(hi) {
				hi = a.End
			}
			first = false
		}
		plans = append(plans, p)
	}
	if first {
		return nil
	}
	ps := buildPieces(segs, lo, hi, plans, "")
	for _, p := range ps {
		if p.reserved > p.capacity {
			return fail(KindCapacity, "ConfigureStation",
				"新配置在 [%s,%s) 容量 %dW 低于既有占用 %dW",
				p.start.Format(time.RFC3339Nano), p.end.Format(time.RFC3339Nano),
				p.capacity, p.reserved)
		}
	}
	return nil
}

// ---- 参数校验 ----

func validateStationConfig(cfg StationConfig) error {
	const op = "ConfigureStation"
	if cfg.StationID == "" {
		return fail(KindParameter, op, "station_id 不能为空")
	}
	if len(cfg.Segments) == 0 {
		return fail(KindParameter, op, "时段配置不能为空")
	}
	for i := range cfg.Segments {
		s := &cfg.Segments[i]
		if !s.Start.Before(s.End) {
			return fail(KindTime, op, "时段 %d 起点必须早于终点", i)
		}
		if s.PowerW < 0 {
			return fail(KindParameter, op, "时段 %d 功率不能为负", i)
		}
		if i > 0 {
			prev := &cfg.Segments[i-1]
			if s.Start.Before(prev.End) {
				return fail(KindParameter, op, "时段 %d 与前一时段重叠", i)
			}
			if s.Start.Before(prev.Start) {
				return fail(KindParameter, op, "时段必须按起点升序排列")
			}
		}
	}
	return nil
}

func validateChargeRequest(req ChargeRequest) error {
	const op = "SubmitPlan"
	if req.RequestID == "" {
		return fail(KindParameter, op, "request_id 不能为空")
	}
	if req.StationID == "" {
		return fail(KindParameter, op, "station_id 不能为空")
	}
	if req.VehicleID == "" {
		return fail(KindParameter, op, "vehicle_id 不能为空")
	}
	if req.MinEnergyUWh <= 0 {
		return fail(KindParameter, op, "min_energy_uwh 必须为正数")
	}
	if req.MaxPowerW <= 0 {
		return fail(KindParameter, op, "max_power_w 必须为正数")
	}
	if req.Arrival.IsZero() || req.Departure.IsZero() {
		return fail(KindParameter, op, "arrival/departure 不能为空")
	}
	if !req.Arrival.Before(req.Departure) {
		return fail(KindTime, op, "到达时间必须早于离开时间")
	}
	return nil
}

func itoaPad(n int64) string {
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
