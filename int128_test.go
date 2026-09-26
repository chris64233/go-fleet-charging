package fleetcharging

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
	"time"
)

func TestInt128MulAgainstBigInt(t *testing.T) {
	cases := []struct{ a, b int64 }{
		{0, 0}, {1, 1}, {-1, 1}, {-1, -1},
		{1 << 40, 1 << 30}, // 超出 int64 的乘积
		{-(1 << 40), 1 << 30},
		{-(1 << 40), -(1 << 30)},
		{math.MaxInt64, 2},
		{math.MaxInt64, -2},
	}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		cases = append(cases, struct{ a, b int64 }{
			rng.Int63() - 1<<62,
			rng.Int63() - 1<<62,
		})
	}
	for _, c := range cases {
		got := Mul128(c.a, c.b)
		want := new(big.Int).Mul(big.NewInt(c.a), big.NewInt(c.b))
		wh := want.Uint64()
		var shi int64
		if want.Sign() < 0 {
			neg := new(big.Int).Neg(want)
			// 128 位二进制补码：-(neg) = ^neg + 1
			lo := ^neg.Uint64() + 1
			var carry uint64
			if lo != 0 {
				carry = 0
			} else {
				carry = 1
			}
			hi := new(big.Int).Rsh(neg, 64).Uint64()
			shi = int64(^hi + carry)
			wh = lo
		} else {
			shi = int64(new(big.Int).Rsh(want, 64).Uint64())
		}
		if got.hi != shi || got.lo != wh {
			t.Fatalf("Mul128(%d,%d) = (%d,%d), want (%d,%d)",
				c.a, c.b, got.hi, got.lo, shi, wh)
		}
		if got.Sign() != want.Sign() {
			t.Fatalf("Sign(%d,%d) = %d, want %d", c.a, c.b, got.Sign(), want.Sign())
		}
	}
}

func TestInt128AddSubRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 1000; i++ {
		a := Mul128(rng.Int63()-1<<62, rng.Int63n(1000)+1)
		b := Mul128(rng.Int63()-1<<62, rng.Int63n(1000)+1)
		s := Add128(a, b)
		d := Sub128(s, b)
		if d.hi != a.hi || d.lo != a.lo {
			t.Fatalf("(a+b)-b != a: a=(%d,%d) b=(%d,%d)", a.hi, a.lo, b.hi, b.lo)
		}
	}
}

func TestCeilDiv128(t *testing.T) {
	// 1 µWh = 3.6e6 W·ns。
	if got := CeilDiv128(From64(3_600_000), 3_600_000); got != 1 {
		t.Fatalf("ceil = %d, want 1", got)
	}
	// 1 W·ns / 3.6e6 -> ceil = 1（至少 1W，保证不少给）。
	if got := CeilDiv128(From64(1), 3_600_000); got != 1 {
		t.Fatalf("ceil = %d, want 1", got)
	}
	if got := CeilDiv128(From64(3_600_001), 1); got != 3_600_001 {
		t.Fatalf("ceil = %d, want 3600001", got)
	}
	// 3.6e6-1 W·ns 分摊在 3.6e6 ns（1h）上 → ceil = 1W。
	if got := CeilDiv128(From64(3_599_999), int64(time.Hour)); got != 1 {
		t.Fatalf("ceil = %d, want 1", got)
	}
	// 超大被除数（超 int64）返回 MaxInt64 而不是溢出/截断。
	huge := Mul128(math.MaxInt64, 1_000_000)
	if got := CeilDiv128(huge, 1); got != math.MaxInt64 {
		t.Fatalf("ceil huge = %d, want MaxInt64", got)
	}
}

func TestScheduleLastSlotUsesMinimalCeilingPower(t *testing.T) {
	// 两片各 1h、各 100kW 空闲；目标 150 kWh。
	// 第一片取满 100kW，第二片只需 50kW（不能取满 100kW）。
	h := int64(time.Hour)
	ps := []piece{
		{start: mkTime(0), end: mkTime(1), capacity: 100_000},
		{start: mkTime(1), end: mkTime(2), capacity: 100_000},
	}
	targetWork := Mul128(150_000, h) // 150 kW × 1h = 150 kWh（W·ns）
	allocs, total := scheduleAllocation(ps, 100_000, targetWork)
	if len(allocs) != 2 {
		t.Fatalf("allocs = %+v", allocs)
	}
	if allocs[0].PowerW != 100_000 || allocs[1].PowerW != 50_000 {
		t.Fatalf("powers = %d,%d want 100000,50000", allocs[0].PowerW, allocs[1].PowerW)
	}
	if Cmp128(total, targetWork) < 0 {
		t.Fatal("delivered work below target")
	}
}

func TestScheduleRespectsReservedAndMaxPower(t *testing.T) {
	ps := []piece{
		{start: mkTime(0), end: mkTime(1), capacity: 100, reserved: 80}, // 仅剩 20W
		{start: mkTime(1), end: mkTime(2), capacity: 100, reserved: 0},
	}
	// 目标 50 Wh（50W × 1h），单车功率上限 40W：
	// 第一片最多 20W，第二片给 30W。
	targetWork := Mul128(50, int64(time.Hour))
	allocs, total := scheduleAllocation(ps, 40, targetWork)
	if len(allocs) != 2 || allocs[0].PowerW != 20 || allocs[1].PowerW != 30 {
		t.Fatalf("allocs = %+v", allocs)
	}
	if Cmp128(total, targetWork) < 0 {
		t.Fatal("under-delivered")
	}
}

func TestScheduleShortfallWhenCapacityExhausted(t *testing.T) {
	ps := []piece{
		{start: mkTime(0), end: mkTime(1), capacity: 100, reserved: 100},
	}
	targetWork := Mul128(1, int64(time.Hour))
	_, total := scheduleAllocation(ps, 100, targetWork)
	if Cmp128(total, targetWork) >= 0 {
		t.Fatal("should report shortfall")
	}
}

func mkTime(h int64) time.Time {
	return time.Unix(0, h*int64(time.Hour))
}
