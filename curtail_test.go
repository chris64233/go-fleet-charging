package fleetcharging

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func t10() time.Time { return tmust(nil, "2026-01-01T10:00:00") }
func t11() time.Time { return tmust(nil, "2026-01-01T11:00:00") }
func t12() time.Time { return tmust(nil, "2026-01-01T12:00:00") }

func findAdjustment(t *testing.T, p *CurtailmentPlan, req string) Adjustment {
	t.Helper()
	for _, a := range p.Adjustments {
		if a.RequestID == req {
			return a
		}
	}
	t.Fatalf("adjustment %q not found", req)
	return Adjustment{}
}

func allocPowerAt(as []SlotAllocation, at time.Time) int64 {
	for _, a := range as {
		if (!at.Before(a.Start)) && at.Before(a.End) {
			return a.PowerW
		}
	}
	return 0
}

func newCurtailService(t *testing.T) *Service {
	t.Helper()
	svc, _ := newTestService(t, tmust(nil, "2026-01-01T09:00:00"))
	if err := svc.ConfigureStation(context.Background(), twoHourStation()); err != nil {
		t.Fatal(err)
	}
	return svc
}

func curReq(id string, prio int32, fixed bool, a, d time.Time, kwh int64) ChargeRequest {
	return ChargeRequest{
		RequestID:    id,
		StationID:    "S1",
		VehicleID:    "V-" + id,
		Arrival:      a,
		Departure:    d,
		MinEnergyUWh: kwh * 1_000_000_000,
		MaxPowerW:    100_000,
		Priority:     prio,
		FixedPower:   fixed,
	}
}

// 基本容量下降：唯一可调整预约被削出的功率转移到窗口外空闲时段，最低电量不变、
// 窗口占用严格下降、确认后逐时段占用守恒。
func TestCurtailmentShiftsLoadOutOfWindow(t *testing.T) {
	ctx := context.Background()
	svc := newCurtailService(t)
	// L：10:00–12:00 要 100kWh，贪心落在第一个小时 100kW。
	if _, err := svc.SubmitPlan(ctx, curReq("RL", 1, false, t10(), t12(), 100)); err != nil {
		t.Fatal(err)
	}

	ev := CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 60_000}
	plan, err := svc.PrepareCurtailment(ctx, ev)
	if err != nil {
		t.Fatalf("PrepareCurtailment: %v", err)
	}
	if got := plan.ChangedRequests(); len(got) != 1 || got[0] != "RL" {
		t.Fatalf("changed=%v, want [RL]", got)
	}
	adj := findAdjustment(t, plan, "RL")
	// 窗口内 100kW→60kW；缺额 40kWh 转移到 11–12 点。
	if allocPowerAt(adj.NewAllocation, t10()) != 60_000 {
		t.Fatalf("window power = %d, want 60000", allocPowerAt(adj.NewAllocation, t10()))
	}
	if allocPowerAt(adj.NewAllocation, t11()) != 40_000 {
		t.Fatalf("shifted power = %d, want 40000", allocPowerAt(adj.NewAllocation, t11()))
	}
	post := &Plan{Allocation: adj.NewAllocation, MinEnergy: 100_000_000_000}
	if got := deliveredUWh(post); got != 100_000_000_000 {
		t.Fatalf("delivered after shift = %d µWh, want 100kWh", got)
	}

	changed, err := svc.ConfirmCurtailment(ctx, plan)
	if err != nil {
		t.Fatalf("ConfirmCurtailment: %v", err)
	}
	if len(changed) != 1 || changed[0].Revision != 2 {
		t.Fatalf("changed plans = %+v, want single RL at revision 2", changed)
	}

	occ, _ := svc.Occupancy(ctx, "S1", t10(), t12())
	if len(occ) != 2 {
		t.Fatalf("occupancy rows=%d, want 2", len(occ))
	}
	// 第一小时容量已降为 60kW 且占满；第二小时容量 100kW，占用 40kW。
	if occ[0].CapacityW != 60_000 || occ[0].ReservedW != 60_000 || occ[0].AvailableW != 0 {
		t.Fatalf("h1 occupancy wrong: %+v", occ[0])
	}
	if occ[1].CapacityW != 100_000 || occ[1].ReservedW != 40_000 || occ[1].AvailableW != 60_000 {
		t.Fatalf("h2 occupancy wrong: %+v", occ[1])
	}
}

// 优先级：容量不足时高优先级预约尽量留在窗口内，低优先级承担位移。
func TestCurtailmentRespectsPriority(t *testing.T) {
	ctx := context.Background()
	svc := newCurtailService(t)
	// H 60kWh → 60kW@10-11；L 60kWh → 40kW@10-11 + 20kW@11-12（受 H 挤压）。
	if _, err := svc.SubmitPlan(ctx, curReq("RH", 10, false, t10(), t12(), 60)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPlan(ctx, curReq("RL", 1, false, t10(), t12(), 60)); err != nil {
		t.Fatal(err)
	}

	ev := CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 50_000}
	plan, err := svc.PrepareCurtailment(ctx, ev)
	if err != nil {
		t.Fatalf("PrepareCurtailment: %v", err)
	}
	h := findAdjustment(t, plan, "RH")
	l := findAdjustment(t, plan, "RL")
	// 高优先级 H 在窗口内保住 50kW；低优先级 L 被完全挤出窗口。
	if allocPowerAt(h.NewAllocation, t10()) != 50_000 {
		t.Fatalf("H window power = %d, want 50000", allocPowerAt(h.NewAllocation, t10()))
	}
	if allocPowerAt(l.NewAllocation, t10()) != 0 {
		t.Fatalf("L window power = %d, want 0 (fully displaced)", allocPowerAt(l.NewAllocation, t10()))
	}
	// 两者全程仍交付各自 60kWh。
	if got := deliveredUWh(&Plan{Allocation: h.NewAllocation, MinEnergy: 60_000_000_000}); got != 60_000_000_000 {
		t.Fatalf("H delivered %d, want 60kWh", got)
	}
	if got := deliveredUWh(&Plan{Allocation: l.NewAllocation, MinEnergy: 60_000_000_000}); got != 60_000_000_000 {
		t.Fatalf("L delivered %d, want 60kWh", got)
	}

	if _, err := svc.ConfirmCurtailment(ctx, plan); err != nil {
		t.Fatalf("ConfirmCurtailment: %v", err)
	}
	occ, _ := svc.Occupancy(ctx, "S1", t10(), t12())
	// 窗口内总占用恰为降容容量 50kW，且只来自 H。
	if occ[0].ReservedW != 50_000 {
		t.Fatalf("h1 reserved=%d, want 50000", occ[0].ReservedW)
	}
	// 第二小时 H 补差 10kW + L 全部 60kW = 70kW。
	if occ[1].ReservedW != 70_000 {
		t.Fatalf("h2 reserved=%d, want 70000", occ[1].ReservedW)
	}
}

// 固定功率预约不可降低：固定预约原样保留，且不递增版本。
func TestCurtailmentNeverReducesFixedPlan(t *testing.T) {
	ctx := context.Background()
	svc := newCurtailService(t)
	if _, err := svc.SubmitPlan(ctx, curReq("RF", 0, true, t10(), t12(), 50)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPlan(ctx, curReq("RL", 1, false, t10(), t12(), 50)); err != nil {
		t.Fatal(err)
	}
	// 降容到 60kW：固定 50kW 必须保留，只剩 10kW 给 L（其缺额转移到第二小时）。
	ev := CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 60_000}
	plan, err := svc.PrepareCurtailment(ctx, ev)
	if err != nil {
		t.Fatalf("PrepareCurtailment: %v", err)
	}
	for _, a := range plan.Adjustments {
		if a.FixedPower {
			t.Fatalf("fixed plan %q must not appear as adjustable", a.RequestID)
		}
	}
	changed, err := svc.ConfirmCurtailment(ctx, plan)
	if err != nil {
		t.Fatalf("ConfirmCurtailment: %v", err)
	}
	for _, c := range changed {
		if c.RequestID == "RF" {
			t.Fatalf("fixed plan changed: %+v", c)
		}
	}
	fixed, _ := svc.GetPlan(ctx, "RF")
	if fixed.Revision != 1 {
		t.Fatalf("fixed plan revision=%d, want 1", fixed.Revision)
	}
	if allocPowerAt(fixed.Allocation, t10()) != 50_000 {
		t.Fatalf("fixed window power=%d, want 50000", allocPowerAt(fixed.Allocation, t10()))
	}
	l, _ := svc.GetPlan(ctx, "RL")
	if allocPowerAt(l.Allocation, t10()) != 10_000 || allocPowerAt(l.Allocation, t11()) != 40_000 {
		t.Fatalf("L alloc wrong: %+v", l.Allocation)
	}
}

// 降容后即便转移也无法满足某笔可调整预约的最低需求 → 容量错误，状态不变。
func TestCurtailmentInfeasibleBelowMinimumRejects(t *testing.T) {
	ctx := context.Background()
	svc := newCurtailService(t)
	// 固定 40kW 占窗口；L 仅在 10–11 在场、要 60kWh，无窗口外空间可转移。
	if _, err := svc.SubmitPlan(ctx, curReq("RF", 0, true, t10(), t11(), 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPlan(ctx, curReq("RL", 1, false, t10(), t11(), 60)); err != nil {
		t.Fatal(err)
	}
	ev := CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 50_000}
	if _, err := svc.PrepareCurtailment(ctx, ev); KindOf(err) != KindCapacity {
		t.Fatalf("want KindCapacity, got %v", err)
	}
	// 失败后站点配置与占用完全不变。
	occ, _ := svc.Occupancy(ctx, "S1", t10(), t11())
	if occ[0].CapacityW != 100_000 || occ[0].ReservedW != 100_000 {
		t.Fatalf("state changed after infeasible prepare: %+v", occ[0])
	}
}

// 固定/已抵达占用本身超过降容容量 → 无法成案。
func TestCurtailmentProtectedExceedsCapacityRejects(t *testing.T) {
	ctx := context.Background()
	svc := newCurtailService(t)
	if _, err := svc.SubmitPlan(ctx, curReq("RF", 0, true, t10(), t11(), 80)); err != nil {
		t.Fatal(err)
	}
	ev := CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 50_000}
	if _, err := svc.PrepareCurtailment(ctx, ev); KindOf(err) != KindCapacity {
		t.Fatalf("want KindCapacity, got %v", err)
	}
}

// 版本冲突：方案生成后修改、取消、新增预约都会使确认整体失败，原安排不被部分修改。
func TestCurtailmentStalePlanRejectedOnModifyCancelSubmit(t *testing.T) {
	ctx := context.Background()

	// 子用例 1：方案后修改
	t.Run("modify", func(t *testing.T) {
		svc := newCurtailService(t)
		l, _ := svc.SubmitPlan(ctx, curReq("RL", 1, false, t10(), t12(), 100))
		plan, err := svc.PrepareCurtailment(ctx, CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 60_000})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ModifyPlan(ctx, "RL", PlanModification{
			StationID: "S1", VehicleID: l.VehicleID, Arrival: l.Arrival, Departure: l.Departure,
			MinEnergyUWh: 90_000_000_000, MaxPowerW: 100_000, Priority: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ConfirmCurtailment(ctx, plan); KindOf(err) != KindConflict {
			t.Fatalf("want KindConflict, got %v", err)
		}
		// 原安排未被部分修改：站点仍 100kW，L 为修改后的 90kWh 版本。
		occ, _ := svc.Occupancy(ctx, "S1", t10(), t11())
		if occ[0].CapacityW != 100_000 {
			t.Fatalf("capacity changed after failed confirm: %+v", occ[0])
		}
		got, _ := svc.GetPlan(ctx, "RL")
		if got.MinEnergy != 90_000_000_000 || got.Revision != 2 {
			t.Fatalf("plan should retain modify result, got %+v", got)
		}
	})

	// 子用例 2：方案后取消
	t.Run("cancel", func(t *testing.T) {
		svc := newCurtailService(t)
		if _, err := svc.SubmitPlan(ctx, curReq("RL", 1, false, t10(), t12(), 100)); err != nil {
			t.Fatal(err)
		}
		plan, err := svc.PrepareCurtailment(ctx, CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 60_000})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.CancelPlan(ctx, "RL"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ConfirmCurtailment(ctx, plan); KindOf(err) != KindConflict {
			t.Fatalf("want KindConflict, got %v", err)
		}
		occ, _ := svc.Occupancy(ctx, "S1", t10(), t12())
		for _, o := range occ {
			if o.CapacityW != 100_000 || o.ReservedW != 0 {
				t.Fatalf("state partially changed after failed confirm: %+v", o)
			}
		}
	})

	// 子用例 3：方案后窗口内新增预约
	t.Run("submit", func(t *testing.T) {
		svc := newCurtailService(t)
		if _, err := svc.SubmitPlan(ctx, curReq("RL", 1, false, t10(), t12(), 50)); err != nil {
			t.Fatal(err)
		}
		plan, err := svc.PrepareCurtailment(ctx, CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 60_000})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.SubmitPlan(ctx, curReq("RN", 1, false, t11(), t12(), 10)); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ConfirmCurtailment(ctx, plan); KindOf(err) != KindConflict {
			t.Fatalf("want KindConflict, got %v", err)
		}
	})

	// 子用例 4：重新生成方案后确认成功。
	t.Run("regenerate", func(t *testing.T) {
		svc := newCurtailService(t)
		if _, err := svc.SubmitPlan(ctx, curReq("RL", 1, false, t10(), t12(), 100)); err != nil {
			t.Fatal(err)
		}
		old, err := svc.PrepareCurtailment(ctx, CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 60_000})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.CancelPlan(ctx, "RL"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ConfirmCurtailment(ctx, old); KindOf(err) != KindConflict {
			t.Fatalf("stale plan should conflict, got %v", err)
		}
		// 取消后窗口内已无可调整预约：重新生成空调整方案，确认只降容量。
		fresh, err := svc.PrepareCurtailment(ctx, CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 60_000})
		if err != nil {
			t.Fatal(err)
		}
		if len(fresh.Adjustments) != 0 {
			t.Fatalf("want no adjustments, got %d", len(fresh.Adjustments))
		}
		if _, err := svc.ConfirmCurtailment(ctx, fresh); err != nil {
			t.Fatalf("fresh confirm: %v", err)
		}
		occ, _ := svc.Occupancy(ctx, "S1", t10(), t11())
		if occ[0].CapacityW != 60_000 || occ[0].ReservedW != 0 {
			t.Fatalf("capacity not lowered: %+v", occ[0])
		}
	})
}

// 确认幂等视角：同一份方案不能重复确认（第二次因版本已递增而冲突）。
func TestCurtailmentCannotConfirmTwice(t *testing.T) {
	ctx := context.Background()
	svc := newCurtailService(t)
	if _, err := svc.SubmitPlan(ctx, curReq("RL", 1, false, t10(), t12(), 100)); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.PrepareCurtailment(ctx, CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 60_000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmCurtailment(ctx, plan); err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	if _, err := svc.ConfirmCurtailment(ctx, plan); KindOf(err) != KindConflict {
		t.Fatalf("second confirm want KindConflict, got %v", err)
	}
}

// 并发：确认与修改/取消竞争时，要么确认成功、要么冲突，绝不部分修改、不崩溃。
func TestCurtailmentConcurrentConfirmVsMutations(t *testing.T) {
	ctx := context.Background()
	for round := 0; round < 30; round++ {
		store := NewMemoryStore()
		now := tmust(nil, "2026-01-01T09:00:00")
		svc, err := NewService(store, WithClock(func() time.Time { return now }))
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 4; i++ {
			id := fmt.Sprintf("R%d", i)
			if _, err := svc.SubmitPlan(ctx, curReq(id, int32(i), false, t10(), t12(), 40)); err != nil {
				t.Fatal(err)
			}
		}
		plan, err := svc.PrepareCurtailment(ctx, CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 80_000})
		if err != nil {
			t.Fatalf("round %d prepare: %v", round, err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = svc.ConfirmCurtailment(ctx, plan)
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = svc.CancelPlan(ctx, "R0")
		}()
		close(start)
		wg.Wait()

		// 不变量：无论谁先完成，逐时段占用都不超过当时容量。
		occ, _ := svc.Occupancy(ctx, "S1", t10(), t12())
		for _, o := range occ {
			if o.ReservedW > o.CapacityW {
				t.Fatalf("round %d over capacity: %+v", round, o)
			}
			if o.AvailableW != o.CapacityW-o.ReservedW {
				t.Fatalf("round %d availability mismatch: %+v", round, o)
			}
		}
	}
}

// 降容确认后的容量与预约调整在重启后完整恢复。
func TestCurtailmentPersistenceAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := dir + "/state.json"
	now := tmust(nil, "2026-01-01T09:00:00")
	open := func() *Service {
		svc, err := NewService(NewFileStore(path), WithClock(func() time.Time { return now }))
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}

	svc := open()
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPlan(ctx, curReq("RL", 1, false, t10(), t12(), 100)); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.PrepareCurtailment(ctx, CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: 60_000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmCurtailment(ctx, plan); err != nil {
		t.Fatal(err)
	}

	svc2 := open()
	p, err := svc2.GetPlan(ctx, "RL")
	if err != nil {
		t.Fatal(err)
	}
	if p.Revision != 2 || allocPowerAt(p.Allocation, t10()) != 60_000 || allocPowerAt(p.Allocation, t11()) != 40_000 {
		t.Fatalf("plan not restored correctly after restart: %+v", p.Allocation)
	}
	occ, _ := svc2.Occupancy(ctx, "S1", t10(), t12())
	if occ[0].CapacityW != 60_000 || occ[0].ReservedW != 60_000 {
		t.Fatalf("curtailed capacity not restored: %+v", occ[0])
	}
}

// 参数校验：空站点、反向窗口、负功率。
func TestCurtailmentEventValidation(t *testing.T) {
	ctx := context.Background()
	svc := newCurtailService(t)
	cases := []struct {
		name string
		ev   CurtailmentEvent
		kind ErrorKind
	}{
		{"empty-station", CurtailmentEvent{Start: t10(), End: t11(), PowerW: 1}, KindParameter},
		{"unknown-station", CurtailmentEvent{StationID: "NOPE", Start: t10(), End: t11(), PowerW: 1}, KindParameter},
		{"reversed-window", CurtailmentEvent{StationID: "S1", Start: t11(), End: t10(), PowerW: 1}, KindTime},
		{"negative-power", CurtailmentEvent{StationID: "S1", Start: t10(), End: t11(), PowerW: -1}, KindParameter},
	}
	for _, c := range cases {
		if _, err := svc.PrepareCurtailment(ctx, c.ev); KindOf(err) != c.kind {
			t.Fatalf("%s: want %s, got %v", c.name, c.kind, err)
		}
	}
}
