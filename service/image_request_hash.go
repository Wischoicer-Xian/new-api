package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

// CanonicalImageRequestHash returns a stable SHA-256 hex digest of an image
// generation request body. The digest is independent of JSON key order and
// insignificant whitespace, so two requests carrying the same semantic
// content hash equally — which is what lets the idempotency layer tell a
// replay (same key, same hash) from a conflict (same key, different hash).
//
// The canonicalization keeps JSON numbers exact, sorts object keys recursively
// and normalizes equivalent decimal spellings. Array order is preserved and
// therefore participates in the digest, since reordering reference images is
// a semantic change. A nil or empty body is rejected rather than hashed so
// that every empty-body request does not collapse onto one shared digest.
func CanonicalImageRequestHash(body []byte) (string, error) {
	if len(body) == 0 {
		return "", fmt.Errorf("canonicalize image request: empty body")
	}
	var parsed map[string]any
	if err := common.UnmarshalUseNumber(body, &parsed); err != nil {
		return "", fmt.Errorf("canonicalize image request: %w", err)
	}
	var canonical bytes.Buffer
	if err := writeCanonicalJSON(&canonical, parsed); err != nil {
		return "", fmt.Errorf("canonicalize image request: %w", err)
	}
	sum := sha256.Sum256(canonical.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

func writeCanonicalJSON(dst *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		dst.WriteString("null")
	case bool:
		if typed {
			dst.WriteString("true")
		} else {
			dst.WriteString("false")
		}
	case string:
		encoded, err := common.Marshal(typed)
		if err != nil {
			return err
		}
		dst.Write(encoded)
	case json.Number:
		normalized, err := normalizeJSONNumber(string(typed))
		if err != nil {
			return err
		}
		dst.WriteString(normalized)
	case []any:
		dst.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				dst.WriteByte(',')
			}
			if err := writeCanonicalJSON(dst, item); err != nil {
				return err
			}
		}
		dst.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		dst.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				dst.WriteByte(',')
			}
			encodedKey, err := common.Marshal(key)
			if err != nil {
				return err
			}
			dst.Write(encodedKey)
			dst.WriteByte(':')
			if err := writeCanonicalJSON(dst, typed[key]); err != nil {
				return err
			}
		}
		dst.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical JSON value %T", value)
	}
	return nil
}

// normalizeJSONNumber preserves exact decimal value without materializing
// potentially huge exponents. The canonical coefficient has no leading or
// trailing zeroes and carries the remaining decimal scale as an exponent.
func normalizeJSONNumber(value string) (string, error) {
	negative := strings.HasPrefix(value, "-")
	if negative {
		value = value[1:]
	}

	mantissa := value
	exponent := new(big.Int)
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		mantissa = value[:index]
		if _, ok := exponent.SetString(value[index+1:], 10); !ok {
			return "", fmt.Errorf("invalid JSON number %q", value)
		}
	}

	integerPart, fractionalPart := mantissa, ""
	if index := strings.IndexByte(mantissa, '.'); index >= 0 {
		integerPart = mantissa[:index]
		fractionalPart = mantissa[index+1:]
	}
	digits := strings.TrimLeft(integerPart+fractionalPart, "0")
	if digits == "" {
		return "0", nil
	}

	trailingZeroes := len(digits) - len(strings.TrimRight(digits, "0"))
	digits = strings.TrimRight(digits, "0")
	power := new(big.Int).Set(exponent)
	power.Sub(power, big.NewInt(int64(len(fractionalPart))))
	power.Add(power, big.NewInt(int64(trailingZeroes)))

	var normalized strings.Builder
	if negative {
		normalized.WriteByte('-')
	}
	normalized.WriteString(digits)
	if power.Sign() != 0 {
		normalized.WriteByte('e')
		normalized.WriteString(power.String())
	}
	return normalized.String(), nil
}
