package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

func TestAdminStatsUserResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	events := []nvidia.RequestEvent{
		{UserID: "alice", Model: "alice-model"},
		{UserID: "bob", Model: "bob-model"},
		{Model: "unattributed-model"},
	}
	adminStatsCache.Lock()
	oldData, oldAt := adminStatsCache.data, adminStatsCache.at
	adminStatsCache.data = gin.H{
		"models":          []nvidia.ModelStats{{Model: "global-model", Requests: 3}},
		"series":          []nvidia.MinuteStats{{Req: 3}},
		"series_by_model": map[string][]nvidia.MinuteStats{"global-model": {{Req: 3}}},
		"events":          events,
	}
	adminStatsCache.at = time.Now()
	adminStatsCache.Unlock()
	t.Cleanup(func() {
		adminStatsCache.Lock()
		adminStatsCache.data, adminStatsCache.at = oldData, oldAt
		adminStatsCache.Unlock()
	})

	request := func(role auth.Role) []byte {
		t.Helper()
		res := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(res)
		c.Request = httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil)
		c.Set(adminIdentityKey, &adminIdentity{UserID: "alice", Role: role})
		handleAdminStats(c)
		if res.Code != http.StatusOK {
			t.Fatalf("GET stats = %d: %s", res.Code, res.Body.String())
		}
		return res.Body.Bytes()
	}

	var user struct {
		Models        []nvidia.ModelStats             `json:"models"`
		Series        []nvidia.MinuteStats            `json:"series"`
		SeriesByModel map[string][]nvidia.MinuteStats `json:"series_by_model"`
		Events        []nvidia.RequestEvent           `json:"events"`
	}
	if err := json.Unmarshal(request(auth.RoleUser), &user); err != nil {
		t.Fatalf("user stats must preserve the dashboard JSON schema: %v", err)
	}
	if user.Models == nil || user.Series == nil || len(user.Models) != 0 || len(user.Series) != 0 || len(user.SeriesByModel) != 0 {
		t.Fatalf("user received global aggregates: %+v", user)
	}
	if len(user.Events) != 1 || user.Events[0].Model != "alice-model" {
		t.Fatalf("user events = %+v, want only Alice's request", user.Events)
	}

	var operator struct {
		Models []nvidia.ModelStats   `json:"models"`
		Events []nvidia.RequestEvent `json:"events"`
	}
	if err := json.Unmarshal(request(auth.RoleAdmin), &operator); err != nil {
		t.Fatal(err)
	}
	if len(operator.Models) != 1 || operator.Models[0].Requests != 3 || len(operator.Events) != 3 {
		t.Fatalf("user read changed the operator's shared stats: %+v", operator)
	}
}

func TestStatsFrameEventsScoped(t *testing.T) {
	events := []nvidia.RequestEvent{
		{UserID: "bob", Model: "bob-model"},
		{Model: "unattributed-model"},
		{UserID: "alice", Model: "alice-model"},
	}
	scoped := scopeEvents(events, "alice")
	if len(scoped) != 1 || scoped[0].Model != "alice-model" {
		t.Fatalf("frame events = %+v, want only Alice's request", scoped)
	}
	if got := scopeEvents(events, "no-traffic-user"); len(got) != 0 {
		t.Fatalf("user without traffic received %+v", got)
	}
	if got := scopeEvents(events, ""); len(got) != 3 {
		t.Fatalf("operator frame events = %+v, want all requests", got)
	}
}
