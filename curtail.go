package fleetcharging

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// curtailPiece 是容量削减重分配使用的最小时间片：
// 范围内的容量、受保护占用、可调整占用三者均为常数。
type curtailPiece struct {
	start     time.Time
	end       time.Time
	capacity  int64 // 降容后该片容量（W）
	protected int64 // 不可移动（固定/已抵达/第三方）占用（W）
	reserved  int64 // 可调整预约的原占用（W）
	inWindow  bool  // 是否落在容量事件窗口内
}

// adjMeta 是一笔可调整预约在重分配期间的工作数据。
type adjMeta struct {
	plan        *Plan
	required    Int128  // 全程必须交付的最低功量
	origByPiece []int64 // 每个时间片上的原占用功率
}

// buildCurtailPieces 在重排范围 [from,to) 内按“降容时段边界 ∪ 在场/分配边界”
// 切分时间片，并分别汇总受保护预约与可调整预约的原占用。
// winFrom/winTo 标出容量事件窗口：落在窗口内的时间片 inWindow=true。
func buildCurtailPieces(segs []Segment, from, to, winFrom, winTo time.Time, protected, adjustable []*Plan) []curtailPiece {
	times := []time.Time{from, to}
	addBound := func(plans []*Plan) {
		for _, p := range plans {
			if p.Arrival.After(from) && p.Arrival.Before(to) {
				times = append(times, p.Arrival)
			}

			if p.Departure.After(from) && p.Departure.Before(to) {
				times = append(times, p.Departure)
			}
			for i := range p.Allocation {
				a := &p.Allocation[i]
				if a.Start.After(from) && a.Start.Before(to) {
					times = append(times, a.Start)
				}
				if a.End.After(from) && a.End.Before(to) {
					times = append(times, a.End)
				}
			}
		}
	}
	for _, sg := range segs {
		if sg.Start.After(from) && sg.Start.Before(to) {
			times = append(times, sg.Start)
		}
		if sg.End.After(from) && sg.End.Before(to) {
			times = append(times, sg.End)
		}
	}
	addBound(protected)
	addBound(adjustable)

	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	uniq := times[:0]
	for _, t := range times {
		if len(uniq) == 0 || !uniq[len(uniq)-1].Equal(t) {
			uniq = append(uniq, t)
		}
	}

	pieces := make([]curtailPiece, 0, len(uniq)-1)
	for i := 0; i+1 < len(uniq); i++ {
		s, e := uniq[i], uniq[i+1]
		pieces = append(pieces, curtailPiece{
			start:    s,
			end:      e,
			inWindow: (!s.Before(winFrom)) && (!e.After(winTo)),
		})
	}

	powerOn := func(t time.Time) int64 {
		for i := range segs {
			s := &segs[i]
			if (!t.Before(s.Start)) && t.Before(s.End) {
				return s.PowerW
			}
		}
		return 0
	}
	sumPower := func(plans []*Plan, ps, pe time.Time) int64 {
		var sum int64
		for _, p := range plans {
			if !intervalOverlaps(p.Arrival, p.Departure, ps, pe) {
				continue
			}
			for i := range p.Allocation {
				a := &p.Allocation[i]
				if (!ps.Before(a.Start)) && ps.Before(a.End) {
					sum += a.PowerW
					break
				}
			}
		}
		return sum
	}
	for i := range pieces {
		t := pieces[i].start
		pieces[i].capacity = powerOn(t)
		pieces[i].protected = sumPower(protected, t, pieces[i].end)
		pieces[i].reserved = sumPower(adjustable, t, pieces[i].end)
	}
	return pieces
}

// reallocateCurtailment 在降容后的时间片上重算可调整预约的功率分配。
// 时间片范围覆盖全部可调整预约在场区间的并集：受影响时段内容量被削减，
// 时段之外容量不变，但可调整预约可以把被挤出的功率转移到其在场期内
// 真正空闲的时间片上——只要全程交付仍达到各自最低电量。
//
// 两步：
//  1. 优先保留：按优先级从高到低（并列时最早离开、再以 request_id）尽量保留
//     每笔预约的原分配；时间片放不下的部分被“挤出”。受保护占用先行扣除，
//     因此高优先级预约几乎不动，低优先级预约承担位移。事件窗口之外原本就放得下，
//     位移只会发生在降容时段内。
//  2. 缺额回补（EDF）：按最早离开优先，把每笔被挤出预约的缺额贪心地补到
//     其在场期内任意空闲时间片（优先更早的片，含窗口内剩余空隙）；
//     任一笔补不足即返回 false（整份方案不可行）。
//
// 全程每片总占用不超过容量，每笔预约最终功量恰为其最低所需（不多给）。
// 返回 requestID -> 新分配（覆盖该预约完整在场期，已合并相邻同功率段）。
func reallocateCurtailment(ps []curtailPiece, metas []*adjMeta) (map[string][]SlotAllocation, bool) {
	n := len(metas)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	present := func(m *adjMeta, j int) bool {
		return intervalOverlaps(m.plan.Arrival, m.plan.Departure, ps[j].start, ps[j].end)
	}

	used := make([]int64, len(ps)) // 每片已占用总量（受保护 + 已保留/回补）
	for j := range ps {
		used[j] = ps[j].protected
	}
	usedAdj := make([]int64, len(ps)) // 每片可调整预约已占用量（单独记账，用于窗口守恒）
	got := make([][]int64, n)         // 每笔预约在每片上的新功率
	for i := range got {
		got[i] = make([]int64, len(ps))
	}

	// 第一步：按优先级保留原分配。
	keepOrder := append([]int(nil), idx...)
	sort.SliceStable(keepOrder, func(a, b int) bool {
		x, y := metas[keepOrder[a]], metas[keepOrder[b]]
		if x.plan.Priority != y.plan.Priority {
			return x.plan.Priority > y.plan.Priority
		}
		if !x.plan.Departure.Equal(y.plan.Departure) {
			return x.plan.Departure.Before(y.plan.Departure)
		}
		return x.plan.RequestID < y.plan.RequestID
	})
	for _, k := range keepOrder {
		m := metas[k]
		for j := range ps {
			if !present(m, j) || m.origByPiece[j] <= 0 {
				continue
			}
			avail := ps[j].capacity - used[j]
			keep := m.origByPiece[j]
			if keep > avail {
				keep = avail // 放不下的部分被挤出，留给第二步回补
			}
			if keep < 0 {
				keep = 0
			}
			got[k][j] = keep
			used[j] += keep
			usedAdj[j] += keep
		}
	}

	// 第二步：EDF 回补缺额到任意空闲片。
	edf := append([]int(nil), idx...)
	sort.SliceStable(edf, func(a, b int) bool {
		x, y := metas[edf[a]], metas[edf[b]]
		if !x.plan.Departure.Equal(y.plan.Departure) {
			return x.plan.Departure.Before(y.plan.Departure)
		}
		return x.plan.RequestID < y.plan.RequestID
	})
	curWork := func(k int) Int128 {
		w := Zero128()
		for j := range ps {
			if got[k][j] > 0 {
				w = Add128(w, Mul128(got[k][j], ps[j].end.Sub(ps[j].start).Nanoseconds()))
			}
		}
		return w
	}
	for _, k := range edf {
		m := metas[k]
		acc := curWork(k)
		for j := range ps {
			if Cmp128(acc, m.required) >= 0 {
				break
			}
			if !present(m, j) {
				continue
			}
			free := ps[j].capacity - used[j]
			// 守恒：事件窗口内可调整预约的总占用不得超过其原占用，
			// 被释放的窗口容量不能被其他预约重新占回。
			if ps[j].inWindow {
				if cap := ps[j].reserved - usedAdj[j]; free > cap {
					free = cap
				}
			}
			if free <= 0 {
				continue
			}
			power := free
			if cap := m.plan.MaxPowerW - got[k][j]; power > cap {
				power = cap
			}
			if power <= 0 {
				continue
			}
			dur := ps[j].end.Sub(ps[j].start).Nanoseconds()
			if Cmp128(Add128(acc, Mul128(power, dur)), m.required) >= 0 {
				need := CeilDiv128(Sub128(m.required, acc), dur)
				if need < 1 {
					need = 1
				}
				if need > power {
					need = power
				}
				got[k][j] += need
				used[j] += need
				usedAdj[j] += need
				acc = Add128(acc, Mul128(need, dur))
				break
			}
			got[k][j] += power
			used[j] += power
			usedAdj[j] += power
			acc = Add128(acc, Mul128(power, dur))
		}
		if Cmp128(acc, m.required) < 0 {
			return nil, false
		}
	}

	out := make(map[string][]SlotAllocation, n)
	for k, m := range metas {
		allocs := make([]SlotAllocation, 0, len(ps))
		for j := range ps {
			if got[k][j] > 0 {
				allocs = append(allocs, SlotAllocation{
					Start: ps[j].start, End: ps[j].end, PowerW: got[k][j],
				})
			}
		}
		out[m.plan.RequestID] = mergeAdjacent(allocs)
	}
	return out, true
}

// applyEventToSegments 把容量事件叠加到站点时段配置上：
// 窗口 [ev.Start,ev.End) 内的容量整体替换为 ev.PowerW（包括覆盖原配置空隙），
// 窗口之外的原时段保持不变；与窗口重叠的原时段在窗口边界处被切开。
func applyEventToSegments(segs []Segment, ev CurtailmentEvent) []Segment {
	times := []time.Time{ev.Start, ev.End}
	for _, sg := range segs {
		times = append(times, sg.Start, sg.End)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	uniq := times[:0]
	for _, t := range times {
		if len(uniq) == 0 || !uniq[len(uniq)-1].Equal(t) {
			uniq = append(uniq, t)
		}
	}
	out := make([]Segment, 0, len(uniq)-1)
	for i := 0; i+1 < len(uniq); i++ {
		s, e := uniq[i], uniq[i+1]
		if !s.Before(e) {
			continue
		}
		// 与窗口有正长度相交的基本区间一律采用事件功率。
		if intervalOverlaps(s, e, ev.Start, ev.End) {
			out = append(out, Segment{Start: s, End: e, PowerW: ev.PowerW})
			continue
		}
		for _, sg := range segs {
			if (!s.Before(sg.Start)) && s.Before(sg.End) {
				out = append(out, Segment{Start: s, End: e, PowerW: sg.PowerW})
				break
			}
		}
	}
	return out
}

// hashStationConfig 计算站点配置的稳定指纹，用于确认时发现“方案生成后配置被改过”。
func hashStationConfig(cfg StationConfig) string {
	h := sha256.New()
	fmt.Fprintf(h, "station=%s\n", cfg.StationID)
	for _, s := range cfg.Segments {
		fmt.Fprintf(h, "%s|%s|%d\n",
			s.Start.UTC().Format(time.RFC3339Nano),
			s.End.UTC().Format(time.RFC3339Nano), s.PowerW)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// verifyCurtailment 在确认前对整份调整做最终、独立的校验：
//  1. 容量：调整后每个最细时间片上的总占用 ≤ 降容后容量；
//  2. 守恒：事件窗口内每个时间片上的总占用只减不增（释放的窗口容量不被重新占满）；
//  3. 最低需求：每笔可调整预约调整后的全程交付功量 ≥ 其最低电量。
//
// curPlans 给出预约当前元数据，oldAlloc/newAlloc 分别为调整前/后的完整分配。
func verifyCurtailment(
	oldSegs, newSegs []Segment,
	ev CurtailmentEvent,
	scopeLo, scopeHi time.Time,
	curPlans map[string]*Plan,
	oldAlloc, newAlloc map[string][]SlotAllocation,
) error {
	const op = "ConfirmCurtailment"
	times := []time.Time{scopeLo, scopeHi, ev.Start, ev.End}
	addSegBounds := func(segs []Segment) {
		for _, sg := range segs {
			if sg.Start.After(scopeLo) && sg.Start.Before(scopeHi) {
				times = append(times, sg.Start)
			}
			if sg.End.After(scopeLo) && sg.End.Before(scopeHi) {
				times = append(times, sg.End)
			}
		}
	}
	addAllocBounds := func(allocs map[string][]SlotAllocation) {
		for _, as := range allocs {
			for _, a := range as {
				if a.Start.After(scopeLo) && a.Start.Before(scopeHi) {
					times = append(times, a.Start)
				}
				if a.End.After(scopeLo) && a.End.Before(scopeHi) {
					times = append(times, a.End)
				}
			}
		}
	}
	addSegBounds(oldSegs)
	addSegBounds(newSegs)
	addAllocBounds(oldAlloc)
	addAllocBounds(newAlloc)
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	uniq := times[:0]
	for _, t := range times {
		if len(uniq) == 0 || !uniq[len(uniq)-1].Equal(t) {
			uniq = append(uniq, t)
		}
	}

	powerOn := func(segs []Segment, t time.Time) int64 {
		for i := range segs {
			s := &segs[i]
			if (!t.Before(s.Start)) && t.Before(s.End) {
				return s.PowerW
			}
		}
		return 0
	}
	sumAt := func(allocs map[string][]SlotAllocation, t time.Time) int64 {
		var sum int64
		for _, as := range allocs {
			for i := range as {
				a := &as[i]
				if (!t.Before(a.Start)) && t.Before(a.End) {
					sum += a.PowerW
					break
				}
			}
		}
		return sum
	}

	for i := 0; i+1 < len(uniq); i++ {
		s, e := uniq[i], uniq[i+1]
		newCap := powerOn(newSegs, s)
		oldRes := sumAt(oldAlloc, s)
		newRes := sumAt(newAlloc, s)
		if newRes > newCap {
			return fail(KindCapacity, op,
				"调整后 [%s,%s) 占用 %dW 超过降容后容量 %dW，方案作废",
				s.Format(time.RFC3339Nano), e.Format(time.RFC3339Nano), newRes, newCap)
		}
		if (!s.Before(ev.Start)) && !e.After(ev.End) && newRes > oldRes {
			return fail(KindCapacity, op,
				"事件窗口 [%s,%s) 内占用由 %dW 上升到 %dW，违反容量守恒，方案作废",
				s.Format(time.RFC3339Nano), e.Format(time.RFC3339Nano), oldRes, newRes)
		}
	}

	for id, as := range newAlloc {
		p := curPlans[id]
		if p == nil {
			continue
		}
		work := Zero128()
		for _, a := range as {
			work = Add128(work, Mul128(a.PowerW, a.End.Sub(a.Start).Nanoseconds()))
		}
		target := Mul128(p.MinEnergy, nanoWorkPerMicroWh)
		if Cmp128(work, target) < 0 {
			return fail(KindCapacity, op,
				"预约 %q 调整后交付能量低于最低需求，方案作废", id)
		}
	}
	return nil
}
