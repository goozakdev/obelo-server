package schemagen

import (
	"bytes"
	"encoding/json"
)

// An ordered JSON object, because a schema is READ by people and a Go map would
// alphabetize every keyword — putting "additionalProperties" above "type" and
// scattering a struct's fields out of wire order. Key order here is the order the
// generator writes keys in, which is the order the Go declaration has them in, so
// a diff of the checked-in file reads like a diff of the Go file.
type object struct {
	keys []string
	vals []any
}

func obj() *object { return &object{} }

// set appends a key. A repeated key overwrites in place, keeping its position, so
// a caller that refines a default cannot silently emit a duplicate key.
func (o *object) set(key string, v any) *object {
	for i, k := range o.keys {
		if k == key {
			o.vals[i] = v
			return o
		}
	}
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, v)
	return o
}

// setIf appends a key only when the string is non-empty, so an absent optional
// annotation leaves no empty key behind.
func (o *object) setIf(key, v string) *object {
	if v != "" {
		o.set(key, v)
	}
	return o
}

func (o *object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := marshalCompact(k)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		vb, err := marshalCompact(o.vals[i])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// marshalCompact marshals WITHOUT HTML escaping. encoding/json escapes <, > and &
// by default, which would turn a description containing "A & B" into "&" —
// legal JSON that reads as line noise in a file a Plugin author is expected to
// read. The outer encoder compacts a Marshaler's output rather than re-encoding
// it, so escaping has to be switched off here, at every level, not only at the
// top.
func marshalCompact(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// encodeIndented writes the finished document the way it is checked in: two-space
// indent, no HTML escaping, one trailing newline. The staleness test compares
// these bytes to the file byte for byte, so this function is the only place the
// file's formatting is decided.
func encodeIndented(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
