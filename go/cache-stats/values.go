package cachestats

import (
	"reflect"
	"sort"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// The SDK hands extension handlers decoded `map[string]any` payloads: a Go map
// has no key order, so a value's original JSON key order is not recoverable
// from what the handler receives. The walker therefore visits a plain map's
// keys in sorted order and keeps true insertion order for the ordered object
// the SDK also exposes (sdk.OrderedObject), which the tests use to reproduce
// the TS `Object.keys` order and prove the fingerprint is order-sensitive.
// See README.md, "TS -> Go 差异".

// jsonObject is a JSON object the walker can enumerate: lookup reads one own
// property, keys returns the property names in the order the walker hashes
// them.
type jsonObject interface {
	lookup(key string) (any, bool)
	keys() []string
}

// plainObject is a decoded map: keys() returns them sorted, because the map
// itself carries no order.
type plainObject map[string]any

func (o plainObject) lookup(key string) (any, bool) {
	value, ok := o[key]
	return value, ok
}

func (o plainObject) keys() []string {
	names := make([]string, 0, len(o))
	for name := range o {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// orderedObject keeps *sdk.OrderedObject's JSON key order.
type orderedObject struct{ inner *sdk.OrderedObject }

func (o orderedObject) lookup(key string) (any, bool) { return o.inner.Get(key) }

func (o orderedObject) keys() []string { return o.inner.Keys() }

// asObject reports whether value is a JSON object (not an array, not a scalar)
// and returns the enumerable view of it.
func asObject(value any) (jsonObject, bool) {
	switch typed := value.(type) {
	case nil:
		return nil, false
	case plainObject:
		return typed, true
	case orderedObject:
		return typed, true
	case jsonObject:
		return typed, true
	case map[string]any:
		return plainObject(typed), true
	case *sdk.OrderedObject:
		if typed == nil {
			return nil, false
		}
		return orderedObject{inner: typed}, true
	}
	return nil, false
}

// objectLookup reads an own property of a JSON object value.
func objectLookup(value any, key string) (any, bool) {
	obj, ok := asObject(value)
	if !ok {
		return nil, false
	}
	return obj.lookup(key)
}

// isArrayValue is Array.isArray(value) for a decoded JSON value.
func isArrayValue(value any) bool {
	_, ok := value.([]any)
	return ok
}

// pointerOf is the backing pointer of a map or slice, used to detect a cycle in
// the fingerprint walk (the TS WeakSet's object identity).
func pointerOf(value any) uintptr {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Map, reflect.Slice:
		return reflected.Pointer()
	}
	return 0
}

// sortStrings sorts keys for a stable display order.
func sortStrings(values []string) { sort.Strings(values) }
