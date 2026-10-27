package config

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// strictUnknownFields reports EVERY object key in data that a strict
// (DisallowUnknownFields) decode of root would reject, in deterministic
// (sorted, depth-first) order — AUD-75 / NENYA-303: the previous
// single-decode probe stopped at the first unknown field, so a file with
// several typos named only one. Acceptance mirrors encoding/json exactly:
// a key is known when it matches a struct field's json name exactly or
// case-insensitively (including fields promoted from embedded structs), and
// subtrees of types implementing json.Unmarshaler are opaque — the strict
// decoder hands them to the custom unmarshaler and reports nothing inside,
// so neither does the walker. A document that does not parse (or is not an
// object at some level the type expects) yields nothing: the lenient decode
// is the load authority and already handled that.
func strictUnknownFields(data []byte, root reflect.Type) []string {
	var tree any
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil
	}
	w := &unknownFieldWalker{}
	var out []string
	w.walkUnknownFields(tree, root, "", &out)
	return out
}

// walker caches the known-key expansion per struct type; a config load walks
// the same nested types repeatedly (every agent, every provider).
type unknownFieldWalker struct {
	known map[reflect.Type]map[string]reflect.Type
}

// walkUnknownFields compares node (a decoded JSON value) against t and
// appends the dotted path of every unknown key to out.
func (w *unknownFieldWalker) walkUnknownFields(node any, t reflect.Type, path string, out *[]string) {
	t = w.deref(t)
	if t == nil {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		w.walkStruct(node, t, path, out)
	case reflect.Map:
		// Any key is accepted; the value type still constrains the values.
		if obj, ok := node.(map[string]any); ok {
			vt := w.deref(t.Elem())
			for _, key := range sortedKeys(obj) {
				w.walkUnknownFields(obj[key], vt, path+"."+key, out)
			}
		}
	case reflect.Slice, reflect.Array:
		items, ok := node.([]any)
		if !ok {
			return
		}
		et := w.deref(t.Elem())
		for _, item := range items {
			w.walkUnknownFields(item, et, path+"[]", out)
		}
	}
}

// walkStruct checks every key of a decoded JSON object against the struct's
// known field names and recurses into the values of known fields.
func (w *unknownFieldWalker) walkStruct(node any, t reflect.Type, path string, out *[]string) {
	obj, ok := node.(map[string]any)
	if !ok {
		return
	}
	known := w.knownFields(t)
	for _, key := range sortedKeys(obj) {
		ft, ok := lookupField(known, key)
		if !ok {
			*out = append(*out, strings.TrimPrefix(path+"."+key, "."))
		}
		w.walkUnknownFields(obj[key], ft, path+"."+key, out)
	}
}

// knownFields expands t's JSON-decodable fields into name → type, including
// fields promoted from embedded structs (an anonymous struct or
// pointer-to-struct field without a json name contributes its own fields).
func (w *unknownFieldWalker) knownFields(t reflect.Type) map[string]reflect.Type {
	if w.known == nil {
		w.known = map[reflect.Type]map[string]reflect.Type{}
	}
	if m, ok := w.known[t]; ok {
		return m
	}
	// Seed before recursion: a recursive type (self-referential via pointers)
	// terminates on the cache hit.
	m := map[string]reflect.Type{}
	w.known[t] = m
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" && f.Anonymous {
			if w.addEmbedded(m, f) {
				continue
			}
			name = f.Type.Name()
		}
		if name == "" {
			name = f.Name
		}
		if _, dup := m[name]; !dup {
			m[name] = f.Type
		}
	}
	return m
}

// addEmbedded expands one anonymous field into m when it promotes its own
// fields (an embedded struct or pointer-to-struct without a json name).
// It reports whether the expansion happened; a non-struct or opaque embedded
// type is a named field spelled by its type name instead.
func (w *unknownFieldWalker) addEmbedded(m map[string]reflect.Type, f reflect.StructField) bool {
	ft := f.Type
	if ft.Kind() == reflect.Pointer {
		ft = ft.Elem()
	}
	if ft.Kind() != reflect.Struct || implementsUnmarshaler(f.Type) {
		return false
	}
	for k, v := range w.knownFields(ft) {
		if _, dup := m[k]; !dup {
			m[k] = v
		}
	}
	return true
}

// deref follows pointers and stops at interface kinds (any JSON is accepted,
// and the strict decoder reports nothing inside).
func (w *unknownFieldWalker) deref(t reflect.Type) reflect.Type {
	for {
		if t == nil {
			return nil
		}
		if implementsUnmarshaler(t) || t.Kind() == reflect.Interface {
			return nil
		}
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
			continue
		}
		return t
	}
}

// implementsUnmarshaler reports whether t (or *t) implements json.Unmarshaler.
func implementsUnmarshaler(t reflect.Type) bool {
	um := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	return t.Implements(um) || reflect.PointerTo(t).Implements(um)
}

// lookupField resolves a JSON object key to a field type: exact name first,
// then encoding/json's case-insensitive fallback.
func lookupField(known map[string]reflect.Type, key string) (reflect.Type, bool) {
	if ft, ok := known[key]; ok {
		return ft, true
	}
	for name, ft := range known {
		if strings.EqualFold(name, key) {
			return ft, true
		}
	}
	return nil, false
}

func sortedKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
