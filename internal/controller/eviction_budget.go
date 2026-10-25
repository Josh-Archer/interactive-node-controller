package controller

import (
	"math"
	"math/bits"
	"sync"
	"time"
)

// EvictionBudget is the token bucket for one workload class.
// An Eviction API call spends one per-node token and one global token.
// Tokens refill continuously at the steady count per Interval, up to the
// burst capacity, and do not reset when Reconcile runs. A zero field inherits
// the conservative policy default: per-node count and burst come from
// MaxPerReconcile, global count and burst match that per-node allowance, and
// the interval comes from BudgetInterval.
type EvictionBudget struct {
	// PerNode is the steady number of eviction API calls one node may make per Interval.
	PerNode int
	// Global is the steady number of eviction API calls this process may make
	// per Interval across every node. It is not a per-node limit.
	Global int
	// Interval is the refill window for this class. Zero inherits BudgetInterval.
	Interval time.Duration
	// BurstPerNode is the per-node bucket capacity. Zero inherits PerNode, so
	// the default burst does not exceed one interval of budget.
	BurstPerNode int
	// BurstGlobal is the process-wide bucket capacity. Zero inherits Global.
	BurstGlobal int
}

func (b EvictionBudget) normalized(count int, interval time.Duration) EvictionBudget {
	if b.PerNode <= 0 {
		b.PerNode = count
	}
	if b.Global <= 0 {
		b.Global = b.PerNode
	}
	if b.Interval <= 0 {
		b.Interval = interval
	}
	if b.BurstPerNode <= 0 {
		b.BurstPerNode = b.PerNode
	}
	if b.BurstGlobal <= 0 {
		b.BurstGlobal = b.Global
	}
	return b
}

// evictionBudgets holds process-local token buckets. A restart or leader
// switch starts from empty maps, which grants a fresh full burst. Spent
// budget is not retained across processes.
type evictionBudgets struct {
	mu             sync.Mutex
	standardNodes  map[string]*tokenBucket
	lossyNodes     map[string]*tokenBucket
	standardGlobal *tokenBucket
	lossyGlobal    *tokenBucket
}

// tryTake spends one per-node token and one global token for the class.
// policy must already be normalized. The returned wait is how long until both
// buckets can spend again; it is positive when the call is refused.
func (b *evictionBudgets) tryTake(lossy bool, node string, now time.Time, policy EvictionPolicy) (bool, time.Duration) {
	spec := policy.StandardBudget
	if lossy {
		spec = policy.LossyBudget
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	nodeBucket := b.nodeBucket(lossy, node)
	globalBucket := b.globalBucket(lossy)
	nodeBucket.configure(spec.PerNode, spec.BurstPerNode, spec.Interval)
	globalBucket.configure(spec.Global, spec.BurstGlobal, spec.Interval)
	nodeBucket.refill(now)
	globalBucket.refill(now)
	if nodeBucket.tokens < 1 || globalBucket.tokens < 1 {
		wait := nodeBucket.waitForOne()
		if globalWait := globalBucket.waitForOne(); globalWait > wait {
			wait = globalWait
		}
		if wait <= 0 {
			// Both buckets must have a token. A zero estimate would skip the
			// requeue and leave the candidate waiting for an unrelated event.
			wait = time.Nanosecond
		}
		return false, wait
	}
	nodeBucket.tokens--
	globalBucket.tokens--
	return true, 0
}

func (b *evictionBudgets) nodeBucket(lossy bool, node string) *tokenBucket {
	nodes := &b.standardNodes
	if lossy {
		nodes = &b.lossyNodes
	}
	if *nodes == nil {
		*nodes = make(map[string]*tokenBucket)
	}
	bucket := (*nodes)[node]
	if bucket == nil {
		bucket = &tokenBucket{}
		(*nodes)[node] = bucket
	}
	return bucket
}

func (b *evictionBudgets) globalBucket(lossy bool) *tokenBucket {
	slot := &b.standardGlobal
	if lossy {
		slot = &b.lossyGlobal
	}
	if *slot == nil {
		*slot = &tokenBucket{}
	}
	return *slot
}

// tokenBucket refills continuously and stores at most capacity whole tokens.
// It starts full so the first eligible Pod is not delayed by process start.
// Time spent while the bucket is already full is discarded; that keeps a
// restart-sized burst from growing past the configured capacity.
type tokenBucket struct {
	tokens   int
	residue  time.Duration
	updated  time.Time
	count    int
	capacity int
	interval time.Duration
	ready    bool
}

func (tb *tokenBucket) configure(count, capacity int, interval time.Duration) {
	if count < 1 {
		count = 1
	}
	if capacity < 1 {
		capacity = 1
	}
	if interval <= 0 {
		interval = time.Minute
	}
	if !tb.ready {
		tb.count = count
		tb.capacity = capacity
		tb.interval = interval
		tb.tokens = capacity
		tb.ready = true
		return
	}
	tb.count = count
	tb.interval = interval
	tb.capacity = capacity
	if tb.tokens > capacity {
		tb.tokens = capacity
	}
}

func (tb *tokenBucket) refill(now time.Time) {
	if !tb.ready {
		return
	}
	if tb.updated.IsZero() {
		tb.updated = now
		tb.residue = 0
		return
	}
	if !now.After(tb.updated) {
		return
	}
	elapsed := now.Sub(tb.updated) + tb.residue
	tb.updated = now
	room := tb.capacity - tb.tokens
	if room <= 0 {
		tb.tokens = tb.capacity
		tb.residue = 0
		return
	}
	earned := tokensEarned(elapsed, tb.count, tb.interval, room)
	if earned <= 0 {
		tb.residue = elapsed
		return
	}
	tb.tokens += earned
	if earned >= room {
		tb.residue = 0
		return
	}
	consumed := time.Duration(mulDivFloor(int64(earned), int64(tb.interval), int64(tb.count)))
	if consumed < 0 || consumed > elapsed {
		tb.residue = 0
		return
	}
	tb.residue = elapsed - consumed
}

func (tb *tokenBucket) waitForOne() time.Duration {
	if tb.tokens >= 1 || tb.count < 1 || tb.interval <= 0 {
		return 0
	}
	have := int64(tb.residue) * int64(tb.count)
	need := int64(tb.interval) - have
	if need <= 0 {
		return time.Nanosecond
	}
	wait := (need + int64(tb.count) - 1) / int64(tb.count)
	if wait < 1 {
		wait = 1
	}
	return time.Duration(wait)
}

// tokensEarned returns how many whole tokens elapsed time is worth, capped at
// limit so a long idle period cannot overflow the bucket.
func tokensEarned(elapsed time.Duration, count int, interval time.Duration, limit int) int {
	if elapsed <= 0 || count <= 0 || interval <= 0 || limit <= 0 {
		return 0
	}
	full := int64(elapsed) / int64(interval)
	if full >= int64(limit) {
		return limit
	}
	earned := int(full) * count
	if earned >= limit {
		return limit
	}
	rem := elapsed % interval
	if rem > 0 {
		earned += int(mulDivFloor(int64(rem), int64(count), int64(interval)))
	}
	if earned > limit {
		return limit
	}
	if earned < 0 {
		return 0
	}
	return earned
}

func mulDivFloor(a, b, div int64) int64 {
	if a <= 0 || b <= 0 || div <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(div) {
		return math.MaxInt64
	}
	q, _ := bits.Div64(hi, lo, uint64(div))
	if q > uint64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(q)
}
