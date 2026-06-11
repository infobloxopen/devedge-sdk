package apikeyv1_test

// security_isolation_test.go — cross-account isolation tests for both the
// ent-backed and GORM-backed APIKey repositories.
//
// Both tests call seccheck.AssertCrossAccountIsolation and expect ZERO findings.
// The repositories scope queries by account_id, so PrincipalB must never see
// resources created by PrincipalA.

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"

	// GORM core + sub-packages (all within gorm.io/gorm, already a dep).
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"

	// The ent test client for the ent isolation test.
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"

	// SDK packages.
	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/seccheck"
	"github.com/infobloxopen/devedge-sdk/secret"

	// The package under test.
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ----------------------------------------------------------------------------
// Minimal inline SQLite GORM dialector
//
// We cannot import gorm.io/driver/sqlite because it blank-imports
// github.com/mattn/go-sqlite3 (CGo), which would conflict with the
// modernc.org/sqlite "sqlite3" shim registered in sqlite_test.go.
// This inline implementation uses only gorm.io/gorm sub-packages (already
// a declared dependency) and the "sqlite3" driver registered by the shim.
// ----------------------------------------------------------------------------

const testSQLiteDriverName = "sqlite3"

type testSQLiteDialector struct {
	dsn  string
	conn gorm.ConnPool
}

func openTestSQLite(dsn string) gorm.Dialector {
	return &testSQLiteDialector{dsn: dsn}
}

func (d *testSQLiteDialector) Name() string { return "sqlite" }

func (d *testSQLiteDialector) Initialize(db *gorm.DB) error {
	if d.conn != nil {
		db.ConnPool = d.conn
	} else {
		sqlDB, err := sql.Open(testSQLiteDriverName, d.dsn)
		if err != nil {
			return err
		}
		db.ConnPool = sqlDB
	}

	// Detect SQLite version to opt in to RETURNING clause support (3.35+).
	var version string
	if err := db.ConnPool.QueryRowContext(context.Background(), "select sqlite_version()").Scan(&version); err != nil {
		return err
	}
	if sqliteCompareVersion(version, "3.35.0") >= 0 {
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

func (d *testSQLiteDialector) clauseBuilders() map[string]clause.ClauseBuilder {
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

func (d *testSQLiteDialector) Migrator(db *gorm.DB) gorm.Migrator {
	return migrator.Migrator{Config: migrator.Config{
		DB:                          db,
		Dialector:                   d,
		CreateIndexAfterCreateTable: true,
	}}
}

func (d *testSQLiteDialector) DataTypeOf(field *schema.Field) string {
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

func (d *testSQLiteDialector) DefaultValueOf(field *schema.Field) clause.Expression {
	if field.AutoIncrement {
		return clause.Expr{SQL: "NULL"}
	}
	return clause.Expr{SQL: "DEFAULT"}
}

func (d *testSQLiteDialector) BindVarTo(writer clause.Writer, _ *gorm.Statement, _ interface{}) {
	writer.WriteByte('?')
}

func (d *testSQLiteDialector) QuoteTo(writer clause.Writer, str string) {
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

func (d *testSQLiteDialector) Explain(sql string, vars ...interface{}) string {
	return logger.ExplainSQL(sql, nil, `"`, vars...)
}

func sqliteCompareVersion(v1, v2 string) int {
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

// ----------------------------------------------------------------------------
// Cross-account isolation test helpers
// ----------------------------------------------------------------------------

// mapToNotFound converts persistence.ErrNotFound to a gRPC codes.NotFound
// status error, which is what seccheck.AssertCrossAccountIsolation expects.
func mapToNotFound(err error) error {
	if errors.Is(err, persistence.ErrNotFound) {
		return status.Error(codes.NotFound, "not found")
	}
	return err
}

// ----------------------------------------------------------------------------
// TestSecurity_CrossAccountIsolation_Ent
// ----------------------------------------------------------------------------

// TestSecurity_CrossAccountIsolation_Ent verifies that the ent-backed
// APIKey repository enforces cross-account isolation: resources created by
// alice are invisible to bob.  Zero seccheck findings are expected.
func TestSecurity_CrossAccountIsolation_Ent(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:sec_ent_iso?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyEntRepository(client, enc)

	cfg := seccheck.IsolationConfig{
		PrincipalA: "alice",
		PrincipalB: "bob",
		CreateFn: func(ctx context.Context) (string, error) {
			// The seccheck framework passes a context with gRPC outgoing
			// metadata for PrincipalA. The ent repo reads the tenant from
			// the context value set by middleware.WithTenantID, so we
			// inject it explicitly.
			aliceCtx := middleware.WithTenantID(ctx, "alice")
			k := &apikeyv1.APIKey{
				Id:        "sec-ent-alice-1",
				Name:      "alice isolation key",
				AccountId: "alice",
				KeyValue:  "sk_alice_sectest",
			}
			created, err := repo.Create(aliceCtx, k)
			if err != nil {
				return "", err
			}
			return created.Id, nil
		},
		ReadFn: func(ctx context.Context, id string) error {
			bobCtx := middleware.WithTenantID(ctx, "bob")
			_, err := repo.Get(bobCtx, id)
			return mapToNotFound(err)
		},
		ListFn: func(ctx context.Context) (int, error) {
			bobCtx := middleware.WithTenantID(ctx, "bob")
			items, _, err := repo.List(bobCtx, persistence.ListOptions{})
			if err != nil {
				return 0, err
			}
			return len(items), nil
		},
	}

	findings := seccheck.AssertCrossAccountIsolation(context.Background(), cfg)
	seccheck.RunT(t, findings)
}

// ----------------------------------------------------------------------------
// TestSecurity_CrossAccountIsolation_GORM
// ----------------------------------------------------------------------------

// TestSecurity_CrossAccountIsolation_GORM verifies that the GORM-backed
// APIKey repository enforces cross-account isolation using an in-memory
// SQLite database.  Zero seccheck findings are expected.
func TestSecurity_CrossAccountIsolation_GORM(t *testing.T) {
	// Open GORM with the inline SQLite dialector backed by the "sqlite3"
	// driver registered by the modernc shim in sqlite_test.go.
	db, err := gorm.Open(openTestSQLite("file:sec_gorm_iso?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("open gorm db: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	if err := db.AutoMigrate(&apikeyv1.APIKeyModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyRepository(db, enc)

	cfg := seccheck.IsolationConfig{
		PrincipalA: "alice",
		PrincipalB: "bob",
		CreateFn: func(ctx context.Context) (string, error) {
			aliceCtx := middleware.WithTenantID(ctx, "alice")
			k := &apikeyv1.APIKey{
				Id:        "sec-gorm-alice-1",
				Name:      "alice gorm isolation key",
				AccountId: "alice",
				KeyValue:  "sk_alice_gorm_sectest",
			}
			created, err := repo.Create(aliceCtx, k)
			if err != nil {
				return "", err
			}
			return created.Id, nil
		},
		ReadFn: func(ctx context.Context, id string) error {
			bobCtx := middleware.WithTenantID(ctx, "bob")
			_, err := repo.Get(bobCtx, id)
			return mapToNotFound(err)
		},
		ListFn: func(ctx context.Context) (int, error) {
			bobCtx := middleware.WithTenantID(ctx, "bob")
			items, _, err := repo.List(bobCtx, persistence.ListOptions{})
			if err != nil {
				return 0, err
			}
			return len(items), nil
		},
	}

	findings := seccheck.AssertCrossAccountIsolation(context.Background(), cfg)
	seccheck.RunT(t, findings)
}
