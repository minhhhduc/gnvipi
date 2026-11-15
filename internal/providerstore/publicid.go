package providerstore

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// publicModelIDLength is how many base32 characters a caller sees. 26
// characters carry 130 bits of the hash: short enough to paste into a client
// config, wide enough that a collision needs a deliberate search.
const publicModelIDLength = 26

// PublicModelID returns the caller-facing id of a shared-key mapping.
//
// A shared key belongs to the operator, so the alias and the upstream model
// behind it are not the caller's to read: the caller selects the mapping by an
// id derived from the provider and the alias. Deriving it keeps the id stable
// across restarts and instances with no stored column, and it cannot be read
// back into the alias without guessing both inputs.
func PublicModelID(providerID, alias string) string {
	sum := sha256.Sum256([]byte(providerID + "\x00" + alias))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return strings.ToLower(encoded[:publicModelIDLength])
}
