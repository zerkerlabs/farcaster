package httpapi

import "encoding/json"

// optional is a JSON field that can tell apart three states a plain pointer
// cannot: the key was absent, the key was present and null, or the key carried a
// value.
//
// Why this exists: Go's encoding/json never calls UnmarshalJSON for a key that
// is absent from the object, so Present stays false. For a key that IS present —
// including an explicit null — it calls UnmarshalJSON, and we record whether the
// value was null. A plain pointer (or even a **string) loses this distinction:
// decoding `null` leaves the outer pointer nil, which is indistinguishable from
// an omitted key, so a "clear this field" PATCH silently does nothing.
type optional[T any] struct {
	Present bool // the key appeared in the JSON object, even if its value was null
	Value   *T   // nil when the JSON value was null; non-nil otherwise
}

// UnmarshalJSON records that the key was present and captures null vs a value.
func (o *optional[T]) UnmarshalJSON(data []byte) error {
	o.Present = true
	if string(data) == "null" {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// toUpdatePointer maps the three states onto the **T convention that
// agent.UpdateFields uses for nullable fields: nil = absent (leave unchanged),
// &nil = clear, &&v = set to v.
func (o optional[T]) toUpdatePointer() **T {
	if !o.Present {
		return nil
	}
	// o.Value is already *T (nil = clear, non-nil = set); take its address to
	// hand the store the outer pointer it expects.
	return &o.Value
}
