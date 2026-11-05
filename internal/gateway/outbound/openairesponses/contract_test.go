package openairesponses_test

import (
	"os"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adaptertest"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound/openairesponses"
)

func TestMain(m *testing.M) {
	adaptertest.RegisterFlags()
	os.Exit(m.Run())
}

func TestOutboundContract(t *testing.T) {
	adaptertest.RunOutboundContract(t, openairesponses.New(), "testdata")
}
