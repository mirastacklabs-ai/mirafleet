// Package sqlite is the SQLite-backed store for central. It uses the pure-Go
// modernc.org/sqlite driver so the binary stays CGO_ENABLED=0.
package sqlite

import _ "modernc.org/sqlite" // register the "sqlite" database/sql driver
