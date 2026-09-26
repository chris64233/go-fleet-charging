package fleetcharging

import "math/bits"

// Int128 是有符号 128 位整数。
// 能量以 W·ns（瓦·纳秒）为单位时，1 µWh = 3.6e6 W·ns，
// 功率与时长上限远超 int64 乘积范围，故用 128 位保证精确、无浮点。
type Int128 struct {
	hi int64
	lo uint64
}

// Zero128 返回 0。
func Zero128() Int128 { return Int128{} }

// IsInt64 报告 x 是否落在 int64 范围内。
func (x Int128) IsInt64() bool {
	return x.hi == 0 && x.lo < 1<<63 || x.hi == -1 && x.lo >= 1<<63
}

// Int64 取低 64 位的二进制补码值；超范围时结果未定义，调用前应先判断 IsInt64。
func (x Int128) Int64() int64 { return int64(x.lo) }

// From64 由 int64 构造。
func From64(v int64) Int128 {
	if v >= 0 {
		return Int128{hi: 0, lo: uint64(v)}
	}
	return Int128{hi: -1, lo: uint64(v)}
}

// Add128 返回 a+b。
func Add128(a, b Int128) Int128 {
	lo, carry := bits.Add64(a.lo, b.lo, 0)
	hi := a.hi + b.hi + int64(carry)
	return Int128{hi: hi, lo: lo}
}

// Sub128 返回 a-b。
func Sub128(a, b Int128) Int128 {
	lo, borrow := bits.Sub64(a.lo, b.lo, 0)
	hi := a.hi - b.hi - int64(borrow)
	return Int128{hi: hi, lo: lo}
}

// Mul128 返回 a*b（有符号）。
func Mul128(a, b int64) Int128 {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	// 处理符号位：若乘数为负，减去另一个操作数。
	if a < 0 {
		hi -= uint64(b)
	}
	if b < 0 {
		hi -= uint64(a)
	}
	return Int128{hi: int64(hi), lo: lo}
}

// Cmp128 返回 -1/0/1。
func Cmp128(a, b Int128) int {
	if a.hi != b.hi {
		if a.hi < b.hi {
			return -1
		}
		return 1
	}
	if a.lo != b.lo {
		if a.lo < b.lo {
			return -1
		}
		return 1
	}
	return 0
}

// Sign 返回 -1/0/1。
func (x Int128) Sign() int {
	if x.hi < 0 {
		return -1
	}
	if x.hi == 0 && x.lo == 0 {
		return 0
	}
	return 1
}

// QuoRem128 计算 128 位非负整数 x 除以非负 int64 d，返回商与余数（商允许超 int64）。
// x、d 必须非负，否则 panic——本服务只用它做向上取整的正数除法。
func QuoRem128(x Int128, d int64) (q Int128, r int64) {
	if d < 0 || x.Sign() < 0 {
		panic("fleetcharging: QuoRem128 requires non-negative operands")
	}
	if d == 0 {
		panic("fleetcharging: divide by zero")
	}
	// 逐 64 位长除法；bits.Div64 返回 (quo, rem)。
	var rhi, rlo, qhi, qlo uint64
	if x.hi >= 0 {
		qhi, rhi = bits.Div64(0, uint64(x.hi), uint64(d))
		qlo, rlo = bits.Div64(rhi, x.lo, uint64(d))
	} else {
		// 不支持负数路径（当前不可达）。
		panic("fleetcharging: negative dividend")
	}
	return Int128{hi: int64(qhi), lo: qlo}, int64(rlo)
}

// CeilDiv128 返回 ceil(x/d)；x、d 非负。
// 商超出 int64 时返回 MaxInt64（对本服务而言等于“需要的功率大到不可能满足”）。
func CeilDiv128(x Int128, d int64) int64 {
	q, r := QuoRem128(x, d)
	if !q.IsInt64() {
		return 1<<63 - 1
	}
	v := int64(q.lo)
	if r > 0 {
		if v == 1<<63-1 {
			return 1<<63 - 1
		}
		v++
	}
	return v
}
