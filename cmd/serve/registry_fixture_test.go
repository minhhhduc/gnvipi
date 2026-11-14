package main

import (
	"os"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/models"
)

// requestFixture is a synthetic catalog entry for tests that execute the NVIDIA
// path without building a catalog of their own. The real catalog is runtime
// state loaded from PostgreSQL or the models file.
const requestFixture = "fixture/vendor-model"

func requestFixtureCatalog() map[string]models.ModelInfo {
	return map[string]models.ModelInfo{
		requestFixture: {
			Slug:       "vendor-model",
			Namespace:  models.Namespace,
			FunctionID: "00000000-0000-0000-0000-000000000005",
		},
	}
}

func seedRequestFixture(t *testing.T) {
	t.Helper()
	saved := models.Snapshot()
	t.Cleanup(func() { models.Replace(saved) })
	models.Replace(requestFixtureCatalog())
}

func TestMain(m *testing.M) {
	models.Replace(requestFixtureCatalog())
	os.Exit(m.Run())
}
