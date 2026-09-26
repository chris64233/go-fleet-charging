package fleetcharging

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func tmust(t *testing.T, tm string, loc ...*time.Location) time.Time {
	loc = append(loc, time.UTC)
	tt, err := time.ParseInLocation("2006-01-02T15:04:05", tm, loc[0])
	if err != nil {
		if t != nil {
			t.Fatalf("parse %q: %v", tm, err)
		}
		panic(err)
	}
	return tt
}

// deliveredUWh 按半开区间求计划实际交付能量（µWh，向下取整），
// 独立于生产代码的 W·ns 计算路径，用于交叉验证精确性。
func deliveredUWh(p *Plan) int64 {
	total := Zero128()
	for _, a := range p.Allocation {
		total = Add128(total, Mul128(a.PowerW, a.End.Sub(a.Start).Nanoseconds()))
	}
	// floor(total / 3.6e6)
	q, _ := QuoRem128(total, nanoWorkPerMicroWh)
	if !q.IsInt64() {
		panic("delivered energy exceeds int64")
	}
	return int64(q.lo)
}

func twoHourStation() StationConfig {
	return StationConfig{
		StationID: "S1",
		Segments: []Segment{
			{Start: tmust(nil, "2026-01-01T10:00:00"), End: tmust(nil, "2026-01-01T11:00:00"), PowerW: 100_000},
			{Start: tmust(nil, "2026-01-01T11:00:00"), End: tmust(nil, "2026-01-01T12:00:00"), PowerW: 100_000},
		},
	}
}

func newTestService(t *testing.T, now time.Time) (*Service, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	svc, err := NewService(store, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, store
}

func TestSubmitAndExactEnergy(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatalf("ConfigureStation: %v", err)
	}

	// 50 kWh 跨两小时窗口：贪心应只在第一小时分配 50kW，恰好 50kWh。
	req := ChargeRequest{
		RequestID:    "R1",
		StationID:    "S1",
		VehicleID:    "V1",
		Arrival:      tmust(t, "2026-01-01T10:00:00"),
		Departure:    tmust(t, "2026-01-01T12:00:00"),
		MinEnergyUWh: 50_000_000_000,
		MaxPowerW:    100_000,
	}
	plan, err := svc.SubmitPlan(ctx, req)
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if len(plan.Allocation) != 1 || plan.Allocation[0].PowerW != 50_000 {
		t.Fatalf("unexpected allocation: %+v", plan.Allocation)
	}
	if got := deliveredUWh(plan); got != 50_000_000_000 {
		t.Fatalf("delivered = %d µWh, want 50000000000", got)
	}
	if plan.Status != StatusActive || plan.Revision != 1 {
		t.Fatalf("unexpected plan state: %+v", plan)
	}
}

func TestExactEnergyNonAlignedCeilNeverUnderDelivers(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	// 非整点边界：10:00:00 ~ 10:00:01（1 秒），站点容量 1000W。
	cfg := StationConfig{StationID: "S1", Segments: []Segment{{
		Start:  tmust(t, "2026-01-01T10:00:00"),
		End:    tmust(t, "2026-01-01T10:00:01"),
		PowerW: 1000,
	}}}
	if err := svc.ConfigureStation(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// 只要 1 µWh：1W × 1s = 277.78 µWh，向上取整为 1W，绝不少给。
	req := ChargeRequest{
		RequestID: "R1", StationID: "S1", VehicleID: "V1",
		Arrival:      cfg.Segments[0].Start,
		Departure:    cfg.Segments[0].End,
		MinEnergyUWh: 1,
		MaxPowerW:    1000,
	}
	plan, err := svc.SubmitPlan(ctx, req)
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if plan.Allocation[0].PowerW != 1 {
		t.Fatalf("power = %d, want 1", plan.Allocation[0].PowerW)
	}
	if got := deliveredUWh(plan); got < 1 {
		t.Fatalf("delivered %d µWh < requested 1", got)
	}
}

func TestHalfOpenWindowNoDoubleCountAtBoundary(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	// 两辆车在 11:00 处首尾相接；左闭右开意味着它们在边界点不共享容量，
	// 各取满 100kW 都应成功。
	mk := func(id, veh, a, b string, kwh int64) ChargeRequest {
		return ChargeRequest{
			RequestID: id, StationID: "S1", VehicleID: veh,
			Arrival: tmust(t, a), Departure: tmust(t, b),
			MinEnergyUWh: kwh * 1_000_000_000, MaxPowerW: 100_000,
		}
	}
	if _, err := svc.SubmitPlan(ctx, mk("RA", "VA", "2026-01-01T10:00:00", "2026-01-01T11:00:00", 100)); err != nil {
		t.Fatalf("plan A: %v", err)
	}
	if _, err := svc.SubmitPlan(ctx, mk("RB", "VB", "2026-01-01T11:00:00", "2026-01-01T12:00:00", 100)); err != nil {
		t.Fatalf("plan B: %v", err)
	}
}

func TestSubmitCapacityRejectIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	// A：第一小时 100kW + 第二小时 60kW（共 160kWh）。
	a := ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "VA",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T12:00:00"),
		MinEnergyUWh: 160_000_000_000, MaxPowerW: 100_000,
	}
	if _, err := svc.SubmitPlan(ctx, a); err != nil {
		t.Fatalf("plan A: %v", err)
	}
	// B：第一小时还想要 10kWh（需 10kW），但第一小时已被占满 → 必须整体拒绝。
	b := ChargeRequest{
		RequestID: "RB", StationID: "S1", VehicleID: "VB",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T11:00:00"),
		MinEnergyUWh: 10_000_000_000, MaxPowerW: 100_000,
	}
	_, err := svc.SubmitPlan(ctx, b)
	if KindOf(err) != KindCapacity {
		t.Fatalf("want KindCapacity, got %v (%v)", KindOf(err), err)
	}

	// 拒绝不能留下任何部分占用：第一小时仍恰好 100kW。
	occ, err := svc.Occupancy(ctx, "S1",
		tmust(t, "2026-01-01T10:00:00"), tmust(t, "2026-01-01T12:00:00"))
	if err != nil {
		t.Fatal(err)
	}
	if occ[0].ReservedW != 100_000 || occ[0].AvailableW != 0 {
		t.Fatalf("h1 occupancy leaked: %+v", occ[0])
	}
	if occ[1].ReservedW != 60_000 || occ[1].AvailableW != 40_000 {
		t.Fatalf("h2 occupancy wrong: %+v", occ[1])
	}
}

func TestSubmitPartialFitButOverallShortRejects(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	// A 占满第一小时。
	a := ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "VA",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T11:00:00"),
		MinEnergyUWh: 100_000_000_000, MaxPowerW: 100_000,
	}
	if _, err := svc.SubmitPlan(ctx, a); err != nil {
		t.Fatal(err)
	}
	// B 跨两小时要 150kWh：第二小时最多给 100kWh，总量不够 → 拒绝，
	// 且第二小时也不能被先占住。
	b := ChargeRequest{
		RequestID: "RB", StationID: "S1", VehicleID: "VB",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T12:00:00"),
		MinEnergyUWh: 150_000_000_000, MaxPowerW: 100_000,
	}
	if _, err := svc.SubmitPlan(ctx, b); KindOf(err) != KindCapacity {
		t.Fatalf("want capacity error, got %v", err)
	}
	occ, _ := svc.Occupancy(ctx, "S1",
		tmust(t, "2026-01-01T11:00:00"), tmust(t, "2026-01-01T12:00:00"))
	if occ[0].ReservedW != 0 {
		t.Fatalf("partial reservation leaked into h2: %+v", occ[0])
	}
}

func TestIdempotencySameReturnsOriginalDifferentConflicts(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	req := ChargeRequest{
		RequestID: "R1", StationID: "S1", VehicleID: "V1",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T12:00:00"),
		MinEnergyUWh: 30_000_000_000, MaxPowerW: 100_000,
	}
	p1, err := svc.SubmitPlan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := svc.SubmitPlan(ctx, req)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if p1.ID != p2.ID || p1.Revision != p2.Revision {
		t.Fatalf("replay did not return original plan: %+v vs %+v", p1, p2)
	}

	// 同号不同内容 → 幂等冲突，即使容量已变化也不覆盖原计划。
	diff := req
	diff.MinEnergyUWh = 40_000_000_000
	_, err = svc.SubmitPlan(ctx, diff)
	if KindOf(err) != KindIdempotency {
		t.Fatalf("want idempotency conflict, got %v", err)
	}
	if !errors.Is(err, ErrIdempotent) {
		t.Fatalf("errors.Is(ErrIdempotent) failed: %v", err)
	}
	p3, _ := svc.GetPlan(ctx, "R1")
	if p3.MinEnergy != 30_000_000_000 {
		t.Fatalf("original plan mutated by conflicting submit: %+v", p3)
	}
}

func TestConcurrentContentionOnlyFittingPlansCommit(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	const n = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	var ok, rejected int64
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			req := ChargeRequest{
				RequestID: fmt.Sprintf("R%02d", i), StationID: "S1", VehicleID: fmt.Sprintf("V%02d", i),
				Arrival: tmust(nil, "2026-01-01T10:00:00"), Departure: tmust(nil, "2026-01-01T11:00:00"),
				MinEnergyUWh: 60_000_000_000, // 需 60kW，100kW 容量最多 1 辆
				MaxPowerW:    60_000,
			}
			_, err := svc.SubmitPlan(ctx, req)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if KindOf(err) == KindCapacity {
				rejected++
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok != 1 || rejected != n-1 {
		t.Fatalf("ok=%d rejected=%d, want 1/%d", ok, rejected, n-1)
	}
	occ, _ := svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T11:00:00"))
	if occ[0].ReservedW != 60_000 {
		t.Fatalf("reserved=%d, want 60000", occ[0].ReservedW)
	}
}

func TestModifyAtomicallyReplacesReservation(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	a := ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "VA",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T11:00:00"),
		MinEnergyUWh: 50_000_000_000, MaxPowerW: 100_000,
	}
	pa, err := svc.SubmitPlan(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	b := ChargeRequest{
		RequestID: "RB", StationID: "S1", VehicleID: "VB",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T11:00:00"),
		MinEnergyUWh: 50_000_000_000, MaxPowerW: 100_000,
	}
	if _, err := svc.SubmitPlan(ctx, b); err != nil {
		t.Fatal(err)
	}

	// A 想扩大到 90kWh：只剩 50kW 空闲 → 容量不足，旧占用原样保留。
	_, err = svc.ModifyPlan(ctx, "RA", PlanModification{
		StationID: "S1", VehicleID: "VA",
		Arrival: pa.Arrival, Departure: pa.Departure,
		MinEnergyUWh: 90_000_000_000, MaxPowerW: 100_000,
	})
	if KindOf(err) != KindCapacity {
		t.Fatalf("want capacity, got %v", err)
	}
	got, _ := svc.GetPlan(ctx, "RA")
	if got.MinEnergy != 50_000_000_000 || got.Revision != 1 {
		t.Fatalf("failed modify mutated plan: %+v", got)
	}
	occ, _ := svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T11:00:00"))
	if occ[0].ReservedW != 100_000 {
		t.Fatalf("reservation changed after failed modify: %+v", occ[0])
	}

	// A 缩小到 20kWh：原子替换，释放 30kW，revision 递增。
	pa2, err := svc.ModifyPlan(ctx, "RA", PlanModification{
		StationID: "S1", VehicleID: "VA",
		Arrival: pa.Arrival, Departure: pa.Departure,
		MinEnergyUWh: 20_000_000_000, MaxPowerW: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pa2.Revision != 2 || pa2.Allocation[0].PowerW != 20_000 {
		t.Fatalf("modify result wrong: %+v", pa2)
	}
	occ, _ = svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T11:00:00"))
	if occ[0].ReservedW != 70_000 || occ[0].AvailableW != 30_000 {
		t.Fatalf("occupancy after shrink wrong: %+v", occ[0])
	}
}

func TestModifyUnknownAndCancelledAndArrived(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	a := ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "VA",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T12:00:00"),
		MinEnergyUWh: 10_000_000_000, MaxPowerW: 100_000,
	}
	if _, err := svc.SubmitPlan(ctx, a); err != nil {
		t.Fatal(err)
	}
	mod := PlanModification{
		StationID: "S1", VehicleID: "VA",
		Arrival: a.Arrival, Departure: a.Departure,
		MinEnergyUWh: 11_000_000_000, MaxPowerW: 100_000,
	}
	if _, err := svc.ModifyPlan(ctx, "NOPE", mod); KindOf(err) != KindState {
		t.Fatalf("unknown modify: %v", err)
	}

	// 越过到达点：计划锁定为 arrived。
	svc.now = func() time.Time { return tmust(nil, "2026-01-01T10:00:00") }
	if _, err := svc.ModifyPlan(ctx, "RA", mod); KindOf(err) != KindState {
		t.Fatalf("modify at arrival: %v", err)
	}
	p, _ := svc.GetPlan(ctx, "RA")
	if p.Status != StatusArrived {
		t.Fatalf("status=%s, want arrived", p.Status)
	}
	if err := svc.CancelPlan(ctx, "RA"); KindOf(err) != KindState {
		t.Fatalf("cancel after arrival: %v", err)
	}
}

func TestCancelReleasesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	a := ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "VA",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T12:00:00"),
		MinEnergyUWh: 120_000_000_000, MaxPowerW: 100_000, // 两小时各 60kW
	}
	if _, err := svc.SubmitPlan(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := svc.CancelPlan(ctx, "RA"); err != nil {
		t.Fatal(err)
	}
	if err := svc.CancelPlan(ctx, "RA"); KindOf(err) != KindState {
		t.Fatalf("double cancel: %v", err)
	}
	if err := svc.CancelPlan(ctx, "MISSING"); KindOf(err) != KindState {
		t.Fatalf("cancel missing: %v", err)
	}
	occ, _ := svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T12:00:00"))
	for _, o := range occ {
		if o.ReservedW != 0 || o.AvailableW != o.CapacityW {
			t.Fatalf("capacity not fully released: %+v", o)
		}
	}
	// 同号同内容重放仍返回原（已取消）计划，不会新建第二条占用。
	p, err := svc.SubmitPlan(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusCancelled {
		t.Fatalf("replay status=%s, want cancelled", p.Status)
	}
	occ, _ = svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T12:00:00"))
	for _, o := range occ {
		if o.ReservedW != 0 {
			t.Fatalf("replay of cancelled plan reserved capacity: %+v", o)
		}
	}
}

func TestCancelVsArrivalRaceNoLeakNoDoubleRelease(t *testing.T) {
	ctx := context.Background()
	for round := 0; round < 50; round++ {
		var mu sync.Mutex
		now := tmust(nil, "2026-01-01T09:59:59")
		store := NewMemoryStore()
		svc, err := NewService(store, WithClock(func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}))
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
			t.Fatal(err)
		}
		req := ChargeRequest{
			RequestID: fmt.Sprintf("R%d", round), StationID: "S1", VehicleID: "V",
			Arrival: tmust(nil, "2026-01-01T10:00:00"), Departure: tmust(nil, "2026-01-01T11:00:00"),
			MinEnergyUWh: 30_000_000_000, MaxPowerW: 100_000,
		}
		if _, err := svc.SubmitPlan(ctx, req); err != nil {
			t.Fatal(err)
		}

		// 多个 goroutine：一些翻时钟越过到达点，一些反复取消。
		var wg sync.WaitGroup
		flip := make(chan struct{})
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-flip
				if g == 0 {
					mu.Lock()
					now = tmust(nil, "2026-01-01T10:00:00")
					mu.Unlock()
				}
				_ = svc.CancelPlan(ctx, req.RequestID)
			}(g)
		}
		close(flip)
		wg.Wait()

		p, _ := svc.GetPlan(ctx, req.RequestID)
		occ, _ := svc.Occupancy(ctx, "S1",
			tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T11:00:00"))
		switch p.Status {
		case StatusCancelled:
			if occ[0].ReservedW != 0 {
				t.Fatalf("round %d: cancelled but occupancy leaked: %+v", round, occ[0])
			}
		case StatusArrived:
			if occ[0].ReservedW != 30_000 {
				t.Fatalf("round %d: arrived but reservation wrong: %+v", round, occ[0])
			}
		default:
			t.Fatalf("round %d: unexpected status %s", round, p.Status)
		}
	}
}

func TestOccupancySplitsAtPlanBoundaries(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	a := ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "VA",
		Arrival: tmust(t, "2026-01-01T10:30:00"), Departure: tmust(t, "2026-01-01T11:30:00"),
		MinEnergyUWh: 20_000_000_000, MaxPowerW: 20_000, // 恰好两小时各 20kW…窗口只有1h
	}
	// 窗口 1 小时，20kWh @ 20kW。
	if _, err := svc.SubmitPlan(ctx, a); err != nil {
		t.Fatal(err)
	}
	occ, err := svc.Occupancy(ctx, "S1",
		tmust(t, "2026-01-01T10:00:00"), tmust(t, "2026-01-01T12:00:00"))
	if err != nil {
		t.Fatal(err)
	}
	// 期望边界：10:00 10:30 11:00 11:30 12:00。
	want := []struct {
		t0, t1    string
		cap, resv int64
	}{
		{"2026-01-01T10:00:00", "2026-01-01T10:30:00", 100_000, 0},
		{"2026-01-01T10:30:00", "2026-01-01T11:00:00", 100_000, 20_000},
		{"2026-01-01T11:00:00", "2026-01-01T11:30:00", 100_000, 20_000},
		{"2026-01-01T11:30:00", "2026-01-01T12:00:00", 100_000, 0},
	}
	if len(occ) != len(want) {
		t.Fatalf("rows=%d want %d: %+v", len(occ), len(want), occ)
	}
	for i, w := range want {
		if !occ[i].Start.Equal(tmust(nil, w.t0)) || !occ[i].End.Equal(tmust(nil, w.t1)) ||
			occ[i].CapacityW != w.cap || occ[i].ReservedW != w.resv ||
			occ[i].AvailableW != w.cap-w.resv {
			t.Fatalf("row %d = %+v, want %+v", i, occ[i], w)
		}
	}
}

func TestScheduleSeesCapacityFreedAfterPlanFinishesEarly(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	// 两段配置：10:00–10:30 容量 100kW，10:30–11:00 容量 60kW。
	cfg := StationConfig{StationID: "S1", Segments: []Segment{
		{Start: tmust(t, "2026-01-01T10:00:00"), End: tmust(t, "2026-01-01T10:30:00"), PowerW: 100_000},
		{Start: tmust(t, "2026-01-01T10:30:00"), End: tmust(t, "2026-01-01T11:00:00"), PowerW: 60_000},
	}}
	if err := svc.ConfigureStation(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// A：50kWh、功率上限 100kW → 前 30 分钟 100kW 恰好充满，10:30 后占用为 0。
	a := ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "VA",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T11:00:00"),
		MinEnergyUWh: 50_000_000_000, MaxPowerW: 100_000,
	}
	pa, err := svc.SubmitPlan(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(pa.Allocation) != 1 || !pa.Allocation[0].End.Equal(tmust(t, "2026-01-01T10:30:00")) {
		t.Fatalf("A should finish at 10:30: %+v", pa.Allocation)
	}
	// B：30kWh 跨整小时；前半被 A 占满，应只用后半段 60kW（30kWh）。
	b := ChargeRequest{
		RequestID: "RB", StationID: "S1", VehicleID: "VB",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T11:00:00"),
		MinEnergyUWh: 30_000_000_000, MaxPowerW: 100_000,
	}
	pb, err := svc.SubmitPlan(ctx, b)
	if err != nil {
		t.Fatalf("B should fit into freed second half: %v", err)
	}
	if len(pb.Allocation) != 1 || !pb.Allocation[0].Start.Equal(tmust(t, "2026-01-01T10:30:00")) ||
		pb.Allocation[0].PowerW != 60_000 {
		t.Fatalf("B allocation wrong: %+v", pb.Allocation)
	}
	// C：再要 1kWh 已无处可放 → 整体拒绝。
	c := ChargeRequest{
		RequestID: "RC", StationID: "S1", VehicleID: "VC",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T11:00:00"),
		MinEnergyUWh: 1_000_000_000, MaxPowerW: 100_000,
	}
	if _, err := svc.SubmitPlan(ctx, c); KindOf(err) != KindCapacity {
		t.Fatalf("C: want capacity, got %v", err)
	}
}

func TestConfigureCatchesOverloadHiddenInsideSingleNewSegment(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	// 旧配置：前半小时容量仅 20kW，后半小时 60kW。
	cfg := StationConfig{StationID: "S1", Segments: []Segment{
		{Start: tmust(t, "2026-01-01T10:00:00"), End: tmust(t, "2026-01-01T10:30:00"), PowerW: 20_000},
		{Start: tmust(t, "2026-01-01T10:30:00"), End: tmust(t, "2026-01-01T11:00:00"), PowerW: 60_000},
	}}
	if err := svc.ConfigureStation(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// A 跨整小时要 40kWh：前半小时 20kW(10kWh) + 后半小时 60kW(30kWh)，
	// 分配功率在 10:30 处从 20kW 跳到 60kW——该点既非到达也非离开。
	a := ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "VA",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T11:00:00"),
		MinEnergyUWh: 40_000_000_000, MaxPowerW: 100_000,
	}
	if _, err := svc.SubmitPlan(ctx, a); err != nil {
		t.Fatal(err)
	}
	// 新配置合并为单个整小时时段 50kW：前半小时 20kW 不超额，
	// 但后半小时 60kW 超额。若不按旧分配边界切分就会漏检。
	cut := StationConfig{StationID: "S1", Segments: []Segment{{
		Start: tmust(t, "2026-01-01T10:00:00"), End: tmust(t, "2026-01-01T11:00:00"), PowerW: 50_000,
	}}}
	if err := svc.ConfigureStation(ctx, cut); KindOf(err) != KindCapacity {
		t.Fatalf("want hidden overload detected, got %v", err)
	}
}

func TestWindowOutsideConfiguredSegmentsIsTimeError(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		a, b string
	}{
		{"before", "2026-01-01T09:00:00", "2026-01-01T10:00:00"},
		{"after", "2026-01-01T12:00:00", "2026-01-01T13:00:00"},
		{"gap-inside", "2026-01-01T10:30:00", "2026-01-01T12:30:00"},
	}
	for _, c := range cases {
		req := ChargeRequest{
			RequestID: "R-" + c.name, StationID: "S1", VehicleID: "V",
			Arrival: tmust(t, c.a), Departure: tmust(t, c.b),
			MinEnergyUWh: 1_000_000_000, MaxPowerW: 10_000,
		}
		_, err := svc.SubmitPlan(ctx, req)
		if KindOf(err) != KindTime {
			t.Fatalf("%s: want KindTime, got %v", c.name, err)
		}
	}
}

func TestConfigureRejectsLoweredCapacityBelowReservations(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	req := ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "VA",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T11:00:00"),
		MinEnergyUWh: 80_000_000_000, MaxPowerW: 100_000,
	}
	if _, err := svc.SubmitPlan(ctx, req); err != nil {
		t.Fatal(err)
	}
	small := twoHourStation()
	small.Segments[0].PowerW = 50_000
	if err := svc.ConfigureStation(ctx, small); KindOf(err) != KindCapacity {
		t.Fatalf("want capacity error, got %v", err)
	}
	// 旧配置仍在：还能用剩余 20kW。
	req2 := ChargeRequest{
		RequestID: "RB", StationID: "S1", VehicleID: "VB",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T11:00:00"),
		MinEnergyUWh: 20_000_000_000, MaxPowerW: 100_000,
	}
	if _, err := svc.SubmitPlan(ctx, req2); err != nil {
		t.Fatalf("old config should remain intact: %v", err)
	}
}

func TestParameterValidationErrorKinds(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)

	if err := svc.ConfigureStation(ctx, StationConfig{StationID: "S", Segments: []Segment{{
		Start: tmust(t, "2026-01-01T11:00:00"), End: tmust(t, "2026-01-01T10:00:00"), PowerW: 1,
	}}}); KindOf(err) != KindTime {
		t.Fatalf("reversed segment: %v", err)
	}
	if err := svc.ConfigureStation(ctx, StationConfig{StationID: "", Segments: []Segment{{
		Start: tmust(t, "2026-01-01T10:00:00"), End: tmust(t, "2026-01-01T11:00:00"), PowerW: 1,
	}}}); KindOf(err) != KindParameter {
		t.Fatalf("empty station: %v", err)
	}

	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	base := ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "VA",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T11:00:00"),
		MinEnergyUWh: 1_000_000_000, MaxPowerW: 10_000,
	}
	bad := base
	bad.RequestID = ""
	if _, err := svc.SubmitPlan(ctx, bad); KindOf(err) != KindParameter {
		t.Fatalf("empty id: %v", err)
	}
	bad = base
	bad.MinEnergyUWh = 0
	if _, err := svc.SubmitPlan(ctx, bad); KindOf(err) != KindParameter {
		t.Fatalf("zero energy: %v", err)
	}
	bad = base
	bad.StationID = "NOPE"
	if _, err := svc.SubmitPlan(ctx, bad); KindOf(err) != KindParameter {
		t.Fatalf("unknown station: %v", err)
	}
	bad = base
	bad.Arrival, bad.Departure = bad.Departure, bad.Arrival
	if _, err := svc.SubmitPlan(ctx, bad); KindOf(err) != KindTime {
		t.Fatalf("arrival>=departure: %v", err)
	}
	// 到达时间已过 → 时间错误（站点配置覆盖该过去窗口，以排除窗口错误干扰）。
	pastCfg := StationConfig{StationID: "SP", Segments: []Segment{{
		Start: tmust(t, "2026-01-01T07:00:00"), End: tmust(t, "2026-01-01T09:00:00"), PowerW: 10_000,
	}}}
	if err := svc.ConfigureStation(ctx, pastCfg); err != nil {
		t.Fatal(err)
	}
	bad = base
	bad.RequestID = "OLD"
	bad.StationID = "SP"
	bad.Arrival = tmust(t, "2026-01-01T08:00:00")
	bad.Departure = tmust(t, "2026-01-01T09:00:00")
	if _, err := svc.SubmitPlan(ctx, bad); KindOf(err) != KindTime {
		t.Fatalf("past arrival: %v", err)
	}
}

func TestFileStorePersistenceAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := dir + "/state.json"
	now := tmust(t, "2026-01-01T09:00:00")

	open := func() *Service {
		svc, err := NewService(NewFileStore(path), WithClock(func() time.Time { return now }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return svc
	}
	svc := open()
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	req := ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "VA",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T12:00:00"),
		MinEnergyUWh: 50_000_000_000, MaxPowerW: 100_000,
	}
	p1, err := svc.SubmitPlan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	// 重新打开：配置、计划、幂等记录都应还在。
	svc2 := open()
	p2, err := svc2.GetPlan(ctx, "RA")
	if err != nil {
		t.Fatalf("after restart: %v", err)
	}
	if p2.ID != p1.ID || deliveredUWh(p2) != 50_000_000_000 {
		t.Fatalf("plan mismatch after restart: %+v", p2)
	}
	// 幂等重放仍返回同一计划。
	p3, err := svc2.SubmitPlan(ctx, req)
	if err != nil || p3.ID != p1.ID {
		t.Fatalf("idempotency after restart: %v %+v", err, p3)
	}
	// 容量约束在重启后继续生效。
	over := ChargeRequest{
		RequestID: "RB", StationID: "S1", VehicleID: "VB",
		Arrival: tmust(t, "2026-01-01T10:00:00"), Departure: tmust(t, "2026-01-01T11:00:00"),
		MinEnergyUWh: 60_000_000_000, MaxPowerW: 100_000,
	}
	if _, err := svc2.SubmitPlan(ctx, over); KindOf(err) != KindCapacity {
		t.Fatalf("capacity after restart: %v", err)
	}
	// 取消后再次重启，释放结果仍然持久。
	if err := svc2.CancelPlan(ctx, "RA"); err != nil {
		t.Fatal(err)
	}
	svc3 := open()
	p4, _ := svc3.GetPlan(ctx, "RA")
	if p4.Status != StatusCancelled {
		t.Fatalf("status after restart = %s", p4.Status)
	}
}
