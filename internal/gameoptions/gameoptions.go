// Package gameoptions reads game options packs: the JSON files RMG-K ships in
// Data/GameOptions, each offering options for one game as ROM patches. The
// lobby keeps its own copy (RMGK_GAME_OPTIONS_DIR) so it can hand a ranked
// match the patches its ruleset mandates; a host can't choose or alter them.
//
// Packs are held to the rules RMG-K's loader applies
// (Source/RMG-Core/GameOptions.cpp): a pack or option it would skip is
// skipped here too, with the same limits.
package gameoptions

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// What one match may carry and one option may hold (RMG-Core's
// CoreGameOption* limits).
const (
	MaxOptions     = 8
	MaxPatches     = 256
	MaxPatchBytes  = 64 * 1024
	MaxIDLength    = 32
	MaxLabelLength = 256 // bytes
)

const (
	packFormat = 1
	// The cartridge's address space; no ROM is larger.
	maxRomBytes = 0x0FC00000
	// Packs are a few kilobytes; anything near this isn't one.
	maxPackFileBytes = 4 << 20
	// IPL3 checksums ROM 0x1000-0x101000 against the header's CRC words
	// (0x10-0x17) at boot.
	bootCodeStart = 0x1000
	bootCodeEnd   = 0x101000
	headerCRCs    = 0x10
	headerCRCsEnd = 0x18
)

// Patch replaces len(Bytes) ROM bytes at Offset, a ROM file offset in .z64
// (big-endian) byte order. Expect is what the unpatched ROM has there.
type Patch struct {
	Offset uint32
	Bytes  []byte
	Expect []byte
}

func (p Patch) end() uint64 { return uint64(p.Offset) + uint64(len(p.Bytes)) }

func (p Patch) overlaps(o Patch) bool {
	return uint64(p.Offset) < o.end() && uint64(o.Offset) < p.end()
}

// Option is one option a pack offers.
type Option struct {
	ID          string
	Label       string // English
	Description string
	// ServerOnly options are never offered to a room's host as a switch;
	// they exist for the lobby to mandate.
	ServerOnly bool
	Patches    []Patch
}

// PatchBytes is how many ROM bytes the option changes.
func (o *Option) PatchBytes() int {
	n := 0
	for _, p := range o.Patches {
		n += len(p.Bytes)
	}
	return n
}

// Pack is one pack file.
type Pack struct {
	ID      string
	Game    string
	RomMD5  []string // upper case
	Options []*Option
	File    string // base name, for logs
}

// Set is the packs of one directory. It isn't changed after loading.
type Set struct {
	packs []*Pack
}

// NewSet makes a set of already parsed packs, in order (tests).
func NewSet(packs ...*Pack) *Set {
	return &Set{packs: packs}
}

// Packs returns the loaded packs in load order.
func (s *Set) Packs() []*Pack {
	if s == nil {
		return nil
	}
	return s.packs
}

// LoadDir reads every *.json pack in dir in file name order. A pack with an
// earlier pack's id replaces it. Packs and options that break a rule are
// skipped and logged, the way RMG-K treats them; a missing directory loads
// no packs.
func LoadDir(dir string) (*Set, error) {
	entries, err := os.ReadDir(dir) // sorted by name
	if errors.Is(err, fs.ErrNotExist) {
		return &Set{}, nil
	}
	if err != nil {
		return nil, err
	}
	s := &Set{}
	for _, entry := range entries {
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > maxPackFileBytes {
			slog.Warn("game options pack skipped: too large", "file", entry.Name())
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("game options pack skipped: unreadable", "file", entry.Name(), "err", err)
			continue
		}
		pack, err := Parse(data, entry.Name())
		if err != nil {
			slog.Warn("game options pack skipped", "file", entry.Name(), "err", err)
			continue
		}
		s.add(pack)
	}
	return s, nil
}

func (s *Set) add(pack *Pack) {
	for i, other := range s.packs {
		if other.ID == pack.ID {
			slog.Info("game options pack replaces an earlier one", "file", pack.File, "pack", pack.ID, "replaced", other.File)
			s.packs[i] = pack
			return
		}
	}
	s.packs = append(s.packs, pack)
}

// Option returns the option id offered for the ROM with this MD5: the first
// pack (in load order) for the ROM that has it wins, as in RMG-K.
func (s *Set) Option(romMD5, id string) (*Option, bool) {
	md5 := strings.ToUpper(strings.TrimSpace(romMD5))
	for _, pack := range s.Packs() {
		if !pack.forRom(md5) {
			continue
		}
		for _, opt := range pack.Options {
			if opt.ID == id {
				return opt, true
			}
		}
	}
	return nil, false
}

func (p *Pack) forRom(md5 string) bool {
	for _, m := range p.RomMD5 {
		if m == md5 {
			return true
		}
	}
	return false
}

// Resolve looks up options ids for the ROM, in the order given, and checks
// they can be on together in one match: each offered for the ROM, no id
// twice, at most MaxOptions and MaxPatchBytes in all, and no two changing the
// same bytes.
func (s *Set) Resolve(romMD5 string, ids []string) ([]*Option, error) {
	md5 := strings.ToUpper(strings.TrimSpace(romMD5))
	if len(ids) > MaxOptions {
		return nil, fmt.Errorf("%d options; a match carries at most %d", len(ids), MaxOptions)
	}
	opts := make([]*Option, 0, len(ids))
	total := 0
	for i, id := range ids {
		for _, earlier := range ids[:i] {
			if earlier == id {
				return nil, fmt.Errorf("option %q is named twice", id)
			}
		}
		opt, ok := s.Option(md5, id)
		if !ok {
			return nil, fmt.Errorf("no game options pack offers option %q for ROM %s", id, md5)
		}
		total += opt.PatchBytes()
		for _, earlier := range opts {
			if at, hit := overlap(earlier, opt); hit {
				return nil, fmt.Errorf("options %q and %q change the same ROM bytes (at 0x%08X)", earlier.ID, opt.ID, at)
			}
		}
		opts = append(opts, opt)
	}
	if total > MaxPatchBytes {
		return nil, fmt.Errorf("the options change %d bytes; a match carries at most %d", total, MaxPatchBytes)
	}
	return opts, nil
}

// overlap reports whether a and b change a byte in common, and where.
func overlap(a, b *Option) (uint32, bool) {
	for _, pa := range a.Patches {
		for _, pb := range b.Patches {
			if pa.overlaps(pb) {
				return max(pa.Offset, pb.Offset), true
			}
		}
	}
	return 0, false
}

// ValidOptionID reports whether id is an option id: [a-z0-9-], 1 to 32.
func ValidOptionID(id string) bool {
	if id == "" || len(id) > MaxIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

func validPackID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_') {
			return false
		}
	}
	return true
}

// validLabel: well-formed UTF-8 of 1-256 bytes without control characters,
// since labels end up in the UI. U+FFFD is refused too: the JSON decoder puts
// it where the file has bytes that aren't UTF-8, which RMG-K refuses.
func validLabel(text string) bool {
	if text == "" || len(text) > MaxLabelLength || !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) || r == utf8.RuneError {
			return false
		}
	}
	return true
}

func validMD5(md5 string) bool {
	if len(md5) != 32 {
		return false
	}
	_, err := hex.DecodeString(md5)
	return err == nil
}

// parseHexBytes reads hex digits, allowing whitespace between bytes.
func parseHexBytes(text string) ([]byte, bool) {
	var out []byte
	high := -1
	for i := 0; i < len(text); i++ {
		c := text[i]
		var nibble int
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if high >= 0 {
				return nil, false
			}
			continue
		case c >= '0' && c <= '9':
			nibble = int(c - '0')
		case c >= 'a' && c <= 'f':
			nibble = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			nibble = int(c-'A') + 10
		default:
			return nil, false
		}
		if high < 0 {
			high = nibble
		} else {
			out = append(out, byte(high<<4|nibble))
			high = -1
		}
	}
	return out, high < 0 && len(out) > 0
}

// parseOffset reads a hex string ("0x00123456" or "00123456") or a
// non-negative JSON integer.
func parseOffset(raw json.RawMessage) (uint32, bool) {
	var parsed uint64
	switch kind(raw) {
	case '"':
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, false
		}
		if len(text) > 2 && text[0] == '0' && (text[1] == 'x' || text[1] == 'X') {
			text = text[2:]
		}
		if text == "" || len(text) > 8 {
			return 0, false
		}
		v, err := strconv.ParseUint(text, 16, 64) // no sign or prefix allowed
		if err != nil {
			return 0, false
		}
		parsed = v
	case 'n':
		text := strings.TrimSpace(string(raw))
		for i := 0; i < len(text); i++ {
			if text[i] < '0' || text[i] > '9' {
				return 0, false // negative, fractional or an exponent
			}
		}
		v, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return 0, false
		}
		parsed = v
	default:
		return 0, false
	}
	if parsed >= maxRomBytes {
		return 0, false
	}
	return uint32(parsed), true
}

// kind is the JSON type of raw: '"' string, '{' object, '[' array, 'b'
// boolean, 'n' number, 0 null or nothing.
func kind(raw json.RawMessage) byte {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return 0
	}
	switch c := text[0]; {
	case c == '"' || c == '{' || c == '[':
		return c
	case c == 't' || c == 'f':
		return 'b'
	case c == '-' || (c >= '0' && c <= '9'):
		return 'n'
	}
	return 0
}

// object decodes a JSON object with case-sensitive keys, as RMG-K reads them.
func object(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if kind(raw) != '{' {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	return m, true
}

func stringMember(m map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := m[key]
	if !ok || kind(raw) != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// Parse reads one pack file; file names it in errors and logs. Options that
// break a rule are skipped and logged; a pack left without any is an error.
func Parse(data []byte, file string) (*Pack, error) {
	data = []byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))
	if !json.Valid(data) {
		return nil, errors.New("isn't valid JSON")
	}
	root, ok := object(data)
	if !ok {
		return nil, errors.New("isn't a JSON object")
	}
	if format := root["format"]; kind(format) != 'n' || strings.TrimSpace(string(format)) != strconv.Itoa(packFormat) {
		return nil, fmt.Errorf("has an unknown format (this version reads format %d)", packFormat)
	}
	pack := &Pack{File: file}
	id, ok := stringMember(root, "id")
	if !ok || !validPackID(id) {
		return nil, errors.New("has no valid id ([a-z0-9._-], up to 64)")
	}
	pack.ID = id
	pack.Game = id
	if game, ok := stringMember(root, "game"); ok && validLabel(game) {
		pack.Game = game
	}

	// one MD5, or a list of them
	var md5s []json.RawMessage
	if raw, ok := root["romMd5"]; ok {
		if kind(raw) == '[' {
			if json.Unmarshal(raw, &md5s) != nil {
				return nil, errors.New("has a romMd5 that isn't 32 hex digits")
			}
		} else {
			md5s = []json.RawMessage{raw}
		}
	}
	for _, raw := range md5s {
		var md5 string
		if kind(raw) != '"' || json.Unmarshal(raw, &md5) != nil || !validMD5(md5) {
			return nil, errors.New("has a romMd5 that isn't 32 hex digits")
		}
		pack.RomMD5 = append(pack.RomMD5, strings.ToUpper(md5))
	}
	if len(pack.RomMD5) == 0 {
		return nil, errors.New("names no ROM (romMd5)")
	}

	var options []json.RawMessage
	if raw := root["options"]; kind(raw) != '[' || json.Unmarshal(raw, &options) != nil {
		return nil, errors.New("has no options")
	}
	for _, raw := range options {
		opt, err := parseOption(raw)
		if err != nil {
			slog.Warn("game options pack: skipped an option", "file", file, "err", err)
			continue
		}
		duplicate := false
		for _, other := range pack.Options {
			duplicate = duplicate || other.ID == opt.ID
		}
		if duplicate {
			slog.Warn("game options pack: skipped a second option", "file", file, "option", opt.ID)
			continue
		}
		pack.Options = append(pack.Options, opt)
	}
	if len(pack.Options) == 0 {
		return nil, errors.New("has no usable options")
	}
	return pack, nil
}

func parseOption(raw json.RawMessage) (*Option, error) {
	m, ok := object(raw)
	if !ok {
		return nil, errors.New("an option isn't an object")
	}
	id, ok := stringMember(m, "id")
	if !ok {
		return nil, errors.New("an option has no id ([a-z0-9-], up to 32)")
	}
	if !ValidOptionID(id) {
		return nil, fmt.Errorf("option id %q isn't valid ([a-z0-9-], up to 32)", id)
	}
	opt := &Option{ID: id}
	if opt.Label, ok = stringMember(m, "label"); !ok || !validLabel(opt.Label) {
		return nil, fmt.Errorf("option %q has no valid label", id)
	}
	opt.Description, _ = stringMember(m, "description")
	if serverOnly := m["serverOnly"]; kind(serverOnly) == 'b' {
		opt.ServerOnly = strings.TrimSpace(string(serverOnly)) == "true"
	}

	var patches []json.RawMessage
	if raw := m["patches"]; kind(raw) != '[' || json.Unmarshal(raw, &patches) != nil || len(patches) == 0 {
		return nil, fmt.Errorf("option %q has no patches", id)
	}
	if len(patches) > MaxPatches {
		return nil, fmt.Errorf("option %q has more than %d patches", id, MaxPatches)
	}
	total := 0
	for _, raw := range patches {
		entry, _ := object(raw)
		var patch Patch
		offset, ok := entry["offset"]
		if !ok {
			return nil, fmt.Errorf("option %q has a patch without a valid offset", id)
		}
		if patch.Offset, ok = parseOffset(offset); !ok {
			return nil, fmt.Errorf("option %q has a patch without a valid offset", id)
		}
		bytesText, okBytes := stringMember(entry, "bytes")
		expectText, okExpect := stringMember(entry, "expect")
		var okHex1, okHex2 bool
		patch.Bytes, okHex1 = parseHexBytes(bytesText)
		patch.Expect, okHex2 = parseHexBytes(expectText)
		if !okBytes || !okExpect || !okHex1 || !okHex2 || len(patch.Bytes) != len(patch.Expect) {
			return nil, fmt.Errorf("option %q has a patch whose expect and bytes aren't equal-length hex", id)
		}
		if patch.end() > maxRomBytes {
			return nil, fmt.Errorf("option %q has a patch past the end of any ROM", id)
		}
		total += len(patch.Bytes)
		if total > MaxPatchBytes {
			return nil, fmt.Errorf("option %q changes more than 64 KiB", id)
		}
		for _, other := range opt.Patches {
			if patch.overlaps(other) {
				return nil, fmt.Errorf("option %q has patches that overlap", id)
			}
		}
		opt.Patches = append(opt.Patches, patch)
	}

	// An option that changes the boot code has to set new header CRCs too,
	// or the game wouldn't start.
	bootCode := Patch{Offset: bootCodeStart, Bytes: make([]byte, bootCodeEnd-bootCodeStart)}
	changesBootCode := false
	for _, p := range opt.Patches {
		changesBootCode = changesBootCode || p.overlaps(bootCode)
	}
	if changesBootCode {
		for crcByte := uint64(headerCRCs); crcByte < headerCRCsEnd; crcByte++ {
			covered := false
			for _, p := range opt.Patches {
				covered = covered || (crcByte >= uint64(p.Offset) && crcByte < p.end())
			}
			if !covered {
				return nil, fmt.Errorf("option %q changes the boot code (0x1000-0x101000) without setting "+
					"the header CRCs (0x10-0x17), so the game wouldn't start", id)
			}
		}
	}
	return opt, nil
}
