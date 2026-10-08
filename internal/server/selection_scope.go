package server

import (
	"encoding/json"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// Unknown catalogs retain legacy routing in the requested realm. Crossing realms
// requires positive model/capability evidence; 11102 remains a per-account backstop.
func (h *Handler) scopeCompatibility(body []byte, model, requested string) func(*auth.Auth) bool {
	if h.cfg.Pool.SelectionScope() == "" {
		return func(a *auth.Auth) bool { return a.Realm() == requested }
	}
	cn := h.fetchDynamicModels()
	names, acct := h.fetchGlobalModels()
	var global []upstream.ModelInfo
	if acct != nil {
		global = h.cfg.Upstream.FetchGlobalModelInfos(acct)
	}
	var req struct {
		Tools     []json.RawMessage `json:"tools"`
		Functions []json.RawMessage `json:"functions"`
	}
	_ = json.Unmarshal(body, &req)
	return catalogCompatibility(model, requested, hasImagePart(body), len(req.Tools) > 0 || len(req.Functions) > 0, cn, names, global)
}

func catalogCompatibility(model, requested string, image, tools bool, cn []upstream.ModelInfo, globalNames []string, global []upstream.ModelInfo) func(*auth.Auth) bool {
	return func(a *auth.Auth) bool {
		realm := a.Realm()
		infos := cn
		known, found := len(cn) > 0, false
		if realm == "global" {
			infos = global
			known = len(globalNames) > 0
			for _, id := range globalNames {
				if id == model {
					found = true
					break
				}
			}
		}
		for _, mi := range infos {
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
		if !known {
			return realm == requested
		}
		// A narrow name-only catalog proves text support, but not multimodal/tools.
		return found && !image && !tools
	}
}
