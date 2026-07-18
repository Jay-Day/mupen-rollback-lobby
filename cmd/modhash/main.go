// modhash generates an admins.json entry for a moderator password.
//
// Usage:
//
//	go run ./cmd/modhash <name> <password>
//
// It prints a JSON object with a random salt and sha256(salt+password) hash.
// Add the object to the admins.json array the server loads (RMGK_ADMINS_PATH,
// default ./admins.json).
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: modhash <name> <password>")
		os.Exit(2)
	}
	name, password := os.Args[1], os.Args[2]

	saltBytes := make([]byte, 8)
	if _, err := rand.Read(saltBytes); err != nil {
		fmt.Fprintln(os.Stderr, "salt generation failed:", err)
		os.Exit(1)
	}
	salt := hex.EncodeToString(saltBytes)

	entry := moderation.Admin{
		Name: name,
		Salt: salt,
		Hash: moderation.HashPassword(salt, password),
	}
	out, _ := json.MarshalIndent(entry, "", "  ")
	fmt.Println(string(out))
}
