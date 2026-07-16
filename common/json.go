package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// UnmarshalUseNumber decodes JSON without converting numbers through float64.
// It is intended for canonicalization and other paths where distinct large
// integer values must remain distinguishable.
func UnmarshalUseNumber(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

// UnmarshalStrict rejects unknown object fields in addition to malformed JSON.
// Map keys remain caller-defined and must be validated by the owning domain.
func UnmarshalStrict(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func UnmarshalJsonStr(data string, v any) error {
	return json.Unmarshal(StringToByteSlice(data), v)
}

func DecodeJson(reader io.Reader, v any) error {
	return json.NewDecoder(reader).Decode(v)
}

func Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func GetJsonType(data json.RawMessage) string {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return "unknown"
	}
	firstChar := trimmed[0]
	switch firstChar {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// JsonRawMessageToString returns JSON strings as their decoded value and other JSON values as raw text.
func JsonRawMessageToString(data json.RawMessage) string {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] != '"' {
		return string(trimmed)
	}
	var value string
	if err := Unmarshal(trimmed, &value); err != nil {
		return string(trimmed)
	}
	return value
}

// ErrJSONObjectExpected is returned by AssertJSONObjectNoDuplicateKeys when the
// top-level JSON value is not an object.
var ErrJSONObjectExpected = errors.New("json: top-level value is not an object")

// ErrJSONMalformed is returned by AssertJSONObjectNoDuplicateKeys when the JSON
// cannot be fully tokenized (truncated, trailing data, or a non-string key).
var ErrJSONMalformed = errors.New("json: malformed")

// DuplicateJSONKeyError reports an object key that repeats within one object
// scope. Keys compare equal after JSON unescaping, so "a" collides with
// "a"; a key reused in a sibling object scope does not.
type DuplicateJSONKeyError struct {
	Key string
}

func (e *DuplicateJSONKeyError) Error() string {
	return fmt.Sprintf("json: duplicate key %q", e.Key)
}

// AssertJSONObjectNoDuplicateKeys validates that data is a single JSON object
// whose objects contain no duplicate keys at any nesting level. encoding/json
// silently keeps the last value for a repeated key and collapses escaped key
// variants to the same string, so strict request schemas must call this before
// typed decoding — DisallowUnknownFields does not detect either case.
//
// A key may legitimately repeat across sibling object scopes (e.g. the same
// field name in two elements of an array); only a repeat within one scope
// fails. The function does not validate field names or values against any
// schema; it only guarantees structural uniqueness.
func AssertJSONObjectNoDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return ErrJSONMalformed
	}
	open, ok := tok.(json.Delim)
	if !ok || open != '{' {
		return ErrJSONObjectExpected
	}
	stack := []*keyScope{{isObject: true, seen: make(map[string]struct{})}}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if len(stack) != 0 {
				return ErrJSONMalformed
			}
			return nil
		}
		if err != nil {
			return ErrJSONMalformed
		}
		if len(stack) == 0 {
			// The top-level object already closed; any further token is trailing data.
			return ErrJSONMalformed
		}
		top := stack[len(stack)-1]
		if delim, isDelim := tok.(json.Delim); isDelim {
			switch delim {
			case '{', '[':
				scope := &keyScope{isObject: delim == '{'}
				if scope.isObject {
					scope.seen = make(map[string]struct{})
				}
				stack = append(stack, scope)
				if top.isObject {
					top.awaitingValue = false
				}
			case '}', ']':
				stack = stack[:len(stack)-1]
			}
			continue
		}
		if top.isObject && !top.awaitingValue {
			key, ok := tok.(string)
			if !ok {
				return ErrJSONMalformed
			}
			if _, dup := top.seen[key]; dup {
				return &DuplicateJSONKeyError{Key: key}
			}
			top.seen[key] = struct{}{}
			top.awaitingValue = true
			continue
		}
		if top.isObject {
			top.awaitingValue = false
		}
	}
}

type keyScope struct {
	isObject      bool
	seen          map[string]struct{}
	awaitingValue bool
}
