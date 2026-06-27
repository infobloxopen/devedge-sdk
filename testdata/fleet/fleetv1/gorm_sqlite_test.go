package fleetv1_test

// gorm_sqlite_test.go — a self-contained GORM SQLite dialector for the fleet
// GORM tests, mirroring the inline dialector the apikey fixtures use. We cannot
// import gorm.io/driver/sqlite (it blank-imports the CGo mattn driver, which
// would clash with the modernc.org/sqlite "sqlite3" shim the ent tests register
// in sqlite_test.go); this uses only gorm.io/gorm sub-packages plus the
// already-registered "sqlite3" driver.

import (
	"context"
	"database/sql"
	"strconv"

	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

const gormTestSQLiteDriverName = "sqlite3"

type gormSQLiteDialector struct {
	dsn  string
	conn gorm.ConnPool
}

// openTestSQLite returns a GORM dialector over the "sqlite3" driver (registered
// in sqlite_test.go's init from modernc.org/sqlite).
func openTestSQLite(dsn string) gorm.Dialector {
	return &gormSQLiteDialector{dsn: dsn}
}

func (d *gormSQLiteDialector) Name() string { return "sqlite" }

func (d *gormSQLiteDialector) Initialize(db *gorm.DB) error {
	if d.conn != nil {
		db.ConnPool = d.conn
	} else {
		sqlDB, err := sql.Open(gormTestSQLiteDriverName, d.dsn)
		if err != nil {
			return err
		}
		db.ConnPool = sqlDB
	}

	var version string
	if err := db.ConnPool.QueryRowContext(context.Background(), "select sqlite_version()").Scan(&version); err != nil {
		return err
	}
	if gormSQLiteCompareVersion(version, "3.35.0") >= 0 {
		callbacks.RegisterDefaultCallbacks(db, &callbacks.Config{
			CreateClauses:        []string{"INSERT", "VALUES", "ON CONFLICT", "RETURNING"},
			UpdateClauses:        []string{"UPDATE", "SET", "FROM", "WHERE", "RETURNING"},
			DeleteClauses:        []string{"DELETE", "FROM", "WHERE", "RETURNING"},
			LastInsertIDReversed: true,
		})
	} else {
		callbacks.RegisterDefaultCallbacks(db, &callbacks.Config{LastInsertIDReversed: true})
	}

	for k, v := range d.clauseBuilders() {
		if _, ok := db.ClauseBuilders[k]; !ok {
			db.ClauseBuilders[k] = v
		}
	}
	return nil
}

func (d *gormSQLiteDialector) clauseBuilders() map[string]clause.ClauseBuilder {
	return map[string]clause.ClauseBuilder{
		"INSERT": func(c clause.Clause, builder clause.Builder) {
			if insert, ok := c.Expression.(clause.Insert); ok {
				if stmt, ok := builder.(*gorm.Statement); ok {
					stmt.WriteString("INSERT ")
					if insert.Modifier != "" {
						stmt.WriteString(insert.Modifier)
						stmt.WriteByte(' ')
					}
					stmt.WriteString("INTO ")
					if insert.Table.Name == "" {
						stmt.WriteQuoted(stmt.Table)
					} else {
						stmt.WriteQuoted(insert.Table)
					}
					return
				}
			}
			c.Build(builder)
		},
		"LIMIT": func(c clause.Clause, builder clause.Builder) {
			if limit, ok := c.Expression.(clause.Limit); ok {
				lmt := -1
				if limit.Limit != nil && *limit.Limit >= 0 {
					lmt = *limit.Limit
				}
				if lmt >= 0 || limit.Offset > 0 {
					builder.WriteString("LIMIT ")
					builder.WriteString(strconv.Itoa(lmt))
				}
				if limit.Offset > 0 {
					builder.WriteString(" OFFSET ")
					builder.WriteString(strconv.Itoa(limit.Offset))
				}
			}
		},
		"FOR": func(c clause.Clause, builder clause.Builder) {
			if _, ok := c.Expression.(clause.Locking); ok {
				return // SQLite does not support row-level locking.
			}
			c.Build(builder)
		},
	}
}

func (d *gormSQLiteDialector) Migrator(db *gorm.DB) gorm.Migrator {
	return migrator.Migrator{Config: migrator.Config{
		DB:                          db,
		Dialector:                   d,
		CreateIndexAfterCreateTable: true,
	}}
}

func (d *gormSQLiteDialector) DataTypeOf(field *schema.Field) string {
	switch field.DataType {
	case schema.Bool:
		return "numeric"
	case schema.Int, schema.Uint:
		if field.AutoIncrement {
			return "integer PRIMARY KEY AUTOINCREMENT"
		}
		return "integer"
	case schema.Float:
		return "real"
	case schema.String:
		return "text"
	case schema.Time:
		if val, ok := field.TagSettings["TYPE"]; ok {
			return val
		}
		return "datetime"
	case schema.Bytes:
		return "blob"
	}
	return string(field.DataType)
}

func (d *gormSQLiteDialector) DefaultValueOf(field *schema.Field) clause.Expression {
	if field.AutoIncrement {
		return clause.Expr{SQL: "NULL"}
	}
	return clause.Expr{SQL: "DEFAULT"}
}

func (d *gormSQLiteDialector) BindVarTo(writer clause.Writer, _ *gorm.Statement, _ any) {
	writer.WriteByte('?')
}

func (d *gormSQLiteDialector) QuoteTo(writer clause.Writer, str string) {
	var (
		underQuoted, selfQuoted bool
		continuousBacktick      int8
		shiftDelimiter          int8
	)
	for _, v := range []byte(str) {
		switch v {
		case '`':
			continuousBacktick++
			if continuousBacktick == 2 {
				writer.WriteString("``")
				continuousBacktick = 0
			}
		case '.':
			if continuousBacktick > 0 || !selfQuoted {
				shiftDelimiter = 0
				underQuoted = false
				continuousBacktick = 0
				writer.WriteString("`")
			}
			writer.WriteByte(v)
			continue
		default:
			if shiftDelimiter-continuousBacktick <= 0 && !underQuoted {
				writer.WriteString("`")
				underQuoted = true
				if selfQuoted = continuousBacktick > 0; selfQuoted {
					continuousBacktick--
				}
			}
			for ; continuousBacktick > 0; continuousBacktick-- {
				writer.WriteString("``")
			}
			writer.WriteByte(v)
		}
		shiftDelimiter++
	}
	if continuousBacktick > 0 && !selfQuoted {
		writer.WriteString("``")
	}
	writer.WriteString("`")
}

func (d *gormSQLiteDialector) Explain(sql string, vars ...any) string {
	return logger.ExplainSQL(sql, nil, `"`, vars...)
}

func gormSQLiteCompareVersion(v1, v2 string) int {
	n, m := len(v1), len(v2)
	i, j := 0, 0
	for i < n || j < m {
		x := 0
		for ; i < n && v1[i] != '.'; i++ {
			x = x*10 + int(v1[i]-'0')
		}
		i++
		y := 0
		for ; j < m && v2[j] != '.'; j++ {
			y = y*10 + int(v2[j]-'0')
		}
		j++
		if x > y {
			return 1
		}
		if x < y {
			return -1
		}
	}
	return 0
}
