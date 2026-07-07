package common

// MaskedSystemModelAlias is the user-facing model name shown for tokens whose
// Hidden flag is true. It hides which upstream model a hidden (system) key
// actually calls, keeping the invocation a black box to end users.
const MaskedSystemModelAlias = "知言云策系统调用"

// MaskedModelNameIf returns the masked alias when hidden is true and name is
// non-empty, otherwise returns name unchanged.
//
// This is the unified, zero-DB masking helper. Callers that already hold the
// token Hidden flag — via RelayInfo.TokenHidden or the token_hidden gin context
// key set in TokenAuth / SetupContextForToken — use this directly, avoiding any
// token lookup on the request hot path. It never mutates the caller's data;
// billing and admin-audit paths keep reading the real model name.
func MaskedModelNameIf(hidden bool, name string) string {
	if hidden && name != "" {
		return MaskedSystemModelAlias
	}
	return name
}
