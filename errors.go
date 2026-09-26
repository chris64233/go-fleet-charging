package fleetcharging

import "fmt"

// Kind 区分错误类别：参数、容量、时间、状态、幂等。
type Kind int

const (
	// KindInvalidParam 请求参数不合法（字段缺失、区间不合法、超出已配置时段等）。
	KindInvalidParam Kind = iota + 1
	// KindCapacity 容量不足，计划被整体拒绝。
	KindCapacity
	// KindTime 时间约束不满足（到达点已过、到达不在未来等）。
	KindTime
	// KindState 计划状态不允许该操作（已取消、存在进行中的计划时重配站点等）。
	KindState
	// KindIdempotency 外部请求号冲突：同号不同内容。
	KindIdempotency
)

// Error 是服务返回的统一错误类型，携带类别以便调用方区分处理。
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Kind, e.Msg) }

func (k Kind) String() string {
	switch k {
	case KindInvalidParam:
		return "invalid parameter"
	case KindCapacity:
		return "insufficient capacity"
	case KindTime:
		return "time constraint violated"
	case KindState:
		return "invalid state"
	case KindIdempotency:
		return "idempotency conflict"
	default:
		return "unknown error"
	}
}

// IsKind 判断 err 是否属于指定类别。
func IsKind(err error, k Kind) bool {
	e, ok := err.(*Error)
	return ok && e.Kind == k
}

func paramErrorf(format string, args ...any) error {
	return &Error{Kind: KindInvalidParam, Msg: fmt.Sprintf(format, args...)}
}

func capacityErrorf(format string, args ...any) error {
	return &Error{Kind: KindCapacity, Msg: fmt.Sprintf(format, args...)}
}

func timeErrorf(format string, args ...any) error {
	return &Error{Kind: KindTime, Msg: fmt.Sprintf(format, args...)}
}

func stateErrorf(format string, args ...any) error {
	return &Error{Kind: KindState, Msg: fmt.Sprintf(format, args...)}
}

func idempotencyErrorf(format string, args ...any) error {
	return &Error{Kind: KindIdempotency, Msg: fmt.Sprintf(format, args...)}
}
