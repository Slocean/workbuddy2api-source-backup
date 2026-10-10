// fractional_credit_test.go —— 小数费用定点累计回归（2026-10-10 edsionss 错账修复）。
//
// 根因：NoteModelCost 逐笔 int64(credit+0.5) 四舍五入，141 笔真实 78.13 credit
// 被扣 115（多扣 36.87），spendCreditExpiries 的 FIFO 走桶把最早到期两笔
// （10-16 剩 7 / 10-17 剩 30）提前掏空，earliest_expiry 据此选错账号。
// 修复：小数费用以 1e-6 credit 定点累计，凑满整 credit 才落扣总量与到期桶；
// 残余小数随官方刷新（SetCredits* 家族）重置，随快照持久化重启不丢。
// 本文件的第 2 节用真实线上逐笔费用序列（141 笔，Hub 日志脱敏提取）重放，
// 证明修复后总扣减=整部分 78、残余 0.13 留账，总量与官方一致。
package pool

import (
	"math"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// edsionssReplayCredits 2026-10-10 09:16-10:43（北京）edsionss 上 141 笔 GLM 请求的
// 真实逐笔 usage.credit（Hub 日志逐笔提取，按时间序；仅费用数值，无账号信息）。
var edsionssReplayCredits = []float64{
	1.37, 0.43, 0.46, 0.42, 0.41, 0.41, 0.42, 0.42, 0.41, 0.43,
	0.49, 0.43, 0.42, 0.43, 0.45, 0.44, 0.43, 0.43, 0.43, 0.45,
	1.35, 0.43, 0.45, 0.44, 0.44, 0.43, 0.43, 0.46, 0.92, 0.58,
	0.54, 0.5, 0.5, 0.5, 0.5, 0.51, 0.5, 0.5, 0.51, 0.51,
	0.52, 0.51, 0.52, 0.51, 0.51, 0.54, 0.52, 0.55, 0.52, 0.53,
	0.54, 0.53, 0.52, 0.52, 0.52, 0.52, 0.53, 0.55, 0.53, 0.53,
	0.53, 0.53, 0.53, 0.53, 0.53, 0.54, 0.53, 0.53, 0.56, 0.54,
	0.55, 0.55, 0.54, 0.57, 0.57, 0.55, 0.55, 0.55, 0.55, 0.59,
	0.56, 0.55, 0.56, 0.56, 0.56, 0.56, 0.56, 0.56, 0.59, 0.56,
	0.59, 0.57, 0.56, 0.58, 0.59, 0.58, 0.59, 0.57, 0.59, 0.59,
	0.58, 0.59, 0.58, 0.58, 0.61, 0.59, 0.59, 0.58, 0.59, 0.59,
	0.59, 0.59, 0.59, 0.63, 0.6, 0.59, 0.59, 0.61, 0.6, 0.6,
	0.61, 0.6, 0.6, 0.61, 0.61, 0.6, 0.61, 0.6, 0.6, 0.61,
	0.61, 0.62, 0.61, 0.62, 0.61, 0.61, 0.61, 0.61, 0.62, 0.62,
	0.62,
}

// expiryPoolWithPackages 构建一个带官方积分包快照的池（ credits 由包总和决定）。
func expiryPoolWithPackages(uid string, packages []CreditExpiry) *poolInternal {
	p := New("")
	p.Add(&auth.Auth{UID: uid})
	var credits int64
	for _, pkg := range packages {
		credits += pkg.Remaining
	}
	p.SetCreditsWithExpiries(uid, credits, 0, packages)
	return &poolInternal{Pool: p, uid: uid}
}

type poolInternal struct {
	*Pool
	uid string
}

func (w *poolInternal) creditsAndPending() (credits int64, pending int64) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	e := w.byUID[w.uid]
	return e.credits, e.creditPendingMicro
}

func (w *poolInternal) expiryRemaining(at time.Time) int64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for _, item := range w.byUID[w.uid].creditExpiries {
		if item.Expiry.Equal(at) {
			return item.Remaining
		}
	}
	return -1
}

// TestNoteModelCostFractionalAccumulatesBeforeDebit 小数费用累计：不足整 credit
// 不落扣；跨过整 credit 边界才扣 1。旧口径逐笔取整（<0.5 恒 0 / ≥0.5 恒 1）两头错。
func TestNoteModelCostFractionalAccumulatesBeforeDebit(t *testing.T) {
	w := expiryPoolWithPackages("u1", []CreditExpiry{
		{Expiry: time.Now().Add(24 * time.Hour), Remaining: 10},
		{Expiry: time.Now().Add(30 * 24 * time.Hour), Remaining: 90},
	})
	for i := 0; i < 3; i++ {
		w.NoteModelCost("u1", "m", 0.3, 1000) // 0.9 累计中
	}
	if credits, pending := w.creditsAndPending(); credits != 100 || pending != 900_000 {
		t.Fatalf("after 3×0.3: credits=%d pending=%d, want 100/900000（不足整不落扣）", credits, pending)
	}
	w.NoteModelCost("u1", "m", 0.3, 1000) // 累计 1.2 → 落扣 1，残余 0.2
	if credits, pending := w.creditsAndPending(); credits != 99 || pending != 200_000 {
		t.Fatalf("after 4×0.3: credits=%d pending=%d, want 99/200000", credits, pending)
	}
}

// TestNoteModelCostMicroFromCreditBounded 有界转换：NaN/负值拒绝为 0；巨值/+Inf
// 钳制不溢出（int64 溢出成负会绕过 d>0 与扣穿钳制）；0.61 常规累计精度无损。
func TestNoteModelCostMicroFromCreditBounded(t *testing.T) {
	for _, c := range []float64{math.NaN(), -0.5, -1e18} {
		if got := microFromCredit(c); got != 0 {
			t.Fatalf("microFromCredit(%v)=%d want 0（NaN/负值拒绝）", c, got)
		}
	}
	if got := microFromCredit(math.Inf(1)); got != int64(maxSafeCredit)*creditMicroPerUnit {
		t.Fatalf("+Inf must clamp to maxSafeCredit, got %d", got)
	}
	if got := microFromCredit(1e15); got != int64(maxSafeCredit)*creditMicroPerUnit {
		t.Fatalf("huge credit must clamp, got %d（不得溢出为负）", got)
	}
	if got := microFromCredit(0.61); got != 610_000 {
		t.Fatalf("microFromCredit(0.61)=%d want 610000", got)
	}
	// 巨值经 NoteModelCost 全路径：钳到余额与上界，不产生负 credits/负 pending。
	w := expiryPoolWithPackages("u1", []CreditExpiry{
		{Expiry: time.Now().Add(24 * time.Hour), Remaining: 3},
		{Expiry: time.Now().Add(30 * 24 * time.Hour), Remaining: 7},
	})
	w.NoteModelCost("u1", "m", 1e15, 1000)
	if credits, pending := w.creditsAndPending(); credits != 0 || pending != 0 {
		t.Fatalf("huge credit through debit: credits=%d pending=%d, want 0/0", credits, pending)
	}
}

// TestPersistRestoreRejectsDirtyPending 恢复守卫：负值/≥整 credit 的脏余量拒绝，
// 合法残余（0.61）正常恢复（虚构账号 + tmp state）。
func TestPersistRestoreRejectsDirtyPending(t *testing.T) {
	dir := t.TempDir()
	fp := stateFilePath(t, dir)
	write := func(pending int64) {
		p := New(fp)
		p.Add(&auth.Auth{UID: "u1"})
		p.SetCreditsWithExpiries("u1", 100, 0, []CreditExpiry{
			{Expiry: time.Now().Add(24 * time.Hour), Remaining: 100},
		})
		p.mu.Lock()
		p.byUID["u1"].creditPendingMicro = pending
		p.mu.Unlock()
		p.Flush()
	}
	read := func() (credits, pending int64) {
		p2 := New(fp)
		p2.Add(&auth.Auth{UID: "u1"})
		p2.mu.RLock()
		defer p2.mu.RUnlock()
		return p2.byUID["u1"].credits, p2.byUID["u1"].creditPendingMicro
	}
	write(610_000) // 合法残余 0.61
	if credits, pending := read(); credits != 100 || pending != 610_000 {
		t.Fatalf("valid pending: credits=%d pending=%d, want 100/610000", credits, pending)
	}
	write(-5) // 负值脏数据
	if _, pending := read(); pending != 0 {
		t.Fatalf("negative pending must be rejected, got %d", pending)
	}
	write(2_000_000) // ≥整 credit：落扣后不可能出现，按脏数据拒绝
	if _, pending := read(); pending != 0 {
		t.Fatalf("out-of-range pending must be rejected, got %d", pending)
	}
}

// TestNoteModelCostSubHalfCreditsStillDebitEventually 旧口径 <0.5 的请求永远扣 0
// （乐观漏扣）；新口径累计到整就扣。
func TestNoteModelCostSubHalfCreditsStillDebitEventually(t *testing.T) {
	w := expiryPoolWithPackages("u1", []CreditExpiry{
		{Expiry: time.Now().Add(24 * time.Hour), Remaining: 5},
		{Expiry: time.Now().Add(30 * 24 * time.Hour), Remaining: 95},
	})
	for i := 0; i < 10; i++ {
		w.NoteModelCost("u1", "m", 0.4, 1000) // 真实消耗 4.0
	}
	if credits, _ := w.creditsAndPending(); credits != 96 {
		t.Fatalf("credits=%d want 96（10×0.4=4.0 应扣 4，旧口径恒扣 0）", credits)
	}
}

// TestNoteModelCostHalfBoundary 恰为 0.5 的费用：定点累计下不立即落扣（旧口径
// int64(0.5+0.5)=1 立即多扣），等下一笔凑整。
func TestNoteModelCostHalfBoundary(t *testing.T) {
	w := expiryPoolWithPackages("u1", []CreditExpiry{
		{Expiry: time.Now().Add(24 * time.Hour), Remaining: 10},
		{Expiry: time.Now().Add(30 * 24 * time.Hour), Remaining: 90},
	})
	w.NoteModelCost("u1", "m", 0.5, 1000)
	if credits, pending := w.creditsAndPending(); credits != 100 || pending != 500_000 {
		t.Fatalf("0.5: credits=%d pending=%d, want 100/500000（不逐笔四舍五入）", credits, pending)
	}
	w.NoteModelCost("u1", "m", 1.5, 1000) // 累计 2.0 → 扣 2
	if credits, pending := w.creditsAndPending(); credits != 98 || pending != 0 {
		t.Fatalf("0.5+1.5: credits=%d pending=%d, want 98/0", credits, pending)
	}
}

// TestNoteModelCostMultiModelSharedAccumulator 同一账号多模型混跑共享同一累计器
// （积分是账号级资源，不分模型）。
func TestNoteModelCostMultiModelSharedAccumulator(t *testing.T) {
	w := expiryPoolWithPackages("u1", []CreditExpiry{
		{Expiry: time.Now().Add(24 * time.Hour), Remaining: 10},
		{Expiry: time.Now().Add(30 * 24 * time.Hour), Remaining: 90},
	})
	w.NoteModelCost("u1", "m1", 0.3, 1000)
	w.NoteModelCost("u1", "m2", 0.3, 1000)
	w.NoteModelCost("u1", "m1", 0.3, 1000)
	w.NoteModelCost("u1", "m2", 0.3, 1000)
	if credits, _ := w.creditsAndPending(); credits != 99 {
		t.Fatalf("credits=%d want 99（四模型混合 1.2 应扣 1）", credits)
	}
}

// TestNoteModelCostIntegerCrossesBuckets 整数消耗仍按 FIFO 走到期桶：最早包先耗尽。
func TestNoteModelCostIntegerCrossesBuckets(t *testing.T) {
	early := time.Now().Add(24 * time.Hour)
	late := time.Now().Add(30 * 24 * time.Hour)
	w := expiryPoolWithPackages("u1", []CreditExpiry{{Expiry: early, Remaining: 10}, {Expiry: late, Remaining: 100}})
	w.NoteModelCost("u1", "m", 25, 1000)
	if credits, _ := w.creditsAndPending(); credits != 85 {
		t.Fatalf("credits=%d want 85", credits)
	}
	if got := w.expiryRemaining(early); got != 0 {
		t.Fatalf("early package remaining=%d want 0（FIFO 先耗最早包）", got)
	}
	if got := w.expiryRemaining(late); got != 85 {
		t.Fatalf("late package remaining=%d want 85", got)
	}
}

// TestNoteModelCostFractionalSurvivesRestart 残余小数随快照持久化：重启不失忆，
// 且继续参与凑整（不跨快照复活——过期快照上的累计器不恢复）。
func TestNoteModelCostFractionalSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	fp := stateFilePath(t, dir)
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsWithExpiries("u1", 100, 0, []CreditExpiry{
		{Expiry: time.Now().Add(24 * time.Hour), Remaining: 100},
	})
	p.NoteModelCost("u1", "m", 0.3, 1000)
	p.NoteModelCost("u1", "m", 0.3, 1000) // pending 0.6，未落扣
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.RLock()
	credits, pending := p2.byUID["u1"].credits, p2.byUID["u1"].creditPendingMicro
	p2.mu.RUnlock()
	if credits != 100 || pending != 600_000 {
		t.Fatalf("after restart: credits=%d pending=%d, want 100/600000（残余小数重启不丢）", credits, pending)
	}
	p2.NoteModelCost("u1", "m", 0.5, 1000) // 累计 1.1 → 扣 1
	p2.mu.RLock()
	credits, pending = p2.byUID["u1"].credits, p2.byUID["u1"].creditPendingMicro
	p2.mu.RUnlock()
	if credits != 99 || pending != 100_000 {
		t.Fatalf("after 0.5 more: credits=%d pending=%d, want 99/100000", credits, pending)
	}
}

// TestNoteModelCostStaleSnapshotDoesNotRestorePending 快照过期的旧 state.json：
// 累计器不跨快照复活（陈旧估算不得扣新快照）。
func TestNoteModelCostStaleSnapshotDoesNotRestorePending(t *testing.T) {
	dir := t.TempDir()
	fp := stateFilePath(t, dir)
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsWithExpiries("u1", 100, 0, []CreditExpiry{
		{Expiry: time.Now().Add(24 * time.Hour), Remaining: 100},
	})
	p.NoteModelCost("u1", "m", 0.9, 1000)
	// 手工把快照时间拨到 25h 前（模拟陈旧 state.json）
	p.mu.Lock()
	p.byUID["u1"].creditObservedAt = time.Now().Add(-25 * time.Hour)
	p.mu.Unlock()
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.RLock()
	credits, pending := p2.byUID["u1"].credits, p2.byUID["u1"].creditPendingMicro
	p2.mu.RUnlock()
	if credits != 100 || pending != 0 {
		t.Fatalf("stale snapshot: credits=%d pending=%d, want 100/0（累计器不跨快照复活）", credits, pending)
	}
}

// TestOfficialRefreshResetsPending 权威余额刷新（SetCredits* 家族）重置累计器：
// 官方快照覆盖估算，旧残余不得扣到新快照上。
func TestOfficialRefreshResetsPending(t *testing.T) {
	w := expiryPoolWithPackages("u1", []CreditExpiry{
		{Expiry: time.Now().Add(24 * time.Hour), Remaining: 10},
		{Expiry: time.Now().Add(30 * 24 * time.Hour), Remaining: 90},
	})
	w.NoteModelCost("u1", "m", 0.9, 1000) // pending 0.9
	// 官方刷新（签到/启动 RefreshCredits 路径）
	w.SetCreditsWithExpiries("u1", 747, 0, []CreditExpiry{
		{Expiry: time.Now().Add(6 * 24 * time.Hour), Remaining: 7},
		{Expiry: time.Now().Add(7 * 24 * time.Hour), Remaining: 30},
		{Expiry: time.Now().Add(30 * 24 * time.Hour), Remaining: 710},
	})
	if credits, pending := w.creditsAndPending(); credits != 747 || pending != 0 {
		t.Fatalf("after official refresh: credits=%d pending=%d, want 747/0", credits, pending)
	}
	w.NoteModelCost("u1", "m", 0.2, 1000)
	if credits, pending := w.creditsAndPending(); credits != 747 || pending != 200_000 {
		t.Fatalf("0.2 on fresh snapshot: credits=%d pending=%d, want 747/200000（不扣新快照）", credits, pending)
	}
	// SetCredits / SetCreditsDetailed 同样重置
	w.SetCredits("u1", 50)
	if _, pending := w.creditsAndPending(); pending != 0 {
		t.Fatalf("SetCredits must reset pending, got %d", pending)
	}
	w.NoteModelCost("u1", "m", 0.8, 1000)
	w.SetCreditsDetailed("u1", 60, 10)
	if _, pending := w.creditsAndPending(); pending != 0 {
		t.Fatalf("SetCreditsDetailed must reset pending, got %d", pending)
	}
	w.NoteModelCost("u1", "m", 0.3, 1000)
	w.ReenableIfCredits("u1", 55)
	if _, pending := w.creditsAndPending(); pending != 0 {
		t.Fatalf("ReenableIfCredits must reset pending, got %d", pending)
	}
}

// TestNoteModelCostClampDiscardsPending 估算余额扣穿：钳 0 防负并丢弃残余小数
// （该快照上无法再落扣；官方刷新会以真实余额覆盖）。
func TestNoteModelCostClampDiscardsPending(t *testing.T) {
	w := expiryPoolWithPackages("u1", []CreditExpiry{
		{Expiry: time.Now().Add(24 * time.Hour), Remaining: 5},
		{Expiry: time.Now().Add(30 * 24 * time.Hour), Remaining: 0},
	})
	w.NoteModelCost("u1", "m", 100, 1000)
	if credits, pending := w.creditsAndPending(); credits != 0 || pending != 0 {
		t.Fatalf("clamp: credits=%d pending=%d, want 0/0", credits, pending)
	}
	w.NoteModelCost("u1", "m", 0.7, 1000) // 余额 0：不再产生负数
	if credits, pending := w.creditsAndPending(); credits != 0 || pending != 700_000 {
		t.Fatalf("zero balance: credits=%d pending=%d, want 0/700000（无余额不再落扣）", credits, pending)
	}
}

// TestEdsionssRealCreditSequenceReplay 真实日志重放（根因复现与修复验证）：
// 2026-10-10 09:00 官方快照 825 credit（此后官方消耗 78.13，17:18 官方余 747），
// 141 笔真实逐笔费用重放——
//   - 旧口径（逐笔 int64(credit+0.5)）扣 115 → 缓存 710，与官方差 37，且最早两笔
//     （10-16 剩 7 / 10-17 剩 30）被提前掏空 → earliest_expiry 选错号（线上事故）；
//   - 新口径累计后落扣整部分 78 → 缓存 747 与官方一致，残余 0.13 留账。
func TestEdsionssRealCreditSequenceReplay(t *testing.T) {
	// 09:00 官方快照形状（17:18 官方形状 + 上游当日实际消耗落在后段包）。
	early1 := time.Date(2026, 10, 16, 0, 38, 12, 0, time.Local)
	early2 := time.Date(2026, 10, 17, 1, 15, 11, 0, time.Local)
	early3 := time.Date(2026, 10, 18, 1, 1, 50, 0, time.Local)
	early4 := time.Date(2026, 10, 20, 1, 12, 56, 0, time.Local)
	late := time.Date(2027, 3, 13, 15, 34, 32, 0, time.Local)
	packages := []CreditExpiry{
		{Expiry: early1, Remaining: 7},
		{Expiry: early2, Remaining: 30},
		{Expiry: early3, Remaining: 30},
		{Expiry: early4, Remaining: 30},
		{Expiry: late, Remaining: 728}, // 825 - 97：当日官方消耗发生在这里
	}
	w := expiryPoolWithPackages("u1", packages)
	var wantSum float64
	for _, c := range edsionssReplayCredits {
		wantSum += c
		w.NoteModelCost("u1", "glm-5.3-flash", c, 1000)
	}
	if wantSum < 78.12 || wantSum > 78.14 {
		t.Fatalf("sequence sum drifted: %f want ~78.13", wantSum)
	}
	credits, pending := w.creditsAndPending()
	if credits != 747 {
		t.Fatalf("replay credits=%d want 747（=官方 17:18 余额；旧口径得 710）", credits)
	}
	if pending != 130_000 {
		t.Fatalf("pending=%d want 130000（78.13 的整部分已扣，残余 0.13 留账）", pending)
	}
	// 修复后走桶仍是 FIFO 估算（10-16 先被估算耗尽）——这是快照间的内插语义，
	// 官方刷新（部署重启 startup RefreshCredits）会以真实包形状重置；
	// 此处只锁「总量精确 + 残余保留」这两条本修复的契约。
	if got := w.expiryRemaining(early1); got != 0 {
		t.Fatalf("early1 remaining=%d want 0（FIFO 估算语义不变）", got)
	}
}
