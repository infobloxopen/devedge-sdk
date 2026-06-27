package iamv1_test

import (
	"database/sql"
	"database/sql/driver"
	"sync"
)

// sqlite3Registrar bridges modernc.org/sqlite (which registers as "sqlite") to
// the "sqlite3" driver name that entgo's ent.Open expects (dialect.SQLite = "sqlite3").
//
// We re-export the already-registered driver under the second name rather than
// importing modernc directly here, so the registration in the relationship test
// (which blank-imports modernc.org/sqlite) happens first via init() ordering.
var _registerSQLite3Once sync.Once

func init() {
	_registerSQLite3Once.Do(func() {
		// "sqlite" is registered by modernc.org/sqlite's own init(). Open a
		// zero-byte in-memory DB to get the driver object, then re-register it
		// under the "sqlite3" alias that entgo uses.
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			panic("fleetv1 test setup: open sqlite driver: " + err.Error())
		}
		drv := db.Driver()
		_ = db.Close()

		for _, name := range sql.Drivers() {
			if name == "sqlite3" {
				return // already registered (e.g. mattn/go-sqlite3 pulled in transitively)
			}
		}
		sql.Register("sqlite3", drv.(driver.Driver))
	})
}
