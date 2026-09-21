package schema

// Pointer constructors for the schema's absent-versus-zero fields.
//
// HistoryEntry.Process, HistoryEntry.OpID, HistoryEntry.Key and Profile.Worlds
// are pointers because their zero values are legitimate observations: process
// 0, op_id 0, the empty key, and an explicit `worlds: 0` all mean something
// different from "the field was absent". Without these helpers every call site
// needs a throwaway local, which is exactly the friction that leads someone to
// "simplify" the field back to a plain int and silently destroy the
// distinction.

// Int returns a pointer to v.
func Int(v int) *int { return &v }

// Int64 returns a pointer to v.
func Int64(v int64) *int64 { return &v }

// Uint64 returns a pointer to v.
func Uint64(v uint64) *uint64 { return &v }

// Str returns a pointer to v.
func Str(v string) *string { return &v }

// Worlds returns a pointer suitable for Profile.Worlds. Pass WorldsUnbounded
// for a profile that runs until its budget expires.
func Worlds(n int) *int { return &n }
