package main

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestKeyWriteRequestOwnerScopeRejectsInvalidUUID(t *testing.T) {
	body := keyWriteRequest{OwnerID: json.RawMessage(`"not-a-uuid"`)}
	if _, _, _, err := body.ownerScope(); !errors.Is(err, errInvalidOwnerID) {
		t.Fatalf("non-UUID owner_id = %v, want errInvalidOwnerID", err)
	}

	body.OwnerID = json.RawMessage(`"11111111-1111-1111-1111-111111111111"`)
	if _, _, id, err := body.ownerScope(); err != nil || id != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("valid owner_id = %q, %v", id, err)
	}

	body.OwnerID = json.RawMessage(`null`)
	if _, shared, _, err := body.ownerScope(); err != nil || !shared {
		t.Fatalf("null owner_id = shared=%v, %v", shared, err)
	}
}
