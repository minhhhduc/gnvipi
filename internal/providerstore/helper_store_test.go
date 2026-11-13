package providerstore

import (
	"context"
	"testing"
	"time"
)

type stubSource struct {
	load    func(context.Context) (Rows, int64, error)
	version func(context.Context) (int64, error)
}

func (s *stubSource) LoadConfig(ctx context.Context) (Rows, int64, error) {
	return s.load(ctx)
}

func (s *stubSource) Version(ctx context.Context) (int64, error) {
	return s.version(ctx)
}

func mustBuildSnapshot(t *testing.T, rows Rows, version int64, opts BuildOptions) *Snapshot {
	t.Helper()
	snapshot, err := BuildSnapshot(rows, version, opts)
	if err != nil {
		t.Fatalf("build snapshot: %v", err)
	}
	return snapshot
}

func snapshotAsStore(snapshot *Snapshot, opts StoreOptions) *Store {
	store := NewStore(&stubSource{
		load: func(context.Context) (Rows, int64, error) { return Rows{}, 0, nil },
		version: func(context.Context) (int64, error) {
			return snapshot.Version(), nil
		},
	}, opts)
	store.current.Store(snapshot)
	return store
}

func fixedNow() time.Time { return time.Unix(1_700_000_000, 0) }
