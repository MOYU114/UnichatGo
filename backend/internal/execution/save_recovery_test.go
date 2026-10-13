package execution

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// This connector still executes every transaction/lock against real MySQL. It
// injects selected transient errors before a statement reaches the driver; it
// deliberately does not use ErrBadConn, which database/sql may transparently retry.
type faultConnector struct {
	base driver.Connector
	plan *sqlFaultPlan
}

type sqlFaultPlan struct {
	assistant        atomic.Bool
	renewal          atomic.Bool
	assistantFailed  chan struct{}
	renewalFailed    chan struct{}
	renewalRecovered chan struct{}
	renewed          sync.Once
}

func (c *faultConnector) Driver() driver.Driver { return c.base.Driver() }
func (c *faultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &faultConnection{Conn: conn, plan: c.plan}, nil
}

type faultConnection struct {
	driver.Conn
	plan *sqlFaultPlan
}

func (c *faultConnection) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *faultConnection) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }
func (c *faultConnection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}
func (c *faultConnection) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if strings.HasPrefix(query, "INSERT INTO messages(") && strings.Contains(query, "'assistant'") && c.plan.assistant.CompareAndSwap(true, false) {
		close(c.plan.assistantFailed)
		return nil, errors.New("injected transient assistant write failure")
	}
	renew := strings.HasPrefix(query, "UPDATE execution_evidence SET lease_until_ms=?")
	if renew && c.plan.renewal.CompareAndSwap(true, false) {
		close(c.plan.renewalFailed)
		return nil, errors.New("injected transient renewal failure")
	}
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if renew && err == nil {
		// This observes the statement, not commit. Finalization takes the same
		// user lock and therefore cannot outrun the renewal transaction's end.
		c.plan.renewed.Do(func() { close(c.plan.renewalRecovered) })
	}
	return result, err
}

func faultRepository(t *testing.T, r *Repository, plan *sqlFaultPlan) *Repository {
	t.Helper()
	cfg, err := mysql.ParseDSN(os.Getenv("UNICHATGO_TEST_MYSQL_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	// Observe successful parameterized statements directly rather than letting
	// ErrSkip route them through an unwrapped prepared-statement fallback.
	cfg.InterpolateParams = true
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(&faultConnector{base: connector, plan: plan})
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { db.Close() })
	return NewRepository(db, r.limits)
}

type recoveryProvider struct {
	calls   atomic.Int64
	entered chan struct{}
	release <-chan struct{}
	abort   chan struct{}
}

func (p *recoveryProvider) Generate(ctx context.Context, _ CallInput) (string, error) {
	p.calls.Add(1)
	close(p.entered)
	if p.release != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-p.abort:
			return "", errors.New("test aborted")
		case <-p.release:
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "original response preserved", nil
}

func TestMySQLRuntimeSavesOriginalAfterTransientSQLFailure(t *testing.T) {
	for _, renewal := range []bool{false, true} {
		name := "result-save"
		if renewal {
			name = "renewal-and-result-save"
		}
		t.Run(name, func(t *testing.T) {
			r, uid, sid := mysqlRepository(t)
			plan := &sqlFaultPlan{assistantFailed: make(chan struct{}), renewalFailed: make(chan struct{}), renewalRecovered: make(chan struct{})}
			plan.assistant.Store(true)
			plan.renewal.Store(renewal)
			wrapped := faultRepository(t, r, plan)
			receipt := acceptRuntime(t, r, uid, sid, "recover without regeneration")
			e, err := wrapped.Claim(context.Background(), receipt.ID, "fault-worker", 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			provider := &recoveryProvider{entered: make(chan struct{}), abort: make(chan struct{})}
			if renewal {
				provider.release = plan.renewalRecovered
			}
			runtime := &Runtime{Repo: wrapped, Provider: provider, Decrypt: func(s string) (string, error) { return s, nil }, Quota: testPolicy(), Lease: 3 * time.Second}
			done := make(chan struct{})
			go func() { defer close(done); runtime.execute(e) }()
			t.Cleanup(func() {
				close(provider.abort)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("test execution did not drain")
				}
			})
			select {
			case <-provider.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("provider did not enter")
			}
			if renewal {
				select {
				case <-plan.renewalFailed:
				case <-time.After(5 * time.Second):
					t.Fatal("renewal fault not reached")
				}
				select {
				case <-plan.renewalRecovered:
				case <-time.After(5 * time.Second):
					t.Fatal("transient renewal failure cancelled recoverable work")
				}
			}
			select {
			case <-plan.assistantFailed:
			case <-time.After(5 * time.Second):
				t.Fatal("save fault not reached")
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("bounded save recovery did not finish")
			}
			got, err := r.Get(context.Background(), receipt.ID)
			if err != nil || got.State != Completed {
				t.Fatalf("state=%s err=%v", got.State, err)
			}
			var count int
			var content string
			if err := r.DB.QueryRow(`SELECT COUNT(*),COALESCE(MAX(content),'') FROM messages WHERE session_id=? AND role='assistant'`, sid).Scan(&count, &content); err != nil {
				t.Fatal(err)
			}
			if provider.calls.Load() != 1 || count != 1 || content != "original response preserved" {
				t.Fatalf("calls=%d messages=%d content=%q", provider.calls.Load(), count, content)
			}
			var calls int
			if err := r.DB.QueryRow(`SELECT COUNT(*) FROM execution_quota_calls WHERE request_id=?`, receipt.ID).Scan(&calls); err != nil || calls != 3 {
				t.Fatalf("quota reserved duplicate calls: %d %v", calls, err)
			}
		})
	}
}
