// selection_policy_persist_test.go 策略持久化的故障与并发契约
// （未通过验收修复交接 §5.4）：
//  1. 首次写/覆盖/连续写成功后，文件、内存与 GET effective 三方一致；
//  2. 替换失败（故障注入）与临时文件创建失败时**旧文件内容完整**、运行时策略
//     不变——绝不先删旧目标（§5.2），失败不产生任何现状改变；
//  3. 并发 PUT 串行化后，最终文件 == 内存 == GET，且无临时文件残留；
//  4. 重新构造 Handler（模拟重启）后读回一致。
//
// 故障注入是主断言手段（renameForPersist 注入点），不依赖平台文件系统行为的差异；
// 竞争检测由 go test -race 下运行并发用例覆盖，不替代注入。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/pool"
)

// persistedPolicy 读持久化文件的 selection_policy 值（测试断言用）。
func persistedPolicy(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	var doc struct {
		SelectionPolicy string `json:"selection_policy"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("persisted file not json: %v", err)
	}
	return doc.SelectionPolicy
}

// noTempResidue 断言目录无 *.tmp-* 残留（唯一临时文件在成功/失败路径都必须清理，
// 或已被 rename 走——固定 .tmp 争用的旧病不允许复发）。
func noTempResidue(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file residue: %s", e.Name())
		}
	}
}

func TestPersistSelectionPolicyFirstOverwriteConsecutive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selection-policy.json")
	// 首次写
	if err := persistSelectionPolicy(path, "cheapest"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if got := persistedPolicy(t, path); got != "cheapest" {
		t.Fatalf("after first write = %q", got)
	}
	// 覆盖写（旧目标存在，替换不先删除）
	if err := persistSelectionPolicy(path, "balanced"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got := persistedPolicy(t, path); got != "balanced" {
		t.Fatalf("after overwrite = %q", got)
	}
	// 连续写
	for i, p := range []string{"expiry_then_cheapest", "earliest_expiry", "cheapest_then_expiry"} {
		if err := persistSelectionPolicy(path, p); err != nil {
			t.Fatalf("consecutive #%d: %v", i, err)
		}
		if got := persistedPolicy(t, path); got != p {
			t.Fatalf("consecutive #%d = %q want %q", i, got, p)
		}
	}
	noTempResidue(t, dir)
}

func TestPersistSelectionPolicyRenameFailureKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selection-policy.json")
	if err := persistSelectionPolicy(path, "cheapest"); err != nil {
		t.Fatal(err)
	}
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 故障注入：替换（rename）失败——模拟 Windows ACL 拒绝/磁盘故障等替换窗口错误。
	orig := renameForPersist
	renameForPersist = func(from, to string) error {
		return errors.New("injected rename failure")
	}
	defer func() { renameForPersist = orig }()

	err = persistSelectionPolicy(path, "balanced")
	if err == nil {
		t.Fatal("rename failure not reported")
	}
	// 旧文件内容完整（字节级不变）——不是"临时文件完整"而是"旧目标始终存在"。
	raw, err2 := os.ReadFile(path)
	if err2 != nil {
		t.Fatalf("old file unreadable after failed rename: %v", err2)
	}
	if string(raw) != string(oldRaw) {
		t.Fatalf("old file corrupted: %q", raw)
	}
	if got := persistedPolicy(t, path); got != "cheapest" {
		t.Fatalf("old policy lost: %q", got)
	}
	noTempResidue(t, dir)
}

func TestPersistSelectionPolicyTempCreateFailureNoSideEffect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selection-policy.json")
	if err := persistSelectionPolicy(path, "cheapest"); err != nil {
		t.Fatal(err)
	}
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 临时文件创建失败注入：目标父路径是一个**文件**（其"子目录"无法创建）。
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(blocker, "sub", "selection-policy.json")
	if err := persistSelectionPolicy(badPath, "balanced"); err == nil {
		t.Fatal("temp create failure not reported")
	}
	// 原目标文件原样保留
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("old file unreadable: %v", err)
	}
	if string(raw) != string(oldRaw) {
		t.Fatalf("old file corrupted: %q", raw)
	}
	noTempResidue(t, dir)
}

// TestSelectionPolicyConcurrentPutConsistent 并发 PUT（同一 Handler）：互斥覆盖
// 「持久化 → 内存更新 → 响应」全程，结束后文件 == 内存 == GET，无临时残留。
// go test -race 下同时作为竞争检测。
func TestSelectionPolicyConcurrentPutConsistent(t *testing.T) {
	h, dataPath := newSelectionPolicyHandler(t, "mgmt-secret", "call-key")
	policies := []string{"balanced", "cheapest", "earliest_expiry", "expiry_then_cheapest", "cheapest_then_expiry"}
	const writers = 8
	const rounds = 6
	var wg sync.WaitGroup
	errs := make(chan error, writers*rounds)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				p := policies[(w*rounds+i)%len(policies)]
				rec := doPolicy(t, h, http.MethodPut, "mgmt-secret",
					fmt.Sprintf(`{"selection_policy":%q}`, p))
				if rec.Code != http.StatusOK {
					errs <- fmt.Errorf("concurrent PUT %s code=%d body=%s", p, rec.Code, rec.Body)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// 三方一致：文件 == 内存 effective == GET current/effective
	inFile := persistedPolicy(t, dataPath)
	if inFile != h.cfg.Pool.SelectionPolicy() {
		t.Fatalf("file=%s memory=%s", inFile, h.cfg.Pool.SelectionPolicy())
	}
	rec := doPolicy(t, h, http.MethodGet, "mgmt-secret", "")
	var snap selectionPolicySnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if snap.Current != inFile || snap.Effective != inFile {
		t.Fatalf("GET current=%s effective=%s file=%s", snap.Current, snap.Effective, inFile)
	}
	noTempResidue(t, filepath.Dir(dataPath))
}

// TestSelectionPolicyHandlerRebuildReadsBack 模拟重启：PUT 成功后用同一 dataPath
// 重新构造 Handler，GET 的 current/effective 与写入值一致，LoadPersistedSelectionPolicy
// 幂等读回（§5.4）。
func TestSelectionPolicyHandlerRebuildReadsBack(t *testing.T) {
	h, dataPath := newSelectionPolicyHandler(t, "mgmt-secret", "call-key")
	rec := doPolicy(t, h, http.MethodPut, "mgmt-secret", `{"selection_policy":"cheapest"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT code=%d body=%s", rec.Code, rec.Body)
	}

	// 重建（重启等价）：按生产装配顺序（cmd/server/main.go）——先读 data 持久化值
	// 注入池，再构建 Handler；锁定的 env 优先路径不涉及本用例。
	p2 := pool.New("")
	p2.SetSelectionPolicy("expiry_then_cheapest")
	if v := LoadPersistedSelectionPolicy(dataPath); v != "" {
		p2.SetSelectionPolicy(v)
	}
	h2 := NewHandler(Config{
		Pool:                       p2,
		AdminSelectionPolicyEnabled: true,
		AdminManagementKey:         "mgmt-secret",
		APIKey:                     "call-key",
		SelectionPolicyConfigValue: "expiry_then_cheapest",
		SelectionPolicyDataPath:    dataPath,
	})
	rec = doPolicy(t, h2, http.MethodGet, "mgmt-secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after rebuild code=%d", rec.Code)
	}
	var snap selectionPolicySnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if snap.Current != "cheapest" || snap.Effective != "cheapest" {
		t.Fatalf("after rebuild current=%s effective=%s want cheapest/cheapest", snap.Current, snap.Effective)
	}
	if v := LoadPersistedSelectionPolicy(dataPath); v != "cheapest" {
		t.Fatalf("LoadPersistedSelectionPolicy=%q", v)
	}
}
