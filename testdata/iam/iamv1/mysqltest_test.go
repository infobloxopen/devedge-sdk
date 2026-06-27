package iamv1_test

// mysqltest_test.go — a testcontainers-backed MySQL harness for the IAM fixture
// (F033 Phase-2). It is the MySQL twin of pgtest_test.go and proves the APPEND-ONLY
// partitioned outbox + drop-partition retention on REAL MySQL (the second production
// target named in the F033 directive: "postgres and mysql").
//
// Docker-optional contract: startMySQL calls t.Skip("docker unavailable: ...")
// cleanly when testcontainers / Docker cannot start, so `go test ./...` on a machine
// WITHOUT Docker still passes (the MySQL tests skip, they do not fail). When Docker
// IS available (e.g. CI's ubuntu-latest, or local Rancher) the MySQL tests run for
// real.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // registers the "mysql" database/sql driver for ent.Open and wait.ForSQL

	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/iamv1"
)

// MySQL admin credentials. The testcontainers mysql module leaves remote root denied
// (root is localhost-only) and scopes the created user to the iam schema, so the init
// script testdata/mysql_grant_iam.sql grants this user *.* — letting the harness
// CREATE a fresh, uniquely named database per test. These are throwaway-container
// credentials only.
const (
	mysqlUser     = "iam"
	mysqlPassword = "iam"
)

var (
	myOnce      sync.Once
	myContainer *tcmysql.MySQLContainer
	myHostPort  string // host:port endpoint of the container's 3306
	myStartErr  error
)

// terminateMySQL terminates the shared MySQL container; called from TestMain (in
// pgtest_test.go) so cleanup is independent of Ryuk (the documented local workflow
// runs with TESTCONTAINERS_RYUK_DISABLED=true on Rancher Desktop). It is a no-op when
// Docker was unavailable (the container was never started).
func terminateMySQL() {
	if myContainer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = myContainer.Terminate(ctx)
		cancel()
	}
}

// startMySQL starts (once) a mysql:8 container via testcontainers and returns its
// host:port endpoint. If Docker/testcontainers cannot start it SKIPS the calling test
// rather than failing, so the suite is green without Docker.
func startMySQL(t *testing.T) string {
	t.Helper()
	myOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		c, err := tcmysql.Run(ctx,
			"mysql:8",
			tcmysql.WithDatabase("iam"),
			tcmysql.WithUsername(mysqlUser),
			tcmysql.WithPassword(mysqlPassword),
			// Grant the iam user *.* so the harness can CREATE a fresh database per test
			// (the module's user is otherwise scoped to the iam schema). The script runs in
			// the entrypoint's init phase.
			tcmysql.WithScripts("testdata/mysql_grant_iam.sql"),
			// A log wait (the second "ready for connections" line, after the entrypoint's
			// init+restart) rather than wait.ForSQL: ForSQL polls the container State over
			// the Docker socket every 100ms, which on the Rancher Desktop socket is slow
			// enough to trip the wait's own deadline before MySQL finishes initializing.
			// The log wait is cheap; the real readiness gate is the connect-retry in
			// mysqlReady below.
			testcontainers.WithWaitStrategy(
				wait.ForLog("ready for connections").
					WithOccurrence(2).
					WithStartupTimeout(150*time.Second),
			),
		)
		if err != nil {
			myStartErr = err
			return
		}
		endpoint, err := c.PortEndpoint(ctx, "3306/tcp", "")
		if err != nil {
			myStartErr = err
			_ = c.Terminate(ctx)
			return
		}
		// Belt-and-suspenders: poll a real connection before handing the endpoint out, so
		// the first test does not race the server's final startup.
		if err := mysqlReady(ctx, endpoint); err != nil {
			myStartErr = err
			_ = c.Terminate(ctx)
			return
		}
		myContainer = c
		myHostPort = endpoint
	})
	if myStartErr != nil {
		t.Skipf("docker unavailable: %v", myStartErr)
	}
	return myHostPort
}

// mysqlReady polls a real admin connection until MySQL accepts queries or ctx ends, so
// the harness does not hand out an endpoint the server is not yet serving.
func mysqlReady(ctx context.Context, endpoint string) error {
	db, err := sql.Open("mysql", mysqlAdminDSN(endpoint))
	if err != nil {
		return err
	}
	defer db.Close()
	deadline := time.Now().Add(60 * time.Second)
	var last error
	for {
		if perr := db.PingContext(ctx); perr == nil {
			if _, qerr := db.ExecContext(ctx, "SELECT 1"); qerr == nil {
				return nil
			} else {
				last = qerr
			}
		} else {
			last = perr
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("mysql not ready before deadline: %w", last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// mysqlAdminDSN is the admin DSN (no database selected) used to create per-test
// databases. The iam user is granted *.* by the init script.
func mysqlAdminDSN(endpoint string) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/?parseTime=true&loc=UTC", mysqlUser, mysqlPassword, endpoint)
}

// mysqlDSN is the DSN scoped to dbName, with parseTime so time.Time round-trips and
// loc=UTC so created_time stamps are interpreted in UTC (matching the partition
// bounds, which are computed in UTC).
func mysqlDSN(endpoint, dbName string) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&loc=UTC", mysqlUser, mysqlPassword, endpoint, dbName)
}

// freshMySQLDatabase creates a brand-new, uniquely named database on the shared
// container and returns a DSN for it, so each test starts from an empty schema.
func freshMySQLDatabase(t *testing.T, endpoint string) string {
	t.Helper()
	dbName := mysqlDBName(t.Name())
	admin, err := sql.Open("mysql", mysqlAdminDSN(endpoint))
	if err != nil {
		t.Fatalf("open mysql admin conn: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + dbName); err != nil {
		t.Fatalf("drop db %s: %v", dbName, err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatalf("create db %s: %v", dbName, err)
	}
	return mysqlDSN(endpoint, dbName)
}

// openIAMGormMySQL opens a GORM client on a fresh MySQL database and AutoMigrates the
// IAM models PLUS the reusable outbox/idempotency tables (the MySQL twin of
// openIAMGormPG). The GormIdempotencyStore's IdemMarker primary key is a real MySQL
// UNIQUE — a concurrent double-apply collides on it (the genuine exactly-once guard).
func openIAMGormMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := freshMySQLDatabase(t, startMySQL(t))
	// DefaultStringSize bounds unsized string columns to varchar(256) instead of MySQL's
	// default longtext, so the gormtx OutboxRow's indexed string columns (account_id,
	// etc.) are indexable — a TEXT/BLOB column cannot be indexed without a key length.
	// This is a connection-level migration knob (the shared OutboxRow model is unchanged,
	// keeping P1 PG/sqlite behavior intact).
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: dsn, DefaultStringSize: 256}), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("gorm.Open mysql: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, derr := db.DB(); derr == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(
		&iamv1.UserModel{},
		&iamv1.ApiKeyModel{},
		&gormtx.OutboxRow{},
		&gormtx.IdemMarker{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func mysqlDBName(testName string) string {
	out := make([]rune, 0, len(testName)+4)
	out = append(out, []rune("tc_")...)
	for _, r := range testName {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '_')
		}
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return strings.ToLower(string(out))
}
