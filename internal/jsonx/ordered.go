// Package jsonx keeps JSON object key order through decode, trim and encode,
// so documents print with "schema" first and fields in their declared order.
package jsonx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Member is one key/value pair of an Object.
type Member struct {
	Key   string
	Value any
}

// Object is a JSON object that remembers key order.
type Object []Member

// Get returns the value for key and whether it exists.
func (o Object) Get(key string) (any, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// MarshalJSON encodes the object with keys in stored order.
func (o Object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.Key)
		b.Write(k)
		b.WriteByte(':')
		v, err := marshalNoEscape(m.Value)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// FromValue converts any JSON-marshalable value into ordered form
// (Object, []any, json.Number, string, bool or nil).
func FromValue(v any) (any, error) {
	raw, err := marshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return decodeValue(dec)
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := Object{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("jsonx: object key is %T", kt)
				}
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				obj = append(obj, Member{key, val})
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("jsonx: unexpected delimiter %v", t)
	default:
		return tok, nil
	}
}

// SchemaFirst moves a top-level "schema" member to the front.
func SchemaFirst(v any) any {
	obj, ok := v.(Object)
	if !ok {
		return v
	}
	for i, m := range obj {
		if m.Key == "schema" && i > 0 {
			out := Object{m}
			out = append(out, obj[:i]...)
			return append(out, obj[i+1:]...)
		}
	}
	return obj
}

// Encode writes v as JSON. indent=true gives two-space indentation and a
// trailing newline; false gives one compact line and a trailing newline.
func Encode(w io.Writer, v any, indent bool) error {
	raw, err := marshalNoEscape(v)
	if err != nil {
		return err
	}
	if indent {
		var b bytes.Buffer
		if err := json.Indent(&b, raw, "", "  "); err != nil {
			return err
		}
		raw = b.Bytes()
	}
	_, err = w.Write(append(raw, '\n'))
	return err
}

// ftree selects fields: a nil subtree means "keep everything below".
type ftree map[string]ftree

// Trim keeps only the named dotted paths (plus "schema") in doc. A path
// segment that lands on an array applies to every element.
func Trim(doc any, paths []string) any {
	tree := ftree{}
	for _, p := range paths {
		node := tree
		segs := strings.Split(p, ".")
		for i, seg := range segs {
			sub, exists := node[seg]
			if exists && sub == nil {
				break // an ancestor already keeps everything below
			}
			if i == len(segs)-1 {
				node[seg] = nil
				break
			}
			if !exists {
				sub = ftree{}
				node[seg] = sub
			}
			node = sub
		}
	}
	out := trim(doc, tree)
	if obj, ok := out.(Object); ok {
		if src, ok := doc.(Object); ok {
			if s, ok := src.Get("schema"); ok {
				if _, has := obj.Get("schema"); !has {
					obj = append(Object{{"schema", s}}, obj...)
				}
			}
		}
		return obj
	}
	return out
}

func trim(v any, tree ftree) any {
	if tree == nil {
		return v
	}
	switch x := v.(type) {
	case Object:
		out := Object{}
		for _, m := range x {
			sub, ok := tree[m.Key]
			if !ok {
				continue
			}
			out = append(out, Member{m.Key, trim(m.Value, sub)})
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = trim(e, tree)
		}
		return out
	default:
		return v
	}
}
