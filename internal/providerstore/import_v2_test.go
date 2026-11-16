package providerstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type stubImportOwnerRow struct {
	id  string
	err error
}

func (r stubImportOwnerRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return errors.New("unexpected scan destination count")
	}
	ptr, ok := dest[0].(*string)
	if !ok {
		return errors.New("unexpected scan destination type")
	}
	*ptr = r.id
	return nil
}

type stubImportOwnerQuerier struct {
	gotSQL  string
	gotArgs []any
	row     stubImportOwnerRow
}

func (q *stubImportOwnerQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.gotSQL = sql
	q.gotArgs = args
	return q.row
}

func TestFindImportOwnerIDAllowsActiveSuperuserOrRoot(t *testing.T) {
	q := &stubImportOwnerQuerier{
		row: stubImportOwnerRow{id: "00000000-0000-0000-0000-000000000001"},
	}

	got, err := findImportOwnerID(t.Context(), q, "gnvipi")
	if err != nil {
		t.Fatalf("findImportOwnerID: %v", err)
	}
	if got != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("ownerID = %q, want 00000000-0000-0000-0000-000000000001", got)
	}
	if len(q.gotArgs) != 1 || q.gotArgs[0] != "gnvipi" {
		t.Fatalf("args = %#v, want [\"gnvipi\"]", q.gotArgs)
	}
	for _, want := range []string{"role IN ('superuser', 'root')", "status = 'active'"} {
		if !strings.Contains(q.gotSQL, want) {
			t.Fatalf("query %q missing %q", q.gotSQL, want)
		}
	}
}

func TestFindImportOwnerIDErrors(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		q := &stubImportOwnerQuerier{row: stubImportOwnerRow{err: pgx.ErrNoRows}}
		_, err := findImportOwnerID(t.Context(), q, "gnvipi")
		if err == nil || !strings.Contains(err.Error(), `active superuser or root "gnvipi" not found`) {
			t.Fatalf("err = %v, want active superuser or root not found", err)
		}
	})

	t.Run("query failure", func(t *testing.T) {
		dbErr := errors.New("connection lost")
		q := &stubImportOwnerQuerier{row: stubImportOwnerRow{err: dbErr}}
		_, err := findImportOwnerID(t.Context(), q, "gnvipi")
		if !errors.Is(err, dbErr) {
			t.Fatalf("err = %v, want wrapped %v", err, dbErr)
		}
	})
}
