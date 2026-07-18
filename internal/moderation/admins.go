package moderation

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
)

// Admin is one moderator credential. Passwords are never stored: hash is the
// hex-encoded sha256(salt + password). Generate entries with cmd/modhash.
type Admin struct {
	Name string `json:"name"`
	Salt string `json:"salt"`
	Hash string `json:"hash"`
}

// AdminStore holds the loaded moderator credentials.
type AdminStore struct {
	admins []Admin
}

// LoadAdmins reads an admins.json file: [{"name","salt","hash"}, ...].
// A missing file yields an empty store (no admins) without error, so the server
// still starts; moderation actions simply can't be authorized until configured.
func LoadAdmins(path string) (*AdminStore, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &AdminStore{}, nil
		}
		return nil, err
	}
	var admins []Admin
	if err := json.Unmarshal(data, &admins); err != nil {
		return nil, err
	}
	return &AdminStore{admins: admins}, nil
}

// Count reports how many admin credentials are loaded.
func (a *AdminStore) Count() int { return len(a.admins) }

// HashPassword returns the hex sha256(salt + password). Shared with cmd/modhash
// so the stored hash and the runtime check use identical encoding.
func HashPassword(salt, password string) string {
	sum := sha256.Sum256([]byte(salt + password))
	return hex.EncodeToString(sum[:])
}

// Authenticate returns the admin name for a matching password, constant-time
// against each stored hash. ok is false when no credential matches.
func (a *AdminStore) Authenticate(password string) (string, bool) {
	for _, ad := range a.admins {
		want := strings.ToLower(strings.TrimSpace(ad.Hash))
		got := HashPassword(ad.Salt, password)
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return ad.Name, true
		}
	}
	return "", false
}
