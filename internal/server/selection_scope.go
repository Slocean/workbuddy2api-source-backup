package server

import (
	"encoding/json"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// catalogEvidence 单个账号自己的模型目录证据（绝不跨账号共用——A 账号的目录
// 不冒充 B 账号的权限）。
//   - known=true：该账号有可用探测名单；complete=true 表示主路径 /v3/config 与
//     企业端点双双成功（名单完整，缺失条目可作为「该账号不支持」的权威否定）；
//   - complete=false：主路径失败、仅企业端点降级（或窄表形态）——名单不完整，
//     缺失条目不构成支持、也**不构成否定**（2026-10-10 线上实证：v3 瞬时超时 →
//     企业 18 窄表被当完整目录缓存 1h，把真实支持 DeepSeek 的账号整域排除）；
//   - freshSuccess：该账号对此精确模型的新鲜实际成功观测（每账号真实成本台账，
//     6h TTL）——跨域放行的唯一非目录正证据，只证明文本，不编造图片/工具能力。
type catalogEvidence struct {
	known        bool
	complete     bool
	freshSuccess bool
	names        []string // 窄表名单（无能力字段）
	infos        []upstream.ModelInfo
}

// catalogAllows 判定该账号凭自己的证据能否承接该模型请求。crossRealm 表示账号域
// 与请求解析域不同（scope 聚合选号下的跨域候选）：
//   - 目录正命中（富条目/窄表）：真实能力旗标把关，不编造——正证据跨域有效；
//   - 完整目录缺失：权威否定，一票排除（含跨域；仍新鲜的完整证据优先于历史成功）；
//   - 降级/未知目录缺失：不构成支持也不构成否定——同域保留 legacy 语义（按
//     requested realm 放行文本）；跨域仅在**同账号新鲜实际成功观测**时放行文本。
//     主路径失败靠短冷却重试恢复完整性（fetchGlobalModelsOnce/fetchDynamicModels），
//     不以 11102 代替权限发现。
func catalogAllows(e catalogEvidence, model string, image, tools, crossRealm bool) bool {
	for _, mi := range e.infos {
		if mi.ID == model {
			if image && !mi.SupportsImages {
				return false
			}
			if tools && !mi.SupportsToolCall {
				return false
			}
			return true
		}
	}
	for _, id := range e.names {
		if id == model {
			// 窄表只证明文本。
			return !image && !tools
		}
	}
	if e.known && e.complete {
		return false
	}
	if image || tools {
		// 能力无目录证据：不编造、不放行。
		return false
	}
	if crossRealm {
		return e.freshSuccess
	}
	// 同域未知/降级缺失：保留既有 legacy 语义（按 requested realm 放行文本）。
	return true
}

// scopeCompatibility 为一次请求构建「按账号自己的证据」放行闭包：解析出的精确裸
// modelID 必须在该账号自己的目录或新鲜实际成功观测里有据，才进入 earliest_expiry
// 排序的候选集合。证据在 Pool 锁外按账号预取（allow 闭包在 pick 锁内运行，锁内
// 不得发起网络请求）；无证据（探测失败/负缓存/新账号）不默认跨域放行。
func (h *Handler) scopeCompatibility(body []byte, model, requested string) func(*auth.Auth) bool {
	if h.cfg.Pool.SelectionScope() == "" {
		return func(a *auth.Auth) bool { return a.Realm() == requested }
	}
	var req struct {
		Tools     []json.RawMessage `json:"tools"`
		Functions []json.RawMessage `json:"functions"`
	}
	_ = json.Unmarshal(body, &req)
	image, tools := hasImagePart(body), len(req.Tools) > 0 || len(req.Functions) > 0

	evidence := make(map[string]catalogEvidence)
	for _, st := range h.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if a.IsGlobal() {
			// GlobalEnabled=false 时 Realm() 已回落 cn（auth 逃生门），不会进本分支。
			names, infos, complete, known := h.cfg.Upstream.GlobalCatalog(a)
			evidence[st.UID] = catalogEvidence{
				known: known, complete: complete, names: names, infos: infos,
				freshSuccess: h.freshSuccessObserved(st.UID, model),
			}
			continue
		}
		infos, complete, known := h.cnCatalogFor(a)
		evidence[st.UID] = catalogEvidence{
			known: known, complete: complete, infos: infos,
			freshSuccess: h.freshSuccessObserved(st.UID, model),
		}
	}
	return func(a *auth.Auth) bool {
		e, ok := evidence[a.UID]
		if !ok {
			// 预取清单外（新账号等）：缺失证据不默认跨域放行；同域按 legacy 语义。
			return !image && !tools && a.Realm() == requested
		}
		return catalogAllows(e, model, image, tools, a.Realm() != requested)
	}
}

// freshSuccessObserved 报告该账号对此精确模型是否有新鲜的实际成功观测
// （每账号成本台账，6h TTL；免费模型 credit=0 也入账）。只读，不发起上游调用。
func (h *Handler) freshSuccessObserved(uid, model string) bool {
	if h.cfg.Pool == nil {
		return false
	}
	_, ok := h.cfg.Pool.ModelCost(uid, model)
	return ok
}
