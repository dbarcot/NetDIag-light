package config

import (
	"fmt"
	"strings"
)

// FieldError is a problem with one config key, named by its full path
// (e.g. "targets.ping[2].rate").
type FieldError struct {
	Path string
	Msg  string
}

func (e FieldError) Error() string {
	p := e.Path
	if p == "" {
		p = "(root)"
	}
	return p + ": " + e.Msg
}

// ValidationError lists every problem found in a config.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		msgs[i] = f.Error()
	}
	return strings.Join(msgs, "\n")
}

// Has reports whether one of the errors is for path.
func (e *ValidationError) Has(path string) bool {
	for _, f := range e.Fields {
		if f.Path == path {
			return true
		}
	}
	return false
}

type errList struct {
	fields []FieldError
}

func (l *errList) add(path, format string, args ...any) {
	l.fields = append(l.fields, FieldError{Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (l *errList) err() error {
	if len(l.fields) == 0 {
		return nil
	}
	return &ValidationError{Fields: l.fields}
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

func indexPath(base string, i int) string {
	return fmt.Sprintf("%s[%d]", base, i)
}
