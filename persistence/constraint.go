package persistence

import "strings"

// ConstraintError inspects a database driver error and, when it recognizes a
// constraint violation, returns the matching clean sentinel — never the raw
// driver message, which can leak table/column names and SQL fragments across
// the API boundary. It returns nil when err is not a recognized constraint
// violation, so the caller can wrap it with its own context as usual.
//
// Mapping:
//   - unique / primary-key violations          → [ErrConflict]          (AlreadyExists)
//   - foreign-key and not-null violations       → [ErrPreconditionFailed] (FailedPrecondition)
//
// Matching is string-based and driver-agnostic (SQLite, PostgreSQL, MySQL) so
// the persistence package stays ORM- and driver-neutral and imposes no extra
// dependency. The generated GORM repositories call this in Create/Update before
// falling back to a generic wrapped error.
func ConstraintError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unique constraint failed"), // SQLite
		strings.Contains(msg, "duplicate key value"), // PostgreSQL
		strings.Contains(msg, "violates unique constraint"), // PostgreSQL
		strings.Contains(msg, "duplicate entry"), // MySQL
		strings.Contains(msg, "sqlstate 23505"): // PostgreSQL unique_violation
		return ErrConflict
	case strings.Contains(msg, "foreign key constraint failed"), // SQLite
		strings.Contains(msg, "violates foreign key constraint"), // PostgreSQL
		strings.Contains(msg, "sqlstate 23503"), // PostgreSQL foreign_key_violation
		strings.Contains(msg, "a foreign key constraint fails"): // MySQL
		return ErrPreconditionFailed
	case strings.Contains(msg, "not null constraint failed"), // SQLite
		strings.Contains(msg, "violates not-null constraint"), // PostgreSQL
		strings.Contains(msg, "sqlstate 23502"): // PostgreSQL not_null_violation
		return ErrPreconditionFailed
	}
	return nil
}
