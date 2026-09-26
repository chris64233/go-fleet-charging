package fleetcharging

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var testBase = time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)

// fakeClock 可手动推进的时钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestService 配置 4 个时段，每个 1 小时、容量 1000W（1_000_000 mW）。
func newTestService(t *testing.T, clock *fakeClock) *Service {
	t.Helper()
	svc, err := NewService(&MemoryStore{}, WithClock(clock.now))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	var slots []Slot
	for i := 0; i < 4; i++ {
		slots = append(slots, Slot{
			Start:    testBase.Add(time.Duration(i) * time.Hour),
			End:      testBase.Add(time.Duration(i+1) * time.Hour),
			Capacity: 1_000_000, // 1000 W
		})
	}
	if err := svc.ConfigureSlots(slots); err != nil {
		t.Fatalf("ConfigureSlots: %v", err)
	}
	return svc
}

func req(id, vehicle string, arrival, departure time.Time, minEnergy Millijoules, maxPower Milliwatts) Request {
	return Request{
		ExternalID: id,
		VehicleID:  vehicle,
		Arrival:    arrival,
		Departure:  departure,
		MinEnergy:  minEnergy,
		MaxPower:   maxPower,
	}
}

func TestConfigureSlotsValidation(t *testing.T) {
	svc, err := NewService(&MemoryStore{})
	if err != nil {
		t.Fatal(err)
	}
	base := testBase

	// 重叠时段
	err = svc.ConfigureSlots([]Slot{
		{Start: base, End: base.Add(2 * time.Hour), Capacity: 1},
		{Start: base.Add(time.Hour), End: base.Add(3 * time.Hour), Capacity: 1},
	})
	if !IsKind(err, KindInvalidParam) {
		t.Fatalf("overlapping slots: want param error, got %v", err)
	}

	// 负容量
	err = svc.ConfigureSlots([]Slot{{Start: base, End: base.Add(time.Hour), Capacity: -1}})
	if !IsKind(err, KindInvalidParam) {
		t.Fatalf("negative capacity: want param error, got %v", err)
	}

	// 非整秒对齐
	err = svc.ConfigureSlots([]Slot{{Start: base, End: base.Add(time.Hour + time.Millisecond), Capacity: 1}})
	if !IsKind(err, KindInvalidParam) {
		t.Fatalf("unaligned slot: want param error, got %v", err)
	}

	// 乱序输入会被排序后接受
	err = svc.ConfigureSlots([]Slot{
		{Start: base.Add(time.Hour), End: base.Add(2 * time.Hour), Capacity: 1},
		{Start: base, End: base.Add(time.Hour), Capacity: 1},
	})
	if err != nil {
		t.Fatalf("unsorted slots should be accepted: %v", err)
	}
}

func TestSubmitAndEnergyExactness(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	svc := newTestService(t, clock)

	// 跨三个时段（00:30–02:30），每时段上限 500W。
	r := req("r1", "v1",
		testBase.Add(30*time.Minute), testBase.Add(2*time.Hour+30*time.Minute),
		3_000_000_000, 500_000)
	p, err := svc.Submit(r)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if p.Energy < r.MinEnergy {
		t.Fatalf("energy %d < min %d", p.Energy, r.MinEnergy)
	}
	if len(p.Allocations) != 3 {
		t.Fatalf("want 3 allocations, got %d", len(p.Allocations))
	}
	// 精确核算每段能量：功率 × 整秒数
	var total Millijoules
	for _, a := range p.Allocations {
		secs := int64(a.End.Sub(a.Start) / time.Second)
		if got := a.Energy(); got != Millijoules(int64(a.Power)*secs) {
			t.Fatalf("allocation energy %d != power*secs %d", got, int64(a.Power)*secs)
		}
		total += a.Energy()
	}
	if total != p.Energy {
		t.Fatalf("plan energy %d != sum of allocations %d", p.Energy, total)
	}
}

func TestSubmitIntervalLeftClosedRightOpen(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	svc := newTestService(t, clock)

	// 到达恰好等于时段起点、离开恰好等于下一时段起点：
	// 只占用第一个时段。
	r := req("r1", "v1",
		testBase.Add(time.Hour), testBase.Add(2*time.Hour),
		1_000_000, 100_000)
	p, err := svc.Submit(r)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(p.Allocations) != 1 {
		t.Fatalf("want 1 allocation, got %d", len(p.Allocations))
	}
	a := p.Allocations[0]
	if !a.Start.Equal(testBase.Add(time.Hour)) || !a.End.Equal(testBase.Add(2*time.Hour)) {
		t.Fatalf("allocation interval [%s, %s), want exactly slot 1", a.Start, a.End)
	}
}

func TestSubmitCapacityExceededRejectsWholePlan(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	svc := newTestService(t, clock)

	// 第一个计划占满时段 0 的全部容量。
	r1 := req("r1", "v1", testBase, testBase.Add(time.Hour), 3_600_000_000, 1_000_000)
	if _, err := svc.Submit(r1); err != nil {
		t.Fatalf("Submit r1: %v", err)
	}

	// 第二个计划还需要同一时段的容量 -> 容量错误。
	r2 := req("r2", "v2", testBase, testBase.Add(time.Hour), 1_000_000, 1_000_000)
	if _, err := svc.Submit(r2); !IsKind(err, KindCapacity) {
		t.Fatalf("want capacity error, got %v", err)
	}

	// 不能留下部分占用。
	occ, err := svc.Occupancy(testBase, testBase.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if occ[0].Allocated != 1_000_000 || occ[0].Available != 0 {
		t.Fatalf("occupancy leaked: allocated=%d available=%d", occ[0].Allocated, occ[0].Available)
	}
}

func TestSubmitPartialSpanRejectedWithoutPartialHold(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	svc := newTestService(t, clock)

	// 占满时段 1。
	r1 := req("r1", "v1", testBase.Add(time.Hour), testBase.Add(2*time.Hour), 3_600_000_000, 1_000_000)
	if _, err := svc.Submit(r1); err != nil {
		t.Fatalf("Submit r1: %v", err)
	}
	// 跨时段 0 和 1 的请求：需要的能量超过时段 0 单独可提供的量
	// （时段 0 最多 1000W×3600s = 3.6e9 mJ），而时段 1 已满 -> 整体拒绝，
	// 时段 0 不得被部分占用。
	r2 := req("r2", "v2", testBase, testBase.Add(2*time.Hour), 5_000_000_000, 1_000_000)
	if _, err := svc.Submit(r2); !IsKind(err, KindCapacity) {
		t.Fatalf("want capacity error, got %v", err)
	}
	occ, _ := svc.Occupancy(testBase, testBase.Add(2*time.Hour))
	if occ[0].Allocated != 0 {
		t.Fatalf("slot 0 leaked allocation: %d", occ[0].Allocated)
	}
	if occ[1].Allocated != 1_000_000 {
		t.Fatalf("slot 1 allocated = %d, want 1000000", occ[1].Allocated)
	}
}

func TestSubmitValidationErrors(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	svc := newTestService(t, clock)
	future := testBase.Add(time.Hour)

	cases := []struct {
		name string
		req  Request
		kind Kind
	}{
		{"empty external id", req("", "v", future, future.Add(time.Hour), 1, 1), KindInvalidParam},
		{"empty vehicle", req("x", "", future, future.Add(time.Hour), 1, 1), KindInvalidParam},
		{"departure before arrival", req("x", "v", future, future.Add(-time.Hour), 1, 1), KindInvalidParam},
		{"zero min energy", req("x", "v", future, future.Add(time.Hour), 0, 1), KindInvalidParam},
		{"zero max power", req("x", "v", future, future.Add(time.Hour), 1, 0), KindInvalidParam},
		{"arrival in past", req("x", "v", testBase.Add(-2*time.Hour), future, 1, 1), KindTime},
		{"unaligned arrival", req("x", "v", future.Add(time.Millisecond), future.Add(time.Hour), 1, 1), KindInvalidParam},
		{"uncovered window", req("x", "v", testBase.Add(100*time.Hour), testBase.Add(101*time.Hour), 1, 1), KindInvalidParam},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Submit(tc.req); !IsKind(err, tc.kind) {
				t.Fatalf("want kind %v, got %v", tc.kind, err)
			}
		})
	}
}

func TestIdempotency(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	svc := newTestService(t, clock)

	r := req("same-id", "v1", testBase, testBase.Add(time.Hour), 1_000_000, 100_000)
	p1, err := svc.Submit(r)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// 同号同内容 -> 返回原计划，不新增占用。
	p2, err := svc.Submit(r)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if p2.ID != p1.ID {
		t.Fatalf("replay returned different plan %s, want %s", p2.ID, p1.ID)
	}
	occ, _ := svc.Occupancy(testBase, testBase.Add(time.Hour))
	if occ[0].Allocated != p1.Allocations[0].Power {
		t.Fatalf("replay doubled allocation: %d", occ[0].Allocated)
	}

	// 同号不同内容 -> 幂等冲突。
	r2 := r
	r2.MinEnergy *= 2
	if _, err := svc.Submit(r2); !IsKind(err, KindIdempotency) {
		t.Fatalf("want idempotency conflict, got %v", err)
	}
}

func TestConcurrentSubmitOnlyFeasibleCombinationCommits(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	svc := newTestService(t, clock)

	// 时段容量 1000W，每个请求要 600W：最多 1 个成功。
	const n = 16
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := req(fmt.Sprintf("r%d", i), fmt.Sprintf("v%d", i),
				testBase, testBase.Add(time.Hour), 2_160_000_000, 600_000)
			_, results[i] = svc.Submit(r)
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else if !IsKind(err, KindCapacity) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("succeeded = %d, want exactly 1", succeeded)
	}
	occ, _ := svc.Occupancy(testBase, testBase.Add(time.Hour))
	if occ[0].Allocated > occ[0].Capacity {
		t.Fatalf("capacity violated: allocated %d > %d", occ[0].Allocated, occ[0].Capacity)
	}
}

func TestModifyAtomicReplace(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	svc := newTestService(t, clock)

	r := req("r1", "v1", testBase, testBase.Add(time.Hour), 1_000_000_000, 1_000_000)
	p, err := svc.Submit(r)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// 修改到时段 1：旧占用释放、新占用生效。
	upd := UpdateRequest{
		Arrival:   testBase.Add(time.Hour),
		Departure: testBase.Add(2 * time.Hour),
		MinEnergy: 2_000_000_000,
		MaxPower:  1_000_000,
	}
	p2, err := svc.Modify(p.ID, upd)
	if err != nil {
		t.Fatalf("Modify: %v", err)
	}
	if p2.Energy < upd.MinEnergy {
		t.Fatalf("modified plan energy %d < min %d", p2.Energy, upd.MinEnergy)
	}
	occ, _ := svc.Occupancy(testBase, testBase.Add(2*time.Hour))
	if occ[0].Allocated != 0 {
		t.Fatalf("old allocation not released: slot0=%d", occ[0].Allocated)
	}
	if occ[1].Allocated == 0 {
		t.Fatalf("new allocation not applied")
	}

	// 修改不可行（能量超过单时段可供应量）-> 原计划保持不变。
	bad := UpdateRequest{
		Arrival:   testBase.Add(time.Hour),
		Departure: testBase.Add(2 * time.Hour),
		MinEnergy: 999_000_000_000,
		MaxPower:  1_000_000,
	}
	if _, err := svc.Modify(p.ID, bad); !IsKind(err, KindCapacity) {
		t.Fatalf("want capacity error, got %v", err)
	}
	cur, _ := svc.GetPlan(p.ID)
	if cur.Energy != p2.Energy {
		t.Fatalf("failed modify changed plan: energy %d -> %d", p2.Energy, cur.Energy)
	}
}

func TestModifyAndCancelAfterArrival(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	svc := newTestService(t, clock)

	r := req("r1", "v1", testBase, testBase.Add(time.Hour), 1_000_000, 100_000)
	p, err := svc.Submit(r)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// 时钟越过到达点。
	clock.set(testBase.Add(time.Second))

	upd := UpdateRequest{Arrival: testBase.Add(time.Hour), Departure: testBase.Add(2 * time.Hour), MinEnergy: 1, MaxPower: 1}
	if _, err := svc.Modify(p.ID, upd); !IsKind(err, KindTime) {
		t.Fatalf("modify after arrival: want time error, got %v", err)
	}
	if _, err := svc.Cancel(p.ID); !IsKind(err, KindTime) {
		t.Fatalf("cancel after arrival: want time error, got %v", err)
	}
	// 占用不得泄漏：计划仍然生效。
	occ, _ := svc.Occupancy(testBase, testBase.Add(time.Hour))
	if occ[0].Allocated == 0 {
		t.Fatalf("allocation leaked after rejected cancel")
	}
}

func TestCancelReleasesOnce(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	svc := newTestService(t, clock)

	r := req("r1", "v1", testBase, testBase.Add(time.Hour), 1_000_000, 100_000)
	p, err := svc.Submit(r)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := svc.Cancel(p.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	occ, _ := svc.Occupancy(testBase, testBase.Add(time.Hour))
	if occ[0].Allocated != 0 {
		t.Fatalf("cancel did not release: %d", occ[0].Allocated)
	}
	// 重复取消 -> 状态错误，不会重复释放。
	if _, err := svc.Cancel(p.ID); !IsKind(err, KindState) {
		t.Fatalf("double cancel: want state error, got %v", err)
	}
	// 已取消的计划不能修改。
	upd := UpdateRequest{Arrival: testBase, Departure: testBase.Add(time.Hour), MinEnergy: 1, MaxPower: 1}
	if _, err := svc.Modify(p.ID, upd); !IsKind(err, KindState) {
		t.Fatalf("modify cancelled: want state error, got %v", err)
	}
}

func TestConcurrentCancelAndClockCrossingArrival(t *testing.T) {
	// 修改/取消与时钟越过到达点并发：占用要么完整保留、要么完整释放，
	// 绝不泄漏或重复释放。
	for trial := 0; trial < 50; trial++ {
		clock := newFakeClock(testBase.Add(-time.Hour))
		svc := newTestService(t, clock)
		r := req("r1", "v1", testBase, testBase.Add(time.Hour), 1_000_000, 100_000)
		p, err := svc.Submit(r)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); svc.Cancel(p.ID) }()
		go func() { defer wg.Done(); clock.set(testBase.Add(time.Second)) }()
		wg.Wait()

		plan, _ := svc.GetPlan(p.ID)
		occ, _ := svc.Occupancy(testBase, testBase.Add(time.Hour))
		switch plan.State {
		case PlanCancelled:
			if occ[0].Allocated != 0 {
				t.Fatalf("trial %d: cancelled plan still occupies %d", trial, occ[0].Allocated)
			}
		case PlanActive:
			want := plan.Allocations[0].Power
			if occ[0].Allocated != want {
				t.Fatalf("trial %d: active plan occupies %d, want %d", trial, occ[0].Allocated, want)
			}
		}
	}
}

func TestConcurrentModifyAndCancel(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	svc := newTestService(t, clock)
	r := req("r1", "v1", testBase, testBase.Add(time.Hour), 1_000_000, 100_000)
	p, err := svc.Submit(r)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				svc.Cancel(p.ID)
			} else {
				svc.Modify(p.ID, UpdateRequest{
					Arrival:   testBase.Add(time.Hour),
					Departure: testBase.Add(2 * time.Hour),
					MinEnergy: 1_000_000, MaxPower: 100_000,
				})
			}
		}(i)
	}
	wg.Wait()

	plan, _ := svc.GetPlan(p.ID)
	occ, _ := svc.Occupancy(testBase, testBase.Add(2*time.Hour))
	var total Milliwatts
	for _, o := range occ {
		total += o.Allocated
	}
	if plan.State == PlanCancelled && total != 0 {
		t.Fatalf("cancelled plan still occupies %d", total)
	}
	if plan.State == PlanActive {
		var want Milliwatts
		for _, a := range plan.Allocations {
			want += a.Power
		}
		if total != want {
			t.Fatalf("active plan occupancy %d != plan allocations %d", total, want)
		}
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	store := &MemoryStore{}
	svc, err := NewService(store, WithClock(clock.now))
	if err != nil {
		t.Fatal(err)
	}
	slots := []Slot{{Start: testBase, End: testBase.Add(time.Hour), Capacity: 1_000_000}}
	if err := svc.ConfigureSlots(slots); err != nil {
		t.Fatal(err)
	}
	r := req("r1", "v1", testBase, testBase.Add(time.Hour), 1_000_000, 100_000)
	p, err := svc.Submit(r)
	if err != nil {
		t.Fatal(err)
	}

	// 从同一存储恢复新实例。
	svc2, err := NewService(store, WithClock(clock.now))
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc2.GetPlan(p.ID)
	if err != nil {
		t.Fatalf("plan not restored: %v", err)
	}
	if got.Energy != p.Energy || got.State != PlanActive {
		t.Fatalf("restored plan mismatch: %+v", got)
	}
	// 幂等表也恢复：重放同号请求返回原计划。
	p2, err := svc2.Submit(r)
	if err != nil || p2.ID != p.ID {
		t.Fatalf("idempotency not restored: plan=%v err=%v", p2, err)
	}
	// 占用恢复：新请求会看到容量已被占用。
	occ, _ := svc2.Occupancy(testBase, testBase.Add(time.Hour))
	if occ[0].Allocated == 0 {
		t.Fatalf("occupancy not restored")
	}
}

func TestFileStorePersistence(t *testing.T) {
	path := t.TempDir() + "/state.json"
	clock := newFakeClock(testBase.Add(-time.Hour))

	svc, err := NewService(NewFileStore(path), WithClock(clock.now))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ConfigureSlots([]Slot{{Start: testBase, End: testBase.Add(time.Hour), Capacity: 1_000_000}}); err != nil {
		t.Fatal(err)
	}
	r := req("r1", "v1", testBase, testBase.Add(time.Hour), 1_000_000, 100_000)
	p, err := svc.Submit(r)
	if err != nil {
		t.Fatal(err)
	}

	svc2, err := NewService(NewFileStore(path), WithClock(clock.now))
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc2.GetPlan(p.ID)
	if err != nil {
		t.Fatalf("plan not persisted to file: %v", err)
	}
	if got.Energy != p.Energy {
		t.Fatalf("energy mismatch after reload: %d != %d", got.Energy, p.Energy)
	}
}

func TestReconfigureRejectedWhileActivePlans(t *testing.T) {
	clock := newFakeClock(testBase.Add(-time.Hour))
	svc := newTestService(t, clock)
	r := req("r1", "v1", testBase, testBase.Add(time.Hour), 1_000_000, 100_000)
	p, err := svc.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.ConfigureSlots([]Slot{{Start: testBase, End: testBase.Add(time.Hour), Capacity: 1}})
	if !IsKind(err, KindState) {
		t.Fatalf("want state error, got %v", err)
	}
	// 取消后可以重配。
	if _, err := svc.Cancel(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ConfigureSlots([]Slot{{Start: testBase, End: testBase.Add(time.Hour), Capacity: 1}}); err != nil {
		t.Fatalf("reconfigure after cancel: %v", err)
	}
}

func TestErrorKindUnwrap(t *testing.T) {
	var e *Error
	err := error(capacityErrorf("boom"))
	if !errors.As(err, &e) || e.Kind != KindCapacity {
		t.Fatalf("errors.As failed: %v", err)
	}
}
