// Package sqlite is the SQLite-backed store for central. It uses the pure-Go
// modernc.org/sqlite driver so the binary stays CGO_ENABLED=0.
package sqlite

import (
	"errors"

	msqlite "modernc.org/sqlite" // also registers the "sqlite" database/sql driver
)

// Extended result codes: SQLITE_CONSTRAINT_PRIMARYKEY and SQLITE_CONSTRAINT_UNIQUE.
const (
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
)

// isUnique reports whether err is a UNIQUE or PRIMARY KEY violation: the only constraint
// errors the store maps to ErrConflict. CHECK, NOT NULL, FOREIGN KEY and trigger aborts are
// not conflicts and are returned as they are.
func isUnique(err error) bool {
	var se *msqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	c := se.Code()
	return c == sqliteConstraintUnique || c == sqliteConstraintPrimaryKey
}

// isBusy reports SQLITE_BUSY or SQLITE_LOCKED (primary codes 5 and 6).
func isBusy(err error) bool {
	var se *msqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	c := se.Code() & 0xff
	return c == 5 || c == 6
}
