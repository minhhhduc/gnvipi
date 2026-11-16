package main

import (
	"context"
	"log"
	"time"

	"github.com/minhhhduc/gnvipi/internal/captcha"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

// championStateKey is the system_state document holding the captcha champion
// selection: the URL that minted tokens fastest on the last benchmark run.
const championStateKey = "captcha_champion"

// loadChampionState resolves the persisted champion from the store that owns
// runtime state: PostgreSQL in database mode, the user-cache JSON file in file
// mode. A missing or unreadable value yields an empty state, which falls back to
// the default playground URL.
func loadChampionState(ctx context.Context, state *appDB) *captcha.ChampionState {
	if state == nil {
		return captcha.LoadChampion(captcha.ChampionStateFile())
	}
	var champion captcha.ChampionState
	ok, err := providerstore.LoadSystemState(ctx, state.pool, championStateKey, &champion)
	if err != nil {
		log.Printf("warning: captcha champion state: %v", err)
		return &captcha.ChampionState{}
	}
	if !ok {
		return &captcha.ChampionState{}
	}
	return &champion
}

// championStateSource names where the loaded champion came from: the log line
// is the only place an operator sees which store is authoritative.
func championStateSource(state *appDB) string {
	if state != nil {
		return "database"
	}
	return "disk"
}

// saveChampionState records a completed benchmark. Both stores are best-effort:
// losing the champion only costs a rerun of the selection, never correctness.
func saveChampionState(ctx context.Context, state *appDB, champion *captcha.ChampionState) {
	if state == nil {
		if err := champion.Save(captcha.ChampionStateFile()); err != nil {
			log.Printf("champion: save failed (non-fatal): %v", err)
		}
		return
	}
	champion.Updated = time.Now()
	if err := providerstore.SaveSystemState(ctx, state.pool, championStateKey, champion); err != nil {
		log.Printf("champion: save failed (non-fatal): %v", err)
	}
}
