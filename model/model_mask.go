package model

// MaskedSystemModelName is the generic alias shown to end users instead of the
// real upstream model name for tokens marked Hidden (知言云策系统账号). Replacing
// the real model keeps the underlying provider opaque to users, so they cannot
// tell which model a hidden system key actually called. All write-side model
// names go through MaskModelNameIfHidden / MaskedModelName so the alias stays
// consistent across logs, quota_data, perf_metrics, tasks, and task responses.
const MaskedSystemModelName = "知言云策系统调用"

// MaskModelNameIfHidden returns the masked alias when hidden is true and name is
// non-empty, otherwise the original name. It centralizes the alias output; callers
// that already hold a Hidden flag (e.g. RelayInfo.TokenHidden injected from the
// gin context) call this directly to avoid a token lookup, while callers with only
// a token id call MaskedModelName.
func MaskModelNameIfHidden(name string, hidden bool) string {
	if hidden && name != "" {
		return MaskedSystemModelName
	}
	return name
}

// MaskedModelName returns the model name to store or return for a given token:
// the masked alias when the token is marked Hidden, otherwise the real name. A
// non-positive token id or empty name is returned unchanged (admin/internal paths
// with no token keep the real model). The Hidden flag is resolved via the cached
// token lookup.
func MaskedModelName(tokenId int, name string) string {
	if tokenId <= 0 || name == "" {
		return name
	}
	token, err := GetTokenById(tokenId)
	if err != nil || token == nil {
		return name
	}
	return MaskModelNameIfHidden(name, token.Hidden)
}

// withRealModelNameAdminInfo returns an "other" map that preserves realModelName
// under the admin-only admin_info key when it was masked (maskedName differs from
// realModelName). The input map is not mutated. formatUserLogs strips
// Other.admin_info for user-facing log queries, while admin queries (GetAllLogs)
// keep it, so admins can still trace the real model used by hidden system tokens
// for troubleshooting (R3 decision 2).
func withRealModelNameAdminInfo(other map[string]interface{}, realModelName, maskedName string) map[string]interface{} {
	if maskedName == realModelName || realModelName == "" {
		return other
	}
	out := make(map[string]interface{}, len(other)+1)
	for k, v := range other {
		out[k] = v
	}
	adminInfo := map[string]interface{}{}
	if prev, ok := out["admin_info"].(map[string]interface{}); ok {
		adminInfo = prev
	}
	adminInfo["real_model_name"] = realModelName
	out["admin_info"] = adminInfo
	return out
}
