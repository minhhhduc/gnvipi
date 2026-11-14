package providerstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// recordingQuerier records Exec calls and lets a test inspect how many round
// trips ReplaceKeyModels makes.
type recordingQuerier struct {
	execs []string
}

func (q *recordingQuerier) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	q.execs = append(q.execs, sql)
	return pgconn.CommandTag{}, nil
}

func (q *recordingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected Query")
}

func (q *recordingQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

func modelsFor(count int) []KeyModel {
	models := make([]KeyModel, 0, count)
	for i := 0; i < count; i++ {
		models = append(models, KeyModel{Alias: "alias-" + string(rune('a'+i%26)), UpstreamModel: "m"})
	}
	return models
}

func TestReplaceKeyModelsUsesCanonicalValidation(t *testing.T) {
	key := KeyRow{ID: "11111111-1111-1111-1111-111111111111", ProviderID: "p"}
	invalid := []KeyModel{{Alias: "ok", UpstreamModel: "m", Fallbacks: []string{""}}}
	q := &recordingQuerier{}
	err := ReplaceKeyModels(context.Background(), q, key, invalid)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid fallback = %v, want ErrInvalidConfig", err)
	}
	if len(q.execs) != 0 {
		t.Fatalf("invalid models reached the database: %d exec(s)", len(q.execs))
	}
}

func TestReplaceKeyModelsBatchesInserts(t *testing.T) {
	key := KeyRow{ID: "11111111-1111-1111-1111-111111111111", ProviderID: "p"}
	q := &recordingQuerier{}
	if err := ReplaceKeyModels(context.Background(), q, key, modelsFor(3)); err != nil {
		t.Fatalf("replace key models: %v", err)
	}
	if len(q.execs) != 2 {
		t.Fatalf("exec count = %d, want one delete plus one batched insert", len(q.execs))
	}
	if tuples := strings.Count(q.execs[1], "::uuid") / 2; tuples != 3 {
		t.Fatalf("batched insert carried %d rows, want 3: %s", tuples, q.execs[1])
	}
}

func TestReplaceKeyModelsRejectsOversizedRequest(t *testing.T) {
	key := KeyRow{ID: "11111111-1111-1111-1111-111111111111", ProviderID: "p"}
	q := &recordingQuerier{}
	err := ReplaceKeyModels(context.Background(), q, key, modelsFor(keyModelsMaxPerRequest+1))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("oversized model list = %v, want ErrInvalidConfig", err)
	}
	if len(q.execs) != 0 {
		t.Fatalf("oversized model list reached the database: %d exec(s)", len(q.execs))
	}
}
