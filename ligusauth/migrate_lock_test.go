package ligusauth

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pods starting together all create the schema; under the advisory lock none
// of them fails on a catalog race.
func TestPGConcurrentMigrations(t *testing.T) {
	dsn := os.Getenv("LIGUS_AUTH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set LIGUS_AUTH_TEST_DATABASE_URL to run Postgres tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("auth_migrate_%d", rand.Int64N(1<<40))
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") //nolint:errcheck
	u, _ := url.Parse(dsn)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			st, err := NewPostgresStore(ctx, u.String())
			if err != nil {
				errs <- err
				return
			}
			st.Close()
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent migration failed: %v", err)
	}
}
