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
	return selectionPolicySnapshot{
		Supported: true,
		Current:   current,
		Effective: h.cfg.Pool.SelectionPolicy(),
		Source:    source,
		Version:   ServiceName + "/" + selectionPolicyProtoVersion,
	}
}

func (h *Handler) selectionPolicyGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.selectionPolicyState())
}

// selectionPolicyPut 校验 → 锁定判定 → 串行互斥下「持久化 → 运行时生效」，
// 任何一步失败都不改变现状（原子写：唯一临时文件 + 不删目标的替换，§5）。
// 成功后 effective 立即为新值。
func (h *Handler) selectionPolicyPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SelectionPolicy string `json:"selection_policy"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"body must be a JSON object with selection_policy")
		return
	}
	policy := strings.TrimSpace(body.SelectionPolicy)
	if !selectionPolicies[policy] {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_selection_policy",
			"selection_policy must be one of: balanced, cheapest, earliest_expiry, expiry_then_cheapest, cheapest_then_expiry")
		return
	}
	if h.cfg.SelectionPolicyLocked {
		// 部署配置锁定：环境变量优先级最高，运行期改不动（Hub 显示"由部署配置锁定"）。
		writeJSON(w, http.StatusConflict, map[string]string{
			"detail": "selection policy is locked by deployment config (WB2A_SELECTION_POLICY)",
		})
		return
	}
	if h.cfg.SelectionPolicyDataPath == "" {
		writeOpenAIError(w, http.StatusInternalServerError, "persist_unavailable",
			"data path not configured; cannot persist selection policy")
		return
	}
	if err := persistSelectionPolicy(h.cfg.SelectionPolicyDataPath, policy); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "persist_failed",
			"failed to persist selection policy: "+err.Error())
		return
	}
	h.cfg.Pool.SetSelectionPolicy(policy)
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

// persistSelectionPolicy 原子写入：同目录临时文件 + rename（Windows 上 rename
// 目标存在时 os.Rename 失败，先删旧文件再 rename——窗口内由临时文件的完整性保证）。
func persistSelectionPolicy(path, policy string) error {
	doc, err := json.MarshalIndent(map[string]string{
		"selection_policy": policy,
		"updated_by":       "proxy-hub",
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(doc, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
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
