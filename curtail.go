package fleetcharging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"time"
)

// pendingCurtailment 是一份已生成、尚未确认的降容方案的服务端台账。
// 方案不持久化（进程重启即作废，需要时重新准备），但它持有生成时刻的
// 全站状态指纹，确认时逐笔比对预约版本，任一变化都整单拒绝。
type pendingCurtailment struct {
	plan        *CurtailmentPlan
	baseStation []Segment      // 生成时站点配置，确认时必须未被重配
	revisions   map[string]int // 生成时全部预约 request_id -> revision
	counter     int64          // 生成时 snapshot.Counter（捕获新增预约）
	planCount   int            // 生成时预约总数（捕获删除/取消）
}

// PrepareCurtailment 针对一次临时降容事件生成可确认的调整方案，但不改动任何状态。
//
// 调整规则：
//   - 事件半开时段 [Start, End) 内容量被覆盖为 PowerW，时段外配置不变；
//     事件窗口必须落在已配置供电时段内，且新功率必须严格低于原容量（否则不是“下降”）。
//   - 固定功率预约（Fixed）在事件时段内的分配功率一寸不让；固定预约之和就已超出
//     新容量时，无法形成方案，返回 KindCapacity。
//   - 可调预约按 Priority 从大到小依次重排（相同优先级按创建次序），高优先级先占用
//     事件时段内的剩余容量，低优先级被挤出事件时段后只能到窗口之外的空闲时段补能；
//     任何预约的调整后交付能量都不得低于其最低需求，否则整份方案无法生成（KindCapacity）。
//
// 方案中的每笔预约都记录生成时的版本号（Revision），确认时用作乐观锁。
func (s *Service) PrepareCurtailment(ctx context.Context, ev CapacityEvent) (*CurtailmentPlan, error) {
	if err := validateCapacityEvent(ev); err != nil {
		return nil, err
	}
	s.lock()
	defer s.unlock()
	now := s.now()
	if s.advanceLocked(now) {
		if err := s.persistLocked(ctx, "PrepareCurtailment:advance"); err != nil {
			return nil, err
		}
	}

	cfg, ok := s.snap.Stations[ev.StationID]
	if !ok {
		return nil, fail(KindParameter, "PrepareCurtailment", "站点 %q 未配置", ev.StationID)
	}
	if !ev.Start.After(now) {
		return nil, fail(KindTime, "PrepareCurtailment",
			"降容起始时间 %s 必须晚于当前时间 %s",
			ev.Start.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	}
	// 事件窗口内逐片检查：必须完全落在已配置时段，且每片原容量都严格高于新功率。
	curPieces := buildPieces(cfg.Segments, ev.Start, ev.End, nil, "")
	for _, p := range curPieces {
		if p.capacity <= 0 {
			return nil, fail(KindTime, "PrepareCurtailment",
				"降容时段 [%s,%s) 未完全落在站点 %s 的已配置供电时段内",
				ev.Start.Format(time.RFC3339Nano), ev.End.Format(time.RFC3339Nano), ev.StationID)
		}
		if p.capacity <= ev.PowerW {
			return nil, fail(KindParameter, "PrepareCurtailment",
				"[%s,%s) 原容量 %dW 不高于降容功率 %dW，不构成容量下降",
				p.start.Format(time.RFC3339Nano), p.end.Format(time.RFC3339Nano),
				p.capacity, ev.PowerW)
		}
	}

	newSegs := applyEventSegments(cfg.Segments, ev)

	// 确定性地枚举本站全部有效预约，并区分固定/可调/不受影响。
	all := make([]*Plan, 0, len(s.snap.Plans))
	for _, p := range s.snap.Plans {
		if p.StationID == ev.StationID && p.Status != StatusCancelled {
			all = append(all, p)
		}
	}
	sortPlansByID(all)

	var fixedAffected, flexAffected, others []*Plan
	for _, p := range all {
		if planTouchesWindow(p, ev.Start, ev.End) {
			if p.Fixed {
				fixedAffected = append(fixedAffected, p)
			} else {
				flexAffected = append(flexAffected, p)
			}
		} else {
			others = append(others, p)
		}
	}

	// 模拟环境：先放入所有不变的预约（含固定预约，保持原分配）。
	committed := append([]*Plan(nil), others...)
	committed = append(committed, fixedAffected...)

	// 固定预约不可削减：直接检验它们在新容量下是否放得下。
	{
		ps := buildPieces(newSegs, ev.Start, ev.End, committed, "")
		for _, p := range ps {
			if p.reserved > p.capacity {
				return nil, fail(KindCapacity, "PrepareCurtailment",
					"固定功率预约在 [%s,%s) 至少需要 %dW，高于降容后容量 %dW，无法调整",
					p.start.Format(time.RFC3339Nano), p.end.Format(time.RFC3339Nano),
					p.reserved, p.capacity)
			}
		}
	}

	// 可调预约按优先级降序逐个重排：高优先级先在新容量上挑时段，
	// 尚未处理的低优先级预约视为零占用（它们最后才领取剩余容量），
	// 因而最终方案的逐片容量可行是构造性的，不需要事后挤兑。
	sort.SliceStable(flexAffected, func(i, j int) bool {
		if flexAffected[i].Priority != flexAffected[j].Priority {
			return flexAffected[i].Priority > flexAffected[j].Priority
		}
		return flexAffected[i].ID < flexAffected[j].ID
	})

	items := make([]AdjustmentItem, 0, len(fixedAffected)+len(flexAffected))
	newAllocs := make(map[string][]SlotAllocation, len(flexAffected))
	for _, p := range fixedAffected {
		items = append(items, AdjustmentItem{
			RequestID:    p.RequestID,
			Priority:     p.Priority,
			Fixed:        true,
			FromRevision: p.Revision,
			Allocation:   append([]SlotAllocation(nil), p.Allocation...),
		})
	}

	for _, p := range flexAffected {
		// 在“已确定占用”（未变预约 + 固定预约 + 已处理高优先级预约）之外，
		// 于新容量上为当前预约重排完整窗口分配。
		ps := buildPieces(newSegs, p.Arrival, p.Departure, committed, p.ID)
		target := Mul128(p.MinEnergy, nanoWorkPerMicroWh)
		allocs, total := scheduleAllocation(ps, p.MaxPowerW, target)
		if Cmp128(total, target) < 0 {
			return nil, fail(KindCapacity, "PrepareCurtailment",
				"降容后预约 %q（优先级 %d）在场窗口 [%s,%s) 内最多只能获得 %d µWh，低于最低需求 %d µWh",
				p.RequestID, p.Priority,
				p.Arrival.Format(time.RFC3339Nano), p.Departure.Format(time.RFC3339Nano),
				floorMicroWh(total), p.MinEnergy)
		}
		allocs = append([]SlotAllocation(nil), allocs...)
		newAllocs[p.RequestID] = allocs
		committed = append(committed, &Plan{
			ID:         p.ID,
			RequestID:  p.RequestID,
			StationID:  p.StationID,
			Arrival:    p.Arrival,
			Departure:  p.Departure,
			MaxPowerW:  p.MaxPowerW,
			Allocation: allocs,
			Status:     StatusActive,
		})
	}

	// 防御性总校验：所有预约（新分配 + 未变预约）在新配置下逐片不超额。
	lo, hi := ev.Start, ev.End
	for _, p := range flexAffected {
		if p.Arrival.Before(lo) {
			lo = p.Arrival
		}
		if p.Departure.After(hi) {
			hi = p.Departure
		}
	}
	finalPieces := buildPieces(newSegs, lo, hi, committed, "")
	for _, p := range finalPieces {
		if p.reserved > p.capacity {
			return nil, fail(KindCapacity, "PrepareCurtailment",
				"内部校验失败：调整方案在 [%s,%s) 占用 %dW 超过容量 %dW",
				p.start.Format(time.RFC3339Nano), p.end.Format(time.RFC3339Nano),
				p.reserved, p.capacity)
		}
	}

	// 汇总输出（可调预约保持优先级降序，便于调用方审阅谁被削减）。
	for _, p := range flexAffected {
		allocs := newAllocs[p.RequestID]
		items = append(items, AdjustmentItem{
			RequestID:    p.RequestID,
			Priority:     p.Priority,
			Fixed:        p.Fixed,
			FromRevision: p.Revision,
			Allocation:   append([]SlotAllocation(nil), allocs...),
		})
	}

	token := newCurtailToken()
	plan := &CurtailmentPlan{
		Token:     token,
		Event:     ev,
		Segments:  append([]Segment(nil), newSegs...),
		Items:     items,
		CreatedAt: now,
	}
	s.pending[token] = &pendingCurtailment{
		plan:        plan,
		baseStation: append([]Segment(nil), cfg.Segments...),
		revisions:   snapshotRevisions(s.snap),
		counter:     s.snap.Counter,
		planCount:   len(s.snap.Plans),
	}
	return cloneCurtailmentPlan(plan), nil
}

// ConfirmCurtailment 确认一份待决方案：在同一次落库事务中替换事件时段容量并更新
// 方案中的全部预约（每笔版本号加一）。方案生成后任一预约被修改/取消、有新预约
// 提交、或站点配置被重配，都返回 KindConflict 且原安排保持不变——
// 不存在“部分预约已改、部分没改”的中间态。抵达锁定（active→arrived）不改版本号，
// 不属于冲突，车辆到场期间仍可确认。
// 返回每笔预约的版本与分配落实结果。
func (s *Service) ConfirmCurtailment(ctx context.Context, token string) ([]AdjustmentResult, error) {
	if token == "" {
		return nil, fail(KindParameter, "ConfirmCurtailment", "token 不能为空")
	}
	s.lock()
	defer s.unlock()
	now := s.now()
	if s.advanceLocked(now) {
		if err := s.persistLocked(ctx, "ConfirmCurtailment:advance"); err != nil {
			return nil, err
		}
	}

	pend, ok := s.pending[token]
	if !ok {
		return nil, fail(KindState, "ConfirmCurtailment",
			"调整方案 %q 不存在、已确认或已失效", token)
	}

	// 1) 站点配置指纹。
	curCfg, ok := s.snap.Stations[pend.plan.Event.StationID]
	if !ok || !segmentsEqual(curCfg.Segments, pend.baseStation) {
		delete(s.pending, token)
		return nil, fail(KindConflict, "ConfirmCurtailment",
			"站点 %s 配置在方案生成后已变化，方案 %q 失效",
			pend.plan.Event.StationID, token)
	}

	// 2) 全站预约指纹：数量、发号计数、逐笔版本。任何提交/修改/取消都会击穿。
	if s.snap.Counter != pend.counter || len(s.snap.Plans) != pend.planCount ||
		!revisionsEqual(s.snap, pend.revisions) {
		delete(s.pending, token)
		return nil, fail(KindConflict, "ConfirmCurtailment",
			"方案生成后预约集合已发生变化（提交/修改/取消），方案 %q 失效", token)
	}

	// 3) 逐笔明细校验：必须仍存在、仍为 active、版本一致。
	type target struct {
		plan *Plan
		item AdjustmentItem
	}
	targets := make([]target, 0, len(pend.plan.Items))
	for _, it := range pend.plan.Items {
		p, exists := s.snap.Plans[it.RequestID]
		if !exists {
			delete(s.pending, token)
			return nil, fail(KindConflict, "ConfirmCurtailment",
				"预约 %q 在方案生成后已不存在，方案 %q 失效", it.RequestID, token)
		}
		if p.Status == StatusCancelled {
			delete(s.pending, token)
			return nil, fail(KindConflict, "ConfirmCurtailment",
				"预约 %q 在方案生成后已取消，方案 %q 失效", it.RequestID, token)
		}
		if p.Revision != it.FromRevision {
			delete(s.pending, token)
			return nil, fail(KindConflict, "ConfirmCurtailment",
				"预约 %q 版本已从 %d 变为 %d，方案 %q 过期，请重新生成",
				it.RequestID, it.FromRevision, p.Revision, token)
		}
		targets = append(targets, target{plan: p, item: it})
	}

	// 4) 以当前真实状态做终态可行性复核（容量逐片守恒 + 每笔最低能量）。
	//    正常路径下指纹校验已足够，这一步是防止任何绕过版本路径的状态差异。
	simPlans := make([]*Plan, 0, len(s.snap.Plans))
	itemByID := map[string]AdjustmentItem{}
	for _, tg := range targets {
		itemByID[tg.plan.ID] = tg.item
	}
	for _, p := range s.snap.Plans {
		if p.StationID != pend.plan.Event.StationID || p.Status == StatusCancelled {
			continue
		}
		if it, hit := itemByID[p.ID]; hit {
			simPlans = append(simPlans, &Plan{
				ID: p.ID, Arrival: p.Arrival, Departure: p.Departure,
				Allocation: it.Allocation, Status: StatusActive,
			})
		} else {
			simPlans = append(simPlans, p)
		}
	}
	ev := pend.plan.Event
	lo, hi := ev.Start, ev.End
	for _, tg := range targets {
		if tg.plan.Arrival.Before(lo) {
			lo = tg.plan.Arrival
		}
		if tg.plan.Departure.After(hi) {
			hi = tg.plan.Departure
		}
	}
	for _, ps := range buildPieces(pend.plan.Segments, lo, hi, simPlans, "") {
		if ps.reserved > ps.capacity {
			delete(s.pending, token)
			return nil, fail(KindCapacity, "ConfirmCurtailment",
				"方案在 [%s,%s) 占用 %dW 超过降容后容量 %dW，拒绝确认",
				ps.start.Format(time.RFC3339Nano), ps.end.Format(time.RFC3339Nano),
				ps.reserved, ps.capacity)
		}
	}
	for _, tg := range targets {
		work := Zero128()
		for _, a := range tg.item.Allocation {
			work = Add128(work, Mul128(a.PowerW, a.End.Sub(a.Start).Nanoseconds()))
		}
		need := Mul128(tg.plan.MinEnergy, nanoWorkPerMicroWh)
		if Cmp128(work, need) < 0 {
			delete(s.pending, token)
			return nil, fail(KindCapacity, "ConfirmCurtailment",
				"方案为预约 %q 安排的电量低于最低需求，拒绝确认", tg.plan.RequestID)
		}
	}

	// 5) 事务体：先在内存中改写全部对象，随后一次 Save 原子落库；
	//    落库失败则逐笔回滚，内存态与持久化态保持一致。
	type undo struct {
		p          *Plan
		oldAlloc   []SlotAllocation
		oldRev     int
		oldUpdated time.Time
	}
	undos := make([]undo, 0, len(targets))
	for _, tg := range targets {
		p := tg.plan
		undos = append(undos, undo{
			p: p, oldAlloc: p.Allocation, oldRev: p.Revision, oldUpdated: p.UpdatedAt,
		})
		p.Allocation = append([]SlotAllocation(nil), tg.item.Allocation...)
		p.Revision++
		p.UpdatedAt = now
	}
	oldSegs := curCfg.Segments
	s.snap.Stations[ev.StationID] = StationConfig{
		StationID: ev.StationID,
		Segments:  append([]Segment(nil), pend.plan.Segments...),
	}
	delete(s.pending, token)

	if err := s.persistLocked(ctx, "ConfirmCurtailment"); err != nil {
		for _, u := range undos {
			u.p.Allocation = u.oldAlloc
			u.p.Revision = u.oldRev
			u.p.UpdatedAt = u.oldUpdated
		}
		s.snap.Stations[ev.StationID] = StationConfig{StationID: ev.StationID, Segments: oldSegs}
		s.pending[token] = pend
		return nil, err
	}

	results := make([]AdjustmentResult, 0, len(targets))
	for _, tg := range targets {
		results = append(results, AdjustmentResult{
			RequestID:    tg.plan.RequestID,
			FromRevision: tg.item.FromRevision,
			ToRevision:   tg.plan.Revision,
			Allocation:   append([]SlotAllocation(nil), tg.plan.Allocation...),
		})
	}
	return results, nil
}

// ---- 辅助 ----

// applyEventSegments 在配置时段轴上覆盖事件窗口：窗口内统一为事件功率，
// 窗口外原时段（含功率与空隙）保持不变；相接且同功率的时段合并，保持紧凑。
func applyEventSegments(segs []Segment, ev CapacityEvent) []Segment {
	out := make([]Segment, 0, len(segs)+1)
	for _, sg := range segs {
		// 窗口左侧残余 [sg.Start, min(sg.End, ev.Start))
		if sg.Start.Before(ev.Start) {
			end := sg.End
			if ev.Start.Before(end) {
				end = ev.Start
			}
			if sg.Start.Before(end) {
				out = append(out, Segment{Start: sg.Start, End: end, PowerW: sg.PowerW})
			}
		}
		// 窗口右侧残余 [max(sg.Start, ev.End), sg.End)
		if sg.End.After(ev.End) {
			start := sg.Start
			if ev.End.After(start) {
				start = ev.End
			}
			if start.Before(sg.End) {
				out = append(out, Segment{Start: start, End: sg.End, PowerW: sg.PowerW})
			}
		}
	}
	out = append(out, Segment{Start: ev.Start, End: ev.End, PowerW: ev.PowerW})
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })

	merged := out[:0]
	for _, sg := range out {
		n := len(merged)
		if n > 0 && merged[n-1].End.After(sg.Start) {
			// 真正重叠（半开区间端点相接不算重叠，不应发生在正常输入上）：
			// 防御性处理，保留更长的右端点。
			if sg.End.After(merged[n-1].End) {
				merged[n-1].End = sg.End
			}
			continue
		}
		if n > 0 && merged[n-1].End.Equal(sg.Start) &&
			merged[n-1].PowerW == sg.PowerW {
			merged[n-1].End = sg.End
			continue
		}
		merged = append(merged, sg)
	}
	return merged
}

// planTouchesWindow 报告预约是否在事件半开窗口内持有正功率分配。
func planTouchesWindow(p *Plan, from, to time.Time) bool {
	if !intervalOverlaps(p.Arrival, p.Departure, from, to) {
		return false
	}
	for i := range p.Allocation {
		a := &p.Allocation[i]
		if a.PowerW > 0 && intervalOverlaps(a.Start, a.End, from, to) {
			return true
		}
	}
	return false
}

func sortPlansByID(plans []*Plan) {
	sort.Slice(plans, func(i, j int) bool { return plans[i].ID < plans[j].ID })
}

// floorMicroWh 把 W·ns 功量换算为 µWh 并向下取整（仅用于错误信息）。
func floorMicroWh(work Int128) int64 {
	q, _ := QuoRem128(work, nanoWorkPerMicroWh)
	if !q.IsInt64() {
		return 1<<63 - 1
	}
	return int64(q.lo)
}

func snapshotRevisions(snap *snapshot) map[string]int {
	m := make(map[string]int, len(snap.Plans))
	for id, p := range snap.Plans {
		m[id] = p.Revision
	}
	return m
}

func revisionsEqual(snap *snapshot, want map[string]int) bool {
	if len(snap.Plans) != len(want) {
		return false
	}
	for id, rev := range want {
		p, ok := snap.Plans[id]
		if !ok || p.Revision != rev {
			return false
		}
	}
	return true
}

func segmentsEqual(a, b []Segment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Start.Equal(b[i].Start) || !a[i].End.Equal(b[i].End) || a[i].PowerW != b[i].PowerW {
			return false
		}
	}
	return true
}

func newCurtailToken() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "curtail-" + hex.EncodeToString(b)
}

func cloneCurtailmentPlan(p *CurtailmentPlan) *CurtailmentPlan {
	cp := *p
	cp.Segments = append([]Segment(nil), p.Segments...)
	cp.Items = make([]AdjustmentItem, len(p.Items))
	for i, it := range p.Items {
		it.Allocation = append([]SlotAllocation(nil), it.Allocation...)
		cp.Items[i] = it
	}
	return &cp
}

func validateCapacityEvent(ev CapacityEvent) error {
	const op = "PrepareCurtailment"
	if ev.EventID == "" {
		return fail(KindParameter, op, "event_id 不能为空")
	}
	if ev.StationID == "" {
		return fail(KindParameter, op, "station_id 不能为空")
	}
	if !ev.Start.Before(ev.End) {
		return fail(KindTime, op, "降容时段起点必须早于终点")
	}
	if ev.PowerW < 0 {
		return fail(KindParameter, op, "降容功率不能为负")
	}
	return nil
}
