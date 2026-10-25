package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// Paths are the directories used for config lookup and save-config.
// Either may be empty when unknown.
type Paths struct {
	ExeDir  string // directory of the running exe
	AppData string // %APPDATA%
}

// SourceDefaults is the source name reported when no file was found.
const SourceDefaults = "built-in defaults"

func (p Paths) exeFile() string {
	if p.ExeDir == "" {
		return ""
	}
	return filepath.Join(p.ExeDir, FileName)
}

func (p Paths) appDataFile() string {
	if p.AppData == "" {
		return ""
	}
	return filepath.Join(p.AppData, "netdiag", FileName)
}

// Load finds, merges and validates the config. Lookup order (SPEC §4):
// explicit path → netdiag.json next to the exe → %APPDATA%\netdiag\netdiag.json
// → built-in defaults. It returns the config and its source (a path or
// SourceDefaults). Any returned error is a config error (exit code 2).
func Load(fsys FS, explicit string, paths Paths) (*Config, string, error) {
	if explicit != "" {
		data, err := fsys.ReadFile(explicit)
		if err != nil {
			return nil, explicit, fmt.Errorf("cannot read config: %w", err)
		}
		c, err := Parse(data)
		return c, explicit, err
	}
	for _, p := range []string{paths.exeFile(), paths.appDataFile()} {
		if p == "" {
			continue
		}
		data, err := fsys.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, p, fmt.Errorf("cannot read config: %w", err)
		}
		c, err := Parse(data)
		return c, p, err
	}
	return Default(), SourceDefaults, nil
}

// Parse merges a (possibly partial) user config over the defaults and
// validates the result.
func Parse(data []byte) (*Config, error) {
	raw, err := decodeRaw(data)
	if err != nil {
		return nil, err
	}
	var errs errList
	checkShape(raw, reflect.TypeFor[Config](), "", &errs)
	if err := errs.err(); err != nil {
		return nil, err
	}
	var missing errList
	checkRequired(raw, &missing)
	applyEntryDefaults(raw)
	merged, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	c := Default()
	// Unmarshalling onto the defaults merges objects key by key. Arrays set
	// in the file are cleared first: json reuses existing slice elements, so
	// entries would otherwise inherit fields from the default entries.
	clearSetSlices(reflect.ValueOf(c).Elem(), raw)
	if err := json.Unmarshal(merged, c); err != nil {
		return nil, err
	}
	if err := withRequired(missing.fields, c.Validate()); err != nil {
		return nil, err
	}
	c.normalize()
	return c, nil
}

// requiredKeys are the keys every user-supplied entry of these arrays must
// set; there is no sensible default for them.
var requiredKeys = []struct {
	section, array string
	keys           []string
}{
	{"targets", "ping", []string{"name", "host"}},
	{"targets", "tcp", []string{"name", "host"}},
	{"targets", "http_check", []string{"url", "expect_status"}},
}

func checkRequired(raw map[string]any, errs *errList) {
	for _, r := range requiredKeys {
		section, _ := raw[r.section].(map[string]any)
		list, _ := section[r.array].([]any)
		for i, e := range list {
			m, _ := e.(map[string]any)
			for _, k := range r.keys {
				if _, ok := m[k]; !ok {
					errs.add(indexPath(r.section+"."+r.array, i)+"."+k, "required")
				}
			}
		}
	}
}

// withRequired combines "required" errors with validation errors, dropping
// the validation error for a missing key (it would only describe the zero
// value, e.g. "must be between 100 and 599, got 0").
func withRequired(missing []FieldError, verr error) error {
	var ve *ValidationError
	if verr != nil && !errors.As(verr, &ve) {
		return verr
	}
	fields := append([]FieldError(nil), missing...)
	if ve != nil {
		skip := make(map[string]bool, len(missing))
		for _, f := range missing {
			skip[f.Path] = true
		}
		for _, f := range ve.Fields {
			if !skip[f.Path] {
				fields = append(fields, f)
			}
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return &ValidationError{Fields: fields}
}

func decodeRaw(data []byte) (map[string]any, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, syntaxError(data, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("invalid JSON: unexpected data after the top-level object")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &ValidationError{Fields: []FieldError{{Msg: "expected an object, got " + jsonKind(v)}}}
	}
	return m, nil
}

func syntaxError(data []byte, err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		line, col := lineCol(data, se.Offset)
		return fmt.Errorf("invalid JSON at line %d, column %d: %v", line, col, err)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("invalid JSON: unexpected end of file")
	}
	return fmt.Errorf("invalid JSON: %v", err)
}

func lineCol(data []byte, offset int64) (int, int) {
	line, col := 1, 1
	for i := int64(0); i < offset-1 && i < int64(len(data)); i++ {
		if data[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// checkShape rejects unknown keys and wrong JSON types, naming each by path.
func checkShape(v any, t reflect.Type, path string, errs *errList) {
	if v == nil {
		errs.add(path, "expected %s, got null", typeName(t))
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			errs.add(path, "expected object, got %s", jsonKind(v))
			return
		}
		fields := jsonFields(t)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ft, ok := fields[k]
			if !ok {
				errs.add(joinPath(path, k), "unknown key")
				continue
			}
			checkShape(m[k], ft, joinPath(path, k), errs)
		}
	case reflect.Slice:
		a, ok := v.([]any)
		if !ok {
			errs.add(path, "expected array, got %s", jsonKind(v))
			return
		}
		for i, e := range a {
			checkShape(e, t.Elem(), indexPath(path, i), errs)
		}
	case reflect.String:
		if _, ok := v.(string); !ok {
			errs.add(path, "expected string, got %s", jsonKind(v))
		}
	case reflect.Bool:
		if _, ok := v.(bool); !ok {
			errs.add(path, "expected boolean, got %s", jsonKind(v))
		}
	case reflect.Int:
		n, ok := v.(json.Number)
		if !ok {
			errs.add(path, "expected integer, got %s", jsonKind(v))
			return
		}
		if _, err := n.Int64(); err != nil {
			errs.add(path, "expected integer, got %s", n)
		}
	case reflect.Float64:
		n, ok := v.(json.Number)
		if !ok {
			errs.add(path, "expected number, got %s", jsonKind(v))
			return
		}
		if _, err := n.Float64(); err != nil {
			errs.add(path, "expected number, got %s", n)
		}
	default:
		panic("config: unsupported field type " + t.String())
	}
}

func jsonFields(t reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		fields[jsonName(f)] = f.Type
	}
	return fields
}

func jsonName(f reflect.StructField) string {
	tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if tag == "" {
		return f.Name
	}
	return tag
}

func typeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Struct:
		return "object"
	case reflect.Slice:
		return "array"
	case reflect.Int:
		return "integer"
	case reflect.Float64:
		return "number"
	case reflect.Bool:
		return "boolean"
	default:
		return t.Kind().String()
	}
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// clearSetSlices sets every slice field of rv that raw (shape-checked
// against rv's type) contains to nil, recursing into nested objects.
func clearSetSlices(rv reflect.Value, raw map[string]any) {
	for i := range rv.NumField() {
		sub, ok := raw[jsonName(rv.Type().Field(i))]
		if !ok {
			continue
		}
		f := rv.Field(i)
		switch f.Kind() {
		case reflect.Slice:
			f.SetZero()
		case reflect.Struct:
			if m, ok := sub.(map[string]any); ok {
				clearSetSlices(f, m)
			}
		}
	}
}

// applyEntryDefaults fills per-entry defaults of array elements, which
// cannot come from defaults.json because the default arrays are empty.
func applyEntryDefaults(raw map[string]any) {
	targets, _ := raw["targets"].(map[string]any)
	for _, key := range []string{"ping", "tcp"} {
		list, _ := targets[key].([]any)
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				if _, set := m["rate"]; !set {
					m["rate"] = json.Number("1")
				}
			}
		}
	}
}
