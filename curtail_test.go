package fleetcharging

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// curtailFixture 构造常用的两小时 100kW 场景，三笔预约初始都占在第一小时：
//   - RF 固定预约：10:00–11:00 30kW（30kWh）
//   - RA 高优先级(10)：10:00–12:00 40kWh，初始占第一小时 40kW
//   - RB 低优先级(1)：10:00–12:00 30kWh，初始占第一小时 30kW
//
// 第一小时初始占用 30+40+30=100kW；第二小时空闲。
func curtailFixture(t *testing.T, now time.Time) *Service {
	t.Helper()
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(context.Background(), twoHourStation()); err != nil {
		t.Fatal(err)
	}
	mk := func(id string, prio int, fixed bool, a, b string, kwh int64) ChargeRequest {
		return ChargeRequest{
			RequestID: id, StationID: "S1", VehicleID: "V-" + id,
			Arrival:      tmust(t, a),
			Departure:    tmust(t, b),
			MinEnergyUWh: kwh * 1_000_000_000,
			MaxPowerW:    100_000,
			Priority:     prio,
			Fixed:        fixed,
		}
	}
	ctx := context.Background()
	if _, err := svc.SubmitPlan(ctx, mk("RF", 0, true, "2026-01-01T10:00:00", "2026-01-01T11:00:00", 30)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPlan(ctx, mk("RA", 10, false, "2026-01-01T10:00:00", "2026-01-01T12:00:00", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPlan(ctx, mk("RB", 1, false, "2026-01-01T10:00:00", "2026-01-01T12:00:00", 30)); err != nil {
		t.Fatal(err)
	}
	return svc
}

func hour1Event(power int64) CapacityEvent {
	return CapacityEvent{
		EventID:   "EV-1",
		StationID: "S1",
		Start:     tmust(nil, "2026-01-01T10:00:00"),
		End:       tmust(nil, "2026-01-01T11:00:00"),
		PowerW:    power,
	}
}

func TestPrepareCurtailmentDoesNotChangeState(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc := curtailFixture(t, now)

	if _, err := svc.PrepareCurtailment(ctx, hour1Event(80_000)); err != nil {
		t.Fatalf("PrepareCurtailment: %v", err)
	}
	// 方案生成后：站点容量、各预约分配与版本都必须原封不动。
	occ, err := svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T12:00:00"))
	if err != nil {
		t.Fatal(err)
	}
	if occ[0].CapacityW != 100_000 || occ[0].ReservedW != 100_000 {
		t.Fatalf("prepare mutated capacity/occupancy: %+v", occ[0])
	}
	pa, _ := svc.GetPlan(ctx, "RA")
	if pa.Revision != 1 || len(pa.Allocation) != 1 || pa.Allocation[0].PowerW != 40_000 {
		t.Fatalf("prepare mutated plan RA: %+v", pa)
	}
}

func TestConfirmCurtailmentAppliesPriorityFixedAndConservation(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc := curtailFixture(t, now)

	plan, err := svc.PrepareCurtailment(ctx, hour1Event(80_000))
	if err != nil {
		t.Fatalf("PrepareCurtailment: %v", err)
	}

	// 方案明细：固定 RF 不变；高优先级 RA 第一小时保留 40kW（能量已足）；
	// 低优先级 RB 被挤出 20kW，改为第一小时 10kW + 第二小时 20kW，总能量不变。
	want := map[string][]struct {
		t0, t1 string
		power  int64
	}{
		"RF": {{"2026-01-01T10:00:00", "2026-01-01T11:00:00", 30_000}},
		"RA": {{"2026-01-01T10:00:00", "2026-01-01T11:00:00", 40_000}},
		"RB": {
			{"2026-01-01T10:00:00", "2026-01-01T11:00:00", 10_000},
			{"2026-01-01T11:00:00", "2026-01-01T12:00:00", 20_000},
		},
	}
	if len(plan.Items) != 3 {
		t.Fatalf("items=%d want 3: %+v", len(plan.Items), plan.Items)
	}
	for _, it := range plan.Items {
		w := want[it.RequestID]
		if len(it.Allocation) != len(w) {
			t.Fatalf("%s alloc=%+v want %+v", it.RequestID, it.Allocation, w)
		}
		for i, a := range it.Allocation {
			if !a.Start.Equal(tmust(nil, w[i].t0)) || !a.End.Equal(tmust(nil, w[i].t1)) || a.PowerW != w[i].power {
				t.Fatalf("%s alloc[%d]=%+v want %+v", it.RequestID, i, a, w[i])
			}
		}
		if it.FromRevision != 1 {
			t.Fatalf("%s fromRev=%d want 1", it.RequestID, it.FromRevision)
		}
	}
	// 方案中的新配置：第一小时 80kW，第二小时维持 100kW。
	if len(plan.Segments) != 2 || plan.Segments[0].PowerW != 80_000 || plan.Segments[1].PowerW != 100_000 {
		t.Fatalf("plan segments wrong: %+v", plan.Segments)
	}

	results, err := svc.ConfirmCurtailment(ctx, plan.Token)
	if err != nil {
		t.Fatalf("ConfirmCurtailment: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results=%d want 3", len(results))
	}
	for _, r := range results {
		if r.FromRevision != 1 || r.ToRevision != 2 {
			t.Fatalf("result %+v revisions wrong", r)
		}
	}

	// 确认后逐笔核对：每笔交付能量不低于最低需求。
	mins := map[string]int64{"RF": 30_000_000_000, "RA": 40_000_000_000, "RB": 30_000_000_000}
	for id, min := range mins {
		p, _ := svc.GetPlan(ctx, id)
		if p.Revision != 2 {
			t.Fatalf("%s revision=%d want 2", id, p.Revision)
		}
		if got := deliveredUWh(p); got < min {
			t.Fatalf("%s delivered %d < min %d", id, got, min)
		}
	}

	// 守恒：第一小时释放 20kW（100→80）后被 RF30+RA40+RB10=80 重新占满；
	// 第二小时容量不变，仅承接被挤出的 RB20kW，空闲恰为 80kW。
	occ, _ := svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T12:00:00"))
	if len(occ) != 2 {
		t.Fatalf("occ rows=%d: %+v", len(occ), occ)
	}
	if occ[0].CapacityW != 80_000 || occ[0].ReservedW != 80_000 || occ[0].AvailableW != 0 {
		t.Fatalf("h1 after curtail: %+v", occ[0])
	}
	if occ[1].CapacityW != 100_000 || occ[1].ReservedW != 20_000 || occ[1].AvailableW != 80_000 {
		t.Fatalf("h2 after curtail: %+v", occ[1])
	}

	// 同一方案不能重复确认。
	if _, err := svc.ConfirmCurtailment(ctx, plan.Token); KindOf(err) != KindState {
		t.Fatalf("double confirm: want state, got %v", err)
	}
}

func TestPrepareCurtailmentFixedOverloadRejectsAndKeepsState(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	req := ChargeRequest{
		RequestID: "RFX", StationID: "S1", VehicleID: "V",
		Arrival: tmust(nil, "2026-01-01T10:00:00"), Departure: tmust(nil, "2026-01-01T11:00:00"),
		MinEnergyUWh: 90_000_000_000, MaxPowerW: 100_000, Fixed: true,
	}
	if _, err := svc.SubmitPlan(ctx, req); err != nil {
		t.Fatal(err)
	}
	// 降到 80kW：固定预约独占 90kW > 80kW，方案无法生成。
	plan, err := svc.PrepareCurtailment(ctx, hour1Event(80_000))
	if KindOf(err) != KindCapacity {
		t.Fatalf("want capacity, got plan=%+v err=%v", plan, err)
	}
	// 状态不变。
	p, _ := svc.GetPlan(ctx, "RFX")
	if p.Allocation[0].PowerW != 90_000 || p.Revision != 1 {
		t.Fatalf("state changed after failed prepare: %+v", p)
	}
	occ, _ := svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T11:00:00"))
	if occ[0].CapacityW != 100_000 || occ[0].ReservedW != 90_000 {
		t.Fatalf("capacity/occupancy changed: %+v", occ[0])
	}
}

func TestPrepareCurtailmentMinimumEnergyCannotBeMetRejects(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	// 预约窗口只有第一小时，要 80kWh；第一小时降到 50kW，窗口外无法补能 → 不可行。
	req := ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "V",
		Arrival: tmust(nil, "2026-01-01T10:00:00"), Departure: tmust(nil, "2026-01-01T11:00:00"),
		MinEnergyUWh: 80_000_000_000, MaxPowerW: 100_000, Priority: 5,
	}
	if _, err := svc.SubmitPlan(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PrepareCurtailment(ctx, hour1Event(50_000)); KindOf(err) != KindCapacity {
		t.Fatalf("want capacity, got %v", err)
	}
	p, _ := svc.GetPlan(ctx, "RA")
	if p.Allocation[0].PowerW != 80_000 {
		t.Fatalf("plan allocation changed: %+v", p.Allocation)
	}
}

func TestCurtailmentEventValidation(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc := curtailFixture(t, now)

	// 起点不早于终点。
	bad := hour1Event(80_000)
	bad.End = bad.Start
	if _, err := svc.PrepareCurtailment(ctx, bad); KindOf(err) != KindTime {
		t.Fatalf("reversed event: %v", err)
	}
	// 新功率不低于原容量：不是“下降”。
	if _, err := svc.PrepareCurtailment(ctx, hour1Event(100_000)); KindOf(err) != KindParameter {
		t.Fatalf("equal power: want parameter, got %v", err)
	}
	if _, err := svc.PrepareCurtailment(ctx, hour1Event(120_000)); KindOf(err) != KindParameter {
		t.Fatalf("higher power: want parameter, got %v", err)
	}
	// 窗口越出已配置供电时段。
	out := hour1Event(80_000)
	out.End = tmust(nil, "2026-01-01T12:30:00")
	if _, err := svc.PrepareCurtailment(ctx, out); KindOf(err) != KindTime {
		t.Fatalf("window outside config: want time, got %v", err)
	}
	// 起始时间已过。
	past := hour1Event(80_000)
	past.Start = tmust(nil, "2026-01-01T09:30:00")
	past.End = tmust(nil, "2026-01-01T10:00:00")
	if _, err := svc.PrepareCurtailment(ctx, past); KindOf(err) != KindTime {
		t.Fatalf("past event: want time, got %v", err)
	}
	// 未知站点 / 空 ID。
	unk := hour1Event(80_000)
	unk.StationID = "NOPE"
	if _, err := svc.PrepareCurtailment(ctx, unk); KindOf(err) != KindParameter {
		t.Fatalf("unknown station: %v", err)
	}
	unk = hour1Event(80_000)
	unk.EventID = ""
	if _, err := svc.PrepareCurtailment(ctx, unk); KindOf(err) != KindParameter {
		t.Fatalf("empty event id: %v", err)
	}
}

func TestConfirmCurtailmentConflictAfterModify(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc := curtailFixture(t, now)
	plan, err := svc.PrepareCurtailment(ctx, hour1Event(80_000))
	if err != nil {
		t.Fatal(err)
	}

	// 运营方在确认前扩大了 RA 的需求（40 → 60kWh，版本 1 → 2）：
	// 第一小时仅剩 40kW 空闲，另外 20kWh 排到第二小时，修改本身可行。
	pa, _ := svc.GetPlan(ctx, "RA")
	if _, err := svc.ModifyPlan(ctx, "RA", PlanModification{
		StationID: "S1", VehicleID: pa.VehicleID,
		Arrival: pa.Arrival, Departure: pa.Departure,
		MinEnergyUWh: 60_000_000_000, MaxPowerW: 100_000,
		Priority: pa.Priority,
	}); err != nil {
		t.Fatal(err)
	}

	_, cerr := svc.ConfirmCurtailment(ctx, plan.Token)
	if KindOf(cerr) != KindConflict {
		t.Fatalf("want conflict, got %v", cerr)
	}
	if !errors.Is(cerr, ErrConflict) {
		t.Fatalf("errors.Is(ErrConflict) failed: %v", cerr)
	}

	// 原安排未被部分修改：容量仍为 100kW；RA 是修改后的分配（h1 40 + h2 20），
	// RF/RB 版本仍为 1。
	occ, _ := svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T11:00:00"))
	if occ[0].CapacityW != 100_000 {
		t.Fatalf("capacity changed after failed confirm: %+v", occ[0])
	}
	pa2, _ := svc.GetPlan(ctx, "RA")
	if pa2.Revision != 2 || pa2.Allocation[0].PowerW != 40_000 {
		t.Fatalf("RA unexpected: %+v", pa2)
	}
	pb, _ := svc.GetPlan(ctx, "RB")
	if pb.Revision != 1 {
		t.Fatalf("RB revision changed: %d", pb.Revision)
	}

	// 旧方案已作废；重新生成后可以确认。
	plan2, err := svc.PrepareCurtailment(ctx, hour1Event(80_000))
	if err != nil {
		t.Fatalf("reprepare: %v", err)
	}
	if _, err := svc.ConfirmCurtailment(ctx, plan2.Token); err != nil {
		t.Fatalf("confirm fresh plan: %v", err)
	}
	occ, _ = svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T11:00:00"))
	if occ[0].CapacityW != 80_000 || occ[0].ReservedW > 80_000 {
		t.Fatalf("fresh confirm wrong: %+v", occ[0])
	}
}

func TestConfirmCurtailmentConflictAfterCancel(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc := curtailFixture(t, now)
	plan, err := svc.PrepareCurtailment(ctx, hour1Event(80_000))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CancelPlan(ctx, "RB"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmCurtailment(ctx, plan.Token); KindOf(err) != KindConflict {
		t.Fatalf("want conflict after cancel, got %v", err)
	}
	// RB 的取消保留，但容量事件未落实。
	pb, _ := svc.GetPlan(ctx, "RB")
	if pb.Status != StatusCancelled {
		t.Fatalf("RB status=%s", pb.Status)
	}
	occ, _ := svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T11:00:00"), tmust(nil, "2026-01-01T12:00:00"))
	if occ[0].CapacityW != 100_000 {
		t.Fatalf("event applied despite conflict: %+v", occ[0])
	}
}

func TestConfirmCurtailmentConflictAfterNewSubmit(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc := curtailFixture(t, now)
	plan, err := svc.PrepareCurtailment(ctx, hour1Event(80_000))
	if err != nil {
		t.Fatal(err)
	}
	// 方案生成后第二小时出现新预约；即使它不落在事件窗口内，
	// 预约集合指纹变化也必须拒绝旧方案（确认后的重排可能已不可行）。
	if _, err := svc.SubmitPlan(ctx, ChargeRequest{
		RequestID: "RC", StationID: "S1", VehicleID: "V-RC",
		Arrival: tmust(nil, "2026-01-01T11:00:00"), Departure: tmust(nil, "2026-01-01T12:00:00"),
		MinEnergyUWh: 10_000_000_000, MaxPowerW: 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmCurtailment(ctx, plan.Token); KindOf(err) != KindConflict {
		t.Fatalf("want conflict after new submit, got %v", err)
	}
}

func TestConfirmCurtailmentConflictAfterReconfigure(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc := curtailFixture(t, now)
	plan, err := svc.PrepareCurtailment(ctx, hour1Event(80_000))
	if err != nil {
		t.Fatal(err)
	}
	// 方案生成后站点被整体重配（第二小时扩容到 120kW，不击穿既有占用，重配成功）：
	// 方案的基线配置指纹已变，确认必须拒绝。
	cfg := twoHourStation()
	cfg.Segments[1].PowerW = 120_000
	if err := svc.ConfigureStation(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmCurtailment(ctx, plan.Token); KindOf(err) != KindConflict {
		t.Fatalf("want conflict after reconfigure, got %v", err)
	}
}

// failOnceStore 包一层 MemoryStore，下一次 Save 按指令失败，用于验证事务回滚。
type failOnceStore struct {
	Store
	failNext bool
}

func (f *failOnceStore) Save(ctx context.Context, snap *snapshot) error {
	if f.failNext {
		f.failNext = false
		return errors.New("forced save failure")
	}
	return f.Store.Save(ctx, snap)
}

func TestConfirmCurtailmentRollsBackWhenPersistFails(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	mem := NewMemoryStore()
	svc, err := NewService(mem, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	mk := func(id string, prio int, a, b string, kwh int64) ChargeRequest {
		return ChargeRequest{
			RequestID: id, StationID: "S1", VehicleID: "V-" + id,
			Arrival: tmust(t, a), Departure: tmust(t, b),
			MinEnergyUWh: kwh * 1_000_000_000, MaxPowerW: 100_000, Priority: prio,
		}
	}
	if _, err := svc.SubmitPlan(ctx, mk("RA", 10, "2026-01-01T10:00:00", "2026-01-01T12:00:00", 70)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPlan(ctx, mk("RB", 1, "2026-01-01T10:00:00", "2026-01-01T12:00:00", 30)); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.PrepareCurtailment(ctx, hour1Event(80_000))
	if err != nil {
		t.Fatal(err)
	}

	// 让确认事务的那一次落库失败。
	failing := &failOnceStore{Store: mem, failNext: true}
	svc.store = failing
	if _, err := svc.ConfirmCurtailment(ctx, plan.Token); err == nil {
		t.Fatal("want persist error, got nil")
	}
	svc.store = mem

	// 内存态完全回滚：容量、预约版本与分配都不变。
	occ, _ := svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T11:00:00"))
	if occ[0].CapacityW != 100_000 || occ[0].ReservedW != 100_000 {
		t.Fatalf("state not rolled back: %+v", occ[0])
	}
	for _, id := range []string{"RA", "RB"} {
		p, _ := svc.GetPlan(ctx, id)
		if p.Revision != 1 {
			t.Fatalf("%s revision=%d after rollback", id, p.Revision)
		}
	}

	// 方案仍然可确认：重试成功。
	if _, err := svc.ConfirmCurtailment(ctx, plan.Token); err != nil {
		t.Fatalf("retry confirm: %v", err)
	}
	occ, _ = svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T11:00:00"))
	if occ[0].CapacityW != 80_000 || occ[0].ReservedW > 80_000 {
		t.Fatalf("retry confirm wrong: %+v", occ[0])
	}
}

func TestConfirmCurtailmentAllowedAfterArrival(t *testing.T) {
	ctx := context.Background()
	now := tmust(t, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPlan(ctx, ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "V",
		Arrival: tmust(nil, "2026-01-01T10:00:00"), Departure: tmust(nil, "2026-01-01T12:00:00"),
		MinEnergyUWh: 100_000_000_000, MaxPowerW: 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	// 第一小时降到 80kW：预约在场两小时，第一小时 80kW + 第二小时 100kW，
	// 总可获得能量充足，方案可行。
	plan, err := svc.PrepareCurtailment(ctx, hour1Event(80_000))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// 越过到达点：计划锁定为 arrived（版本号不变），临时降容仍应能落实。
	svc.now = func() time.Time { return tmust(nil, "2026-01-01T10:30:00") }
	results, err := svc.ConfirmCurtailment(ctx, plan.Token)
	if err != nil {
		t.Fatalf("confirm after arrival: %v", err)
	}
	if results[0].FromRevision != 1 || results[0].ToRevision != 2 {
		t.Fatalf("revisions: %+v", results)
	}
	p, _ := svc.GetPlan(ctx, "RA")
	if p.Status != StatusArrived {
		t.Fatalf("status=%s want arrived", p.Status)
	}
	if deliveredUWh(p) < 100_000_000_000 {
		t.Fatalf("delivered %d < 100kWh", deliveredUWh(p))
	}
}

func TestConfirmCurtailmentVsModifyRaceNeverPartial(t *testing.T) {
	ctx := context.Background()
	// 多轮随机交错：确认 vs 修改 RA。无论谁赢，都不能出现部分修改或超额。
	for round := 0; round < 50; round++ {
		now := tmust(nil, "2026-01-01T09:00:00")
		svc := curtailFixture(t, now)
		plan, err := svc.PrepareCurtailment(ctx, hour1Event(80_000))
		if err != nil {
			t.Fatalf("round %d prepare: %v", round, err)
		}
		pa, _ := svc.GetPlan(ctx, "RA")

		var wg sync.WaitGroup
		start := make(chan struct{})
		var confirmErr, modifyErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, confirmErr = svc.ConfirmCurtailment(ctx, plan.Token)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, modifyErr = svc.ModifyPlan(ctx, "RA", PlanModification{
				StationID: "S1", VehicleID: pa.VehicleID,
				Arrival: pa.Arrival, Departure: pa.Departure,
				MinEnergyUWh: 70_000_000_000, MaxPowerW: 100_000,
				Priority: 10,
			})
		}()
		close(start)
		wg.Wait()

		// 互斥结果：确认成功则修改必失败或在其后成功；反之亦然。
		// 关键不变量：任何时刻逐片占用不超容量，每笔有效预约能量达标，无部分落实。
		occ, qerr := svc.Occupancy(ctx, "S1",
			tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T12:00:00"))
		if qerr != nil {
			t.Fatalf("round %d occupancy: %v", round, qerr)
		}
		for _, o := range occ {
			if o.ReservedW > o.CapacityW {
				t.Fatalf("round %d overload: confirmErr=%v modifyErr=%v occ=%+v",
					round, confirmErr, modifyErr, o)
			}
			if o.ReservedW < 0 || o.AvailableW != o.CapacityW-o.ReservedW {
				t.Fatalf("round %d conservation broken: %+v", round, o)
			}
		}
		for _, id := range []string{"RF", "RA", "RB"} {
			p, _ := svc.GetPlan(ctx, id)
			if p.Status == StatusCancelled {
				continue
			}
			if got := deliveredUWh(p); got < p.MinEnergy {
				t.Fatalf("round %d %s delivered %d < min %d", round, id, got, p.MinEnergy)
			}
		}

		cfg := svc.snap.Stations["S1"]
		confirmed := cfg.Segments[0].PowerW == 80_000
		if confirmed && confirmErr != nil {
			t.Fatalf("round %d: capacity lowered despite confirm error %v", round, confirmErr)
		}
		if !confirmed && confirmErr == nil {
			t.Fatalf("round %d: confirm nil error but capacity not lowered", round)
		}
		if confirmed {
			// 确认若先于修改：RF/RB 版本至少为 2；方案是整单落实，不可能只动一笔。
			rf, _ := svc.GetPlan(ctx, "RF")
			rb, _ := svc.GetPlan(ctx, "RB")
			if rf.Revision < 2 || rb.Revision < 2 {
				t.Fatalf("round %d: partial confirm, RF rev=%d RB rev=%d",
					round, rf.Revision, rb.Revision)
			}
		}
	}
}

func TestPrepareCurtailmentPartialWindowAcrossSegments(t *testing.T) {
	ctx := context.Background()
	now := tmust(nil, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	// RA 跨两小时要 80kWh，初始贪心占第一小时 80kW。
	if _, err := svc.SubmitPlan(ctx, ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "V",
		Arrival: tmust(nil, "2026-01-01T10:00:00"), Departure: tmust(nil, "2026-01-01T12:00:00"),
		MinEnergyUWh: 80_000_000_000, MaxPowerW: 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	// 事件 10:30–11:30 跨两个配置时段，统一降到 50kW。
	ev := CapacityEvent{
		EventID: "EV-X", StationID: "S1",
		Start:  tmust(nil, "2026-01-01T10:30:00"),
		End:    tmust(nil, "2026-01-01T11:30:00"),
		PowerW: 50_000,
	}
	plan, err := svc.PrepareCurtailment(ctx, ev)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// 新配置应被切成三段：事件外保持 100kW，事件内 50kW。
	wantSegs := []struct {
		t0, t1 string
		power  int64
	}{
		{"2026-01-01T10:00:00", "2026-01-01T10:30:00", 100_000},
		{"2026-01-01T10:30:00", "2026-01-01T11:30:00", 50_000},
		{"2026-01-01T11:30:00", "2026-01-01T12:00:00", 100_000},
	}
	if len(plan.Segments) != len(wantSegs) {
		t.Fatalf("segments=%+v", plan.Segments)
	}
	for i, w := range wantSegs {
		g := plan.Segments[i]
		if !g.Start.Equal(tmust(nil, w.t0)) || !g.End.Equal(tmust(nil, w.t1)) || g.PowerW != w.power {
			t.Fatalf("seg %d = %+v want %+v", i, g, w)
		}
	}
	// RA 重排：10:00–10:30 满 100kW(50kWh)，10:30–11:00 50kW(25kWh)，
	// 还差 5kWh → 11:00–11:30 取 10kW，逐片不超额且总量达标。
	if _, err := svc.ConfirmCurtailment(ctx, plan.Token); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	p, _ := svc.GetPlan(ctx, "RA")
	if got := deliveredUWh(p); got < 80_000_000_000 {
		t.Fatalf("delivered %d < 80kWh", got)
	}
	occ, _ := svc.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T12:00:00"))
	for _, o := range occ {
		if o.ReservedW > o.CapacityW {
			t.Fatalf("overload after partial-window confirm: %+v", o)
		}
	}
}

func TestPrepareCurtailmentSamePriorityKeepsCreationOrder(t *testing.T) {
	ctx := context.Background()
	now := tmust(nil, "2026-01-01T09:00:00")
	svc, _ := newTestService(t, now)
	if err := svc.ConfigureStation(ctx, twoHourStation()); err != nil {
		t.Fatal(err)
	}
	mk := func(id string) ChargeRequest {
		return ChargeRequest{
			RequestID: id, StationID: "S1", VehicleID: "V-" + id,
			Arrival: tmust(nil, "2026-01-01T10:00:00"), Departure: tmust(nil, "2026-01-01T12:00:00"),
			MinEnergyUWh: 40_000_000_000, MaxPowerW: 100_000, Priority: 5,
		}
	}
	if _, err := svc.SubmitPlan(ctx, mk("RA")); err != nil { // 先创建
		t.Fatal(err)
	}
	if _, err := svc.SubmitPlan(ctx, mk("RB")); err != nil { // 后创建
		t.Fatal(err)
	}
	plan, err := svc.PrepareCurtailment(ctx, hour1Event(60_000))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	byID := map[string]AdjustmentItem{}
	for _, it := range plan.Items {
		byID[it.RequestID] = it
	}
	ra, rb := byID["RA"], byID["RB"]
	// RA 先创建 → 第一小时 40kW 不动；RB 被挤为 h1 20kW + h2 20kW。
	if len(ra.Allocation) != 1 || ra.Allocation[0].PowerW != 40_000 ||
		!ra.Allocation[0].Start.Equal(tmust(nil, "2026-01-01T10:00:00")) {
		t.Fatalf("RA allocation: %+v", ra.Allocation)
	}
	if len(rb.Allocation) != 1 || rb.Allocation[0].PowerW != 20_000 ||
		!rb.Allocation[0].Start.Equal(tmust(nil, "2026-01-01T10:00:00")) ||
		!rb.Allocation[0].End.Equal(tmust(nil, "2026-01-01T12:00:00")) {
		t.Fatalf("RB allocation: %+v", rb.Allocation)
	}
	if got := deliveredUWh(&Plan{Allocation: rb.Allocation}); got != 40_000_000_000 {
		t.Fatalf("RB delivered %d want 40kWh", got)
	}
}

func TestCurtailmentPersistedAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := dir + "/state.json"
	now := tmust(nil, "2026-01-01T09:00:00")
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
	if _, err := svc.SubmitPlan(ctx, ChargeRequest{
		RequestID: "RA", StationID: "S1", VehicleID: "V",
		Arrival: tmust(nil, "2026-01-01T10:00:00"), Departure: tmust(nil, "2026-01-01T12:00:00"),
		MinEnergyUWh: 90_000_000_000, MaxPowerW: 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.PrepareCurtailment(ctx, hour1Event(80_000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmCurtailment(ctx, plan.Token); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	svc2 := open()
	p, err := svc2.GetPlan(ctx, "RA")
	if err != nil {
		t.Fatalf("after restart: %v", err)
	}
	if p.Revision != 2 || deliveredUWh(p) < 90_000_000_000 {
		t.Fatalf("plan after restart: %+v", p)
	}
	occ, _ := svc2.Occupancy(ctx, "S1",
		tmust(nil, "2026-01-01T10:00:00"), tmust(nil, "2026-01-01T11:00:00"))
	if occ[0].CapacityW != 80_000 {
		t.Fatalf("curtailed capacity not persisted: %+v", occ[0])
	}
	// 待确认方案是内存态：重启后旧 token 无法再确认（KindState）。
	if _, err := svc2.ConfirmCurtailment(ctx, plan.Token); KindOf(err) != KindState {
		t.Fatalf("stale token after restart: want state, got %v", err)
	}
}
