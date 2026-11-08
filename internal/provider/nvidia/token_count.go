package nvidia

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/minhhhduc/gnvipi/internal/gateway/tokencount"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	clipexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func (*Executor) CountTokens(
	_ context.Context,
	_ *coreauth.Auth,
	req clipexec.Request,
	_ clipexec.Options, //nolint:gocritic // hugeParam: CLIProxyAPI ProviderExecutor requires options by value.
) (clipexec.Response, error) {
	count, err := estimateRequestTokens(req.Payload)
	if err != nil {
		return clipexec.Response{}, requestErr("invalid json body")
	}
	payload, err := json.Marshal(struct {
		InputTokens int `json:"input_tokens"`
	}{InputTokens: count})
	if err != nil {
		return clipexec.Response{}, fmt.Errorf("encode token count: %w", err)
	}
	return clipexec.Response{Payload: payload}, nil
}

func estimateRequestTokens(payload []byte) (int, error) {
	return tokencount.EstimateRequest(payload)
}
