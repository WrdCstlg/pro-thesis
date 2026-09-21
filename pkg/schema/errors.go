package schema

import (
	"fmt"
	"strings"
)

// ValidationError is one problem found in a document, addressed by a dotted
// path so a user can find it without counting lines.
type ValidationError struct {
	// Path is a dotted/indexed field path, e.g. "harness.nodes[2].id".
	Path string
	// Msg says what is wrong and, where possible, what to write instead.
	Msg string
}

func (e ValidationError) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return e.Path + ": " + e.Msg
}

// ValidationErrors collects every problem in one document so a user fixes the
// whole file in one pass rather than one error per run. A non-nil result from
// any Validate method in this package maps to ExitConfigError.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	switch len(e) {
	case 0:
		return "no validation errors"
	case 1:
		return e[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d validation errors:", len(e))
	for _, v := range e {
		b.WriteString("\n  - ")
		b.WriteString(v.Error())
	}
	return b.String()
}

// Add appends a formatted problem at path.
func (e *ValidationErrors) Add(path, format string, a ...any) {
	*e = append(*e, ValidationError{Path: path, Msg: fmt.Sprintf(format, a...)})
}

// Merge folds another error into the collection. A ValidationErrors is
// flattened; anything else is recorded verbatim at path.
func (e *ValidationErrors) Merge(path string, err error) {
	if err == nil {
		return
	}
	if ve, ok := err.(ValidationErrors); ok {
		*e = append(*e, ve...)
		return
	}
	e.Add(path, "%s", err.Error())
}

// OrNil returns nil when there are no problems, so callers can `return
// errs.OrNil()` without a length check. It deliberately returns the concrete
// ValidationErrors type only when non-empty, so a nil error is a true nil.
func (e ValidationErrors) OrNil() error {
	if len(e) == 0 {
		return nil
	}
	return e
}

// indexPath renders "prefix[i]" for slice element paths.
func indexPath(prefix string, i int) string {
	return fmt.Sprintf("%s[%d]", prefix, i)
}

// keyPath renders "prefix.key" for map entry paths, quoting keys that would be
// ambiguous.
func keyPath(prefix, key string) string {
	if key == "" || strings.ContainsAny(key, ". []") {
		return fmt.Sprintf("%s[%q]", prefix, key)
	}
	return prefix + "." + key
}
