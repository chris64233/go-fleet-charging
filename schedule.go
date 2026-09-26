package fleetcharging

import (
	"sort"
	"time"
)

// piece 是调度用的最小时间片：在 [start,end) 内站点容量与既有占用均为常数。
type piece struct {
	start    time.Time
	end      time.Time
	capacity int64 // 站点该时间片可用功率（W），配置空隙为 0
	reserved int64 // 既有有效计划在该时间片已占用功率（W）
}

// buildPieces 取站点配置时段边界与 [from,to) 的并集切分时间轴，
// 并汇总所有有效计划（active/arrived）在每个时间片上的占用功率。
// 被排除的计划（如修改中的自身）通过 excludeID 传入。
func buildPieces(segs []Segment, from, to time.Time, plans []*Plan, excludeID string) []piece {
	times := []time.Time{from, to}
	for _, s := range segs {
		if s.Start.After(from) && s.Start.Before(to) {
			times = append(times, s.Start)
		}
		if s.End.After(from) && s.End.Before(to) {
			times = append(times, s.End)
		}
	}
	// 计划的在场边界与实际分配边界也要作为切分点，保证每个时间片内占用恒定
	//（重配站点后，旧分配边界不一定落在新配置时段边界上）。
	for _, p := range plans {
		if p.Status == StatusCancelled {
			continue
		}
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
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

	uniq := times[:0]
	for _, t := range times {
		if len(uniq) == 0 || !uniq[len(uniq)-1].Equal(t) {
			uniq = append(uniq, t)
		}
	}

	pieces := make([]piece, 0, len(uniq)-1)
	for i := 0; i+1 < len(uniq); i++ {
		pieces = append(pieces, piece{start: uniq[i], end: uniq[i+1]})
	}

	for i := range pieces {
		t := pieces[i].start
		for j := range segs {
			s := &segs[j]
			if (!t.Before(s.Start)) && t.Before(s.End) {
				pieces[i].capacity = s.PowerW
				break
			}
		}
		var reserved int64
		for _, p := range plans {
			if p.ID == excludeID || p.Status == StatusCancelled {
				continue
			}
			if !intervalOverlaps(p.Arrival, p.Departure, t, pieces[i].end) {
				continue
			}
			for k := range p.Allocation {
				a := &p.Allocation[k]
				if (!t.Before(a.Start)) && t.Before(a.End) {
					reserved += a.PowerW
					break
				}
			}
		}
		pieces[i].reserved = reserved
	}
	return pieces
}

// intervalOverlaps 报告两个半开区间是否有正长度相交。
func intervalOverlaps(aStart, aEnd, bStart, bEnd time.Time) bool {
	return aStart.Before(bEnd) && bStart.Before(aEnd)
}

// fullyConfigured 报告 [from,to) 是否完全落在有正容量的配置时段内（无越界、无空隙）。
func fullyConfigured(pieces []piece) bool {
	for _, p := range pieces {
		if p.capacity <= 0 {
			return false
		}
	}
	return true
}

// allocationResult 是一次成功调度的结果。
type allocationResult struct {
	allocation []SlotAllocation
	totalWork  Int128 // 实际可交付功量（W·ns）
}

// scheduleAllocation 在给定时间片上贪心分配功率：优先填满早的时间片，
// 最后一个时间片只取补足目标能量所需的最小整数功率（向上取整，宁多勿少）。
// 返回的 totalWork 可能略大于 target（整数瓦取整所致），但绝不会小于。
func scheduleAllocation(ps []piece, maxPowerW int64, targetWork Int128) ([]SlotAllocation, Int128) {
	acc := Zero128()
	allocs := make([]SlotAllocation, 0, len(ps))
	for _, p := range ps {
		avail := p.capacity - p.reserved
		if avail <= 0 || Cmp128(acc, targetWork) >= 0 {
			continue
		}
		power := avail
		if power > maxPowerW {
			power = maxPowerW
		}
		dur := p.end.Sub(p.start).Nanoseconds()
		work := Mul128(power, dur)
		if Cmp128(Add128(acc, work), targetWork) >= 0 {
			remaining := Sub128(targetWork, acc)
			last := CeilDiv128(remaining, dur)
			if last < 1 {
				last = 1
			}
			if last > power {
				// 理论不可达：整块功量足够而 ceil 反而超上限。
				last = power
			}
			allocs = append(allocs, SlotAllocation{Start: p.start, End: p.end, PowerW: last})
			acc = Add128(acc, Mul128(last, dur))
			break
		}
		allocs = append(allocs, SlotAllocation{Start: p.start, End: p.end, PowerW: power})
		acc = Add128(acc, work)
	}
	return mergeAdjacent(allocs), acc
}

// mergeAdjacent 合并端点相接且功率相同的相邻分配，使计划更紧凑。
func mergeAdjacent(in []SlotAllocation) []SlotAllocation {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for i := 1; i < len(in); i++ {
		last := &out[len(out)-1]
		if last.End.Equal(in[i].Start) && last.PowerW == in[i].PowerW {
			last.End = in[i].End
			continue
		}
		out = append(out, in[i])
	}
	return out
}
