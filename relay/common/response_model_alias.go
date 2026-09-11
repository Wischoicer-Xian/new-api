package common

import (
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// GetCallerModelName returns the model name that should be shown to the API caller.
// Priority: Gin context "original_model" -> info.OriginModelName -> info.UpstreamModelName.
func GetCallerModelName(c *gin.Context, info *RelayInfo) string {
	if c != nil {
		if v := c.GetString(string(constant.ContextKeyOriginalModel)); v != "" {
			return v
		}
	}
	if info != nil && info.OriginModelName != "" {
		return info.OriginModelName
	}
	if info != nil {
		return info.UpstreamModelName
	}
	return ""
}

// JSONFieldExists reports whether the given JSON path exists in the raw payload.
func JSONFieldExists(data []byte, path string) bool {
	return len(data) > 0 && gjson.GetBytes(data, path).Exists()
}

// PatchJSONStringFieldRaw rewrites a string field at the given JSON path in a
// raw payload. Returns (patched data, whether a change was made, error).
// No-op when the field is absent, already equals the target value, or the
// target value is empty.
func PatchJSONStringFieldRaw(data []byte, path string, value string) ([]byte, bool, error) {
	if len(data) == 0 || value == "" {
		return data, false, nil
	}

	existing := gjson.GetBytes(data, path)
	if !existing.Exists() || existing.String() == value {
		return data, false, nil
	}

	patched, err := sjson.SetBytes(data, path, value)
	if err != nil {
		return data, false, err
	}
	return patched, true, nil
}

// RewriteCallerModelRaw patches the JSON string field at path to the caller's
// model and marks the response body rewritten. Returns data unchanged when the
// caller model is empty, the field is absent, or the value already matches.
func RewriteCallerModelRaw(c *gin.Context, info *RelayInfo, data []byte, path string) []byte {
	callerModel := GetCallerModelName(c, info)
	if callerModel == "" {
		return data
	}
	patched, changed, err := PatchJSONStringFieldRaw(data, path, callerModel)
	if err != nil || !changed {
		return data
	}
	MarkResponseBodyRewritten(c)
	return patched
}

// PatchTopLevelModelRaw rewrites the top-level "model" field in a raw JSON payload.
// Returns (patched data, whether a change was made, error).
func PatchTopLevelModelRaw(data []byte, callerModel string) ([]byte, bool, error) {
	return PatchJSONStringFieldRaw(data, "model", callerModel)
}

// PatchResponsesEventModelRaw rewrites response.model in a Responses API SSE event payload.
// It applies to any event that contains a response.model field, regardless of event type.
// Returns (patched data, whether a change was made, error).
func PatchResponsesEventModelRaw(data []byte, callerModel string) ([]byte, bool, error) {
	return PatchJSONStringFieldRaw(data, "response.model", callerModel)
}

const responseBodyRewrittenKey = "response_body_rewritten"

// MarkResponseBodyRewritten sets a context flag indicating the response body was modified.
func MarkResponseBodyRewritten(c *gin.Context) {
	if c != nil {
		c.Set(responseBodyRewrittenKey, true)
	}
}

// IsResponseBodyRewritten returns true if the response body was rewritten.
func IsResponseBodyRewritten(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, _ := c.Get(responseBodyRewrittenKey)
	b, ok := v.(bool)
	return ok && b
}
