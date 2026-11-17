// modhash generates an admins.json entry for a moderator password.
//
// Usage:
//
//	go run ./cmd/modhash <name>              # makes up a long random password
//	go run ./cmd/modhash <name> <password>   # uses the given one
//
// It prints a JSON object with a random salt and sha256(salt+password) hash.
// Add the object to the admins.json array the server loads (RMGK_ADMINS_PATH,
// default ./admins.json). With no password given it makes one up (24 random
// characters, about 140 bits) and prints it to stderr, once: give it to the
// moderator over a private channel. That also keeps it out of shell history.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
)

// passwordChars avoids look-alikes (0/O, 1/l/I) so a password can be read out.
const passwordChars = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomPassword(n int) (string, error) {
	b := make([]byte, n)
	for i := range b {
		var r [1]byte
		for {
			if _, err := rand.Read(r[:]); err != nil {
				return "", err
			}
			// reject what would bias the modulo
			if int(r[0]) < 256-256%len(passwordChars) {
				break
			}
		}
		b[i] = passwordChars[int(r[0])%len(passwordChars)]
	}
	return string(b), nil
}

func main() {
	if len(os.Args) != 2 && len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: modhash <name> [password]")
		os.Exit(2)
	}
	name := os.Args[1]
	var password string
	if len(os.Args) == 3 {
		password = os.Args[2]
	} else {
		var err error
		if password, err = randomPassword(24); err != nil {
			fmt.Fprintln(os.Stderr, "password generation failed:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "password for "+name+": "+password)
	}

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
