// selection_policy.go 账号池选号策略的管理端点（总反代平台阶段 3.1.1-3.1.2）。
//
// 提供给 Hub（总反代平台）的受控管理面：
//   - GET /admin/pool/selection-policy  读当前生效值
//   - PUT /admin/pool/selection-policy  修改并立即生效、持久化
//
// 凭证契约（修复清单 §3.1）：这两个端点**只接受独立管理 Key**
// （config admin.management_key / env WB2A_ADMIN_MANAGEMENT_KEY），
// 普通**模型调用 Key（api_key）一律 401**——选号策略是路由控制面配置，
// 与调用面凭证分离，防止调用凭证持有者获得账号池策略控制权。
// 该校验在本服务强制执行，不依赖 Hub 侧自律。
//
// 可变策略持久化到 data/selection-policy.json（与 state.json 同卷，原子替换）。
// 优先级：WB2A_SELECTION_POLICY 环境变量（部署锁定，PUT → 409）>
// data/selection-policy.json > config.json pool.selection_policy > 内置默认。
// config.json 通常是只读挂载，它只在尚无专用持久化值时作为初始值——
// 首次 PUT 之后 data 文件成为权威，重启读回不变。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"workbuddy2api/internal/pool"
)

// selectionPolicies 与 pool.SetSelectionPolicy 的白名单一致（两处必须同步修改）。
var selectionPolicies = map[string]bool{
	"balanced": true, "cheapest": true, "earliest_expiry": true,
	"expiry_then_cheapest": true, "cheapest_then_expiry": true,
}

// selectionPolicyProtoVersion 归属/管理协议版本：Hub 据此识别实例能力代次。
const selectionPolicyProtoVersion = "selection-policy.v1"

// selectionPolicySnapshot GET/PUT 的统一响应体。Hub 写后读回 effective 判断成败。
type selectionPolicySnapshot struct {
	Supported       bool   `json:"supported"`
	Current         string `json:"current"`
	Effective       string `json:"effective"`
	Source          string `json:"source"`
	ScopeSupported  bool   `json:"scope_supported"`
	ScopeCurrent    string `json:"scope_current"`
	ScopeEffective  string `json:"scope_effective"`
	ScopeSource     string `json:"scope_source"`
	ScopeLocked     bool   `json:"scope_locked"`
	PolicyLocked    bool   `json:"policy_locked"`
	RestartRequired bool   `json:"restart_required"`
	Version         string `json:"version"`
}

// withManagementKey 独立管理 Key 鉴权。与 withAuth（模型调用 Key）**互斥**：
// 只比较 admin.management_key；普通调用 Key 即使合法也拿不到管理面。
// 除"未配置一律 401"外，再做**运行期同值防御**（未通过验收修复交接 §4.1）：
// management_key 与 api_key 配成同值时直接拒绝——cmd/server 启动校验虽已拦截，
// 但单元测试/嵌入式装配可绕过 Load 直接构造 Config，鉴权层必须独立兜底，
// 不让"同值配置"在运行期拿到管理面。
func (h *Handler) withManagementKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(h.cfg.AdminManagementKey)
		callKey := strings.TrimSpace(h.cfg.APIKey)
		if key == "" || (callKey != "" && key == callKey) ||
			!strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_management_key",
				"management key required")
			return
		}
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !constantTimeEqual(provided, key) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_management_key",
				"missing or invalid management key")
			return
		}
		next(w, r)
	}
}

// selectionPolicySource 解析 current 的来源与取值：
// 环境变量（部署锁定）> data 持久化文件 > config.json 初始值。
func (h *Handler) selectionPolicySource() (current, source string) {
	if h.cfg.SelectionPolicyLocked {
		return h.cfg.SelectionPolicyEnvValue, "env:WB2A_SELECTION_POLICY"
	}
	if v := h.readPersistedSelectionPolicy(); v != "" {
		return v, "data/selection-policy.json"
	}
	return h.cfg.SelectionPolicyConfigValue, "config.json"
}

// selectionPolicyState 组装快照。effective 取池的运行时真值；
// restart_required 恒 false（PUT 立即生效并持久化，无"内存变了重启丢"的形态）。
func (h *Handler) selectionPolicyState() selectionPolicySnapshot {
	current, source := h.selectionPolicySource()
	scopeCurrent, scopeSource := h.selectionScopeSource()
	return selectionPolicySnapshot{
		Supported:      true,
		ScopeSupported: true, ScopeCurrent: scopeCurrent, ScopeEffective: h.cfg.Pool.SelectionScope(), ScopeSource: scopeSource,
		ScopeLocked: h.cfg.SelectionScopeLocked, PolicyLocked: h.cfg.SelectionPolicyLocked,
		Current:   current,
		Effective: h.cfg.Pool.SelectionPolicy(),
		Source:    source,
		Version:   ServiceName + "/" + selectionPolicyProtoVersion,
	}
}

func (h *Handler) selectionPolicyGet(w http.ResponseWriter, r *http.Request) {
	h.selPolicyMu.Lock()
	defer h.selPolicyMu.Unlock()
	writeJSON(w, http.StatusOK, h.selectionPolicyState())
}

// selectionPolicyPut 校验 → 锁定判定 → 串行互斥下「持久化 → 运行时生效」，
// 任何一步失败都不改变现状（原子写：唯一临时文件 + 不删目标的替换，§5）。
// 成功后 effective 立即为新值。
func (h *Handler) selectionPolicyPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SelectionPolicy *string `json:"selection_policy"`
		SelectionScope  *string `json:"selection_scope"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || (body.SelectionPolicy == nil && body.SelectionScope == nil) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "selection_policy or selection_scope required")
		return
	}
	if body.SelectionPolicy != nil && !selectionPolicies[strings.TrimSpace(*body.SelectionPolicy)] {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_selection_policy", "invalid selection_policy")
		return
	}
	if body.SelectionScope != nil && (strings.TrimSpace(*body.SelectionScope) == "" || !pool.ValidSelectionScope(strings.TrimSpace(*body.SelectionScope))) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_selection_scope", "selection_scope must be prefer_global, prefer_cn or all")
		return
	}
	if (body.SelectionPolicy != nil && h.cfg.SelectionPolicyLocked) || (body.SelectionScope != nil && h.cfg.SelectionScopeLocked) {
		writeJSON(w, http.StatusConflict, map[string]string{"detail": "requested field is locked by deployment config"})
		return
	}
	if h.cfg.SelectionPolicyDataPath == "" {
		writeOpenAIError(w, http.StatusInternalServerError, "persist_unavailable",
			"data path not configured; cannot persist selection policy")
		return
	}
	// 串行化「持久化 → 内存更新 → 响应组装」：并发 PUT 时后到者覆盖先到者且
	// 响应各自如实反映自身写入结果，不出现文件/内存/响应三方不一致（§5.3）。
	h.selPolicyMu.Lock()
	defer h.selPolicyMu.Unlock()
	policy, scope := h.cfg.Pool.SelectionPolicy(), h.cfg.Pool.SelectionScope()
	if body.SelectionPolicy != nil {
		policy = strings.TrimSpace(*body.SelectionPolicy)
	}
	if body.SelectionScope != nil {
		scope = strings.TrimSpace(*body.SelectionScope)
	}
	persistPolicy, persistScope := policy, scope
	// Editing the other field must not save an environment override into data.
	if h.cfg.SelectionPolicyLocked {
		persistPolicy = h.readPersistedSelectionPolicy()
	}
	if h.cfg.SelectionScopeLocked {
		persistScope = LoadPersistedSelectionScope(h.cfg.SelectionPolicyDataPath)
	}
	if err := persistSelectionPolicy(h.cfg.SelectionPolicyDataPath, persistPolicy, persistScope); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "persist_failed",
			"failed to persist selection policy: "+err.Error())
		return
	}
	h.cfg.Pool.SetSelectionConfig(policy, scope)
	writeJSON(w, http.StatusOK, h.selectionPolicyState())
}

// readPersistedSelectionPolicy 读 data 持久化值（缺失/损坏/白名单外 → 空串，
// 回落 config 初始值；损坏文件不阻断读路径，下次 PUT 会覆盖）。
func (h *Handler) readPersistedSelectionPolicy() string {
	if h.cfg.SelectionPolicyDataPath == "" {
		return ""
	}
	raw, err := os.ReadFile(h.cfg.SelectionPolicyDataPath)
	if err != nil {
		return ""
	}
	var doc struct {
		SelectionPolicy string `json:"selection_policy"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	v := strings.TrimSpace(doc.SelectionPolicy)
	if !selectionPolicies[v] {
		return ""
	}
	return v
}

// renameForPersist 替换实现的注入点（故障注入测试用）：正常路径即 os.Rename。
var renameForPersist = os.Rename

// persistSelectionPolicy 原子写入：目标同目录**唯一**临时文件（os.CreateTemp，
// 权限 0600）→ 写完整 JSON → fsync → 关闭 → 原子替换目标。全程**绝不先删除
// 旧目标**（未通过验收修复交接 §5.2）：
//   - Linux：os.Rename 即 rename(2)，目标存在时原子覆盖；
//   - Windows：Go 的 os.Rename 走 MoveFileEx(MOVEFILE_REPLACE_EXISTING)，同样原子替换。
//
// 旧实现「固定 path+.tmp、先 os.Remove 旧目标再 rename」有两个致命点：删除后、
// 替换前崩溃/重启会**永久丢旧策略文件**（重启回落 config 初始值）；并发 PUT 争用
// 同一固定 .tmp 互相覆盖。本实现两者皆除：唯一临时文件互不干扰，替换不经过
// "目标不存在"窗口。失败路径清理本次临时文件，旧目标保持原状（内容完整）；
// 运行时策略是否随之保持由调用方负责（selectionPolicyPut 持锁且失败不 Set）。
func persistSelectionPolicy(path, policy string, scope ...string) error {
	fields := map[string]string{
		"selection_policy": policy,
		"updated_by":       "proxy-hub",
	}
	if len(scope) > 0 {
		fields["selection_scope"] = scope[0]
	}
	doc, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	doc = append(doc, '\n')

	dir, base := filepath.Dir(path), filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".tmp-*") // 唯一命名：并发写者不争用同一文件
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// 任何后续失败清理本次临时文件；成功路径 rename 后 tmpName 已不存在，Remove 无害。
	defer func() {
		if err != nil {
			os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(doc); err != nil {
		tmp.Close()
		return err
	}
	// 先落盘数据再替换：替换成功即数据完整，不留"半截内容顶着正式文件名"的形态。
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = renameForPersist(tmpName, path); err != nil {
		return err
	}
	// 目录 fsync 尽力而为：让 rename 本身持久化（Linux 生效；Windows 打开目录
	// Sync 不支持，错误忽略——崩溃一致性按文件系统能力取上限）。
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// selectionPolicyDataPath 由 state.json 路径推导 data/selection-policy.json
// （同目录，与池状态同卷持久化）。
func selectionPolicyDataPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "selection-policy.json")
}

// SelectionPolicyDataPath 导出给 cmd/server 装配用（与内部实现同语义）。
func SelectionPolicyDataPath(stateFile string) string {
	return selectionPolicyDataPath(stateFile)
}

// LoadPersistedSelectionPolicy 导出给 cmd/server 启动加载用：读 data 持久化值，
// 缺失/损坏/白名单外返回空串（调用方回落 config 初始值，不报错不阻断）。
func LoadPersistedSelectionPolicy(dataPath string) string {
	return (&Handler{cfg: Config{SelectionPolicyDataPath: dataPath}}).readPersistedSelectionPolicy()
}

// constantTimeEqual 常量时间字符串比较（与 withAuth 同一防时序侧信道纪律）。
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (h *Handler) selectionScopeSource() (string, string) {
	if h.cfg.SelectionScopeLocked {
		return h.cfg.SelectionScopeEnvValue, "env:WB2A_SELECTION_SCOPE"
	}
	if v := LoadPersistedSelectionScope(h.cfg.SelectionPolicyDataPath); v != "" {
		return v, "data/selection-policy.json"
	}
	return h.cfg.SelectionScopeConfigValue, "config.json"
}
func LoadPersistedSelectionScope(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc struct {
		SelectionScope string `json:"selection_scope"`
	}
	if json.Unmarshal(raw, &doc) != nil || !pool.ValidSelectionScope(doc.SelectionScope) {
		return ""
	}
	return doc.SelectionScope
}
