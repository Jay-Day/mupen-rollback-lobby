package gameoptions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const remixMD5 = "2B2D6B295106C54216B7FC7A2F14346E"

// pack is a pack file offering options (JSON objects) for the Remix ROM.
func pack(id string, options ...string) string {
	return `{"format":1,"id":"` + id + `","game":"Test","romMd5":["` + remixMD5 + `"],"options":[` +
		strings.Join(options, ",") + `]}`
}

// option is an option with one patch per offset, each two bytes long.
func option(id string, offsets ...uint32) string {
	var patches []string
	for _, off := range offsets {
		patches = append(patches, fmt.Sprintf(`{"offset":"0x%08X","expect":"0000","bytes":"FFFF"}`, off))
	}
	return `{"id":"` + id + `","label":"` + id + `","patches":[` + strings.Join(patches, ",") + `]}`
}

func mustParse(t *testing.T, data string) *Pack {
	t.Helper()
	p, err := Parse([]byte(data), "test.json")
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, data)
	}
	return p
}

// The pack RMG-K ships for Smash Remix 2.0.1, copied into the repo for
// deployment, loads with its option intact.
func TestShippedRemixPack(t *testing.T) {
	set, err := LoadDir("../../gameoptions")
	if err != nil {
		t.Fatal(err)
	}
	opt, ok := set.Option(strings.ToLower(remixMD5), "vanilla-only")
	if !ok {
		t.Fatalf("vanilla-only not offered; packs %+v", set.Packs())
	}
	if opt.Label != "Vanilla only" || opt.ServerOnly || len(opt.Patches) != 8 || opt.PatchBytes() != 152 {
		t.Fatalf("vanilla-only = %q serverOnly %v, %d patches, %d bytes",
			opt.Label, opt.ServerOnly, len(opt.Patches), opt.PatchBytes())
	}
	first := opt.Patches[0]
	if first.Offset != 0x02D1EE10 || !bytes.Equal(first.Expect[:4], []byte{0x3C, 0x08, 0x80, 0x51}) ||
		!bytes.Equal(first.Bytes[:4], []byte{0x2C, 0x48, 0x00, 0x0C}) {
		t.Fatalf("first patch = %+v", first)
	}
}

func TestLoadDirMissingIsEmpty(t *testing.T) {
	set, err := LoadDir(filepath.Join(t.TempDir(), "missing"))
	if err != nil || len(set.Packs()) != 0 {
		t.Fatalf("LoadDir(missing) = %+v, %v", set, err)
	}
}

// Packs load in file name order; a later file with an earlier pack's id
// replaces it in place; bad packs and other files are skipped.
func TestLoadDirOrderAndReplacement(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.json", pack("first", option("x", 0x2100000)))
	write("b.json", pack("second", option("x", 0x200000), option("y", 0x200010)))
	write("c.JSON", pack("first", option("z", 0x300000)))
	write("d.json", `{"format":1,"id":"broken"`)
	write("notes.txt", pack("third", option("w", 0x400000)))
	if err := os.Mkdir(filepath.Join(dir, "e.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	set, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range set.Packs() {
		ids = append(ids, p.ID+"@"+p.File)
	}
	if got := strings.Join(ids, " "); got != "first@c.JSON second@b.json" {
		t.Fatalf("packs = %s", got)
	}
	// the first pack offering an id for the ROM wins
	if opt, ok := set.Option(remixMD5, "x"); !ok || opt.Patches[0].Offset != 0x200000 {
		t.Fatalf("x = %+v, %v", opt, ok)
	}
	if opt, ok := set.Option(remixMD5, "z"); !ok || opt.Patches[0].Offset != 0x300000 {
		t.Fatalf("z = %+v, %v", opt, ok)
	}
	if _, ok := set.Option(remixMD5, "w"); ok {
		t.Fatal("a .txt file was loaded")
	}
	if _, ok := set.Option("00000000000000000000000000000000", "x"); ok {
		t.Fatal("an option was offered for another ROM")
	}
}

func TestParseAcceptsTheFormatsRMGKReads(t *testing.T) {
	p := mustParse(t, "\xef\xbb\xbf"+`{"format":1,"id":"remix.v2_0-1","romMd5":"2b2d6b295106c54216b7fc7a2f14346e","unknown":[1,2],
		"options":[
		  {"id":"num","label":"日本語のラベル","serverOnly":true,"translations":{"ja":{"label":"x"}},"patches":[
		    {"offset":46263824,"expect":"3c 08 80 51","bytes":"2C48000C","note":"n"}]},
		  {"id":"boot","label":"Boot code","patches":[
		    {"offset":"0x10","expect":"0000000000000000","bytes":"0102030405060708"},
		    {"offset":"00001000","expect":"00","bytes":"01"}]}]}`)
	if p.ID != "remix.v2_0-1" || p.Game != "remix.v2_0-1" || len(p.RomMD5) != 1 || p.RomMD5[0] != remixMD5 {
		t.Fatalf("pack = %+v", p)
	}
	if len(p.Options) != 2 {
		t.Fatalf("options = %d", len(p.Options))
	}
	num := p.Options[0]
	if !num.ServerOnly || num.Patches[0].Offset != 0x02C1EE10 ||
		!bytes.Equal(num.Patches[0].Expect, []byte{0x3C, 0x08, 0x80, 0x51}) {
		t.Fatalf("num = %+v", num)
	}
	if p.Options[1].ServerOnly {
		t.Fatal("serverOnly without the field")
	}
}

func TestParseRefusesBadPacks(t *testing.T) {
	good := option("ok", 0x2100000)
	for _, bad := range []string{
		`[]`,
		`{"format":2,"id":"p","romMd5":"` + remixMD5 + `","options":[` + good + `]}`,
		`{"format":"1","id":"p","romMd5":"` + remixMD5 + `","options":[` + good + `]}`,
		`{"format":1.0,"id":"p","romMd5":"` + remixMD5 + `","options":[` + good + `]}`,
		`{"id":"p","romMd5":"` + remixMD5 + `","options":[` + good + `]}`,
		`{"format":1,"id":"P","romMd5":"` + remixMD5 + `","options":[` + good + `]}`,
		`{"format":1,"romMd5":"` + remixMD5 + `","options":[` + good + `]}`,
		`{"format":1,"id":"p","options":[` + good + `]}`,
		`{"format":1,"id":"p","romMd5":[],"options":[` + good + `]}`,
		`{"format":1,"id":"p","romMd5":["` + remixMD5[1:] + `"],"options":[` + good + `]}`,
		`{"format":1,"id":"p","romMd5":null,"options":[` + good + `]}`,
		`{"format":1,"id":"p","romMd5":"` + remixMD5 + `","options":{}}`,
		`{"format":1,"id":"p","romMd5":"` + remixMD5 + `","options":[]}`,
		`{"format":1,"id":"p","romMd5":"` + remixMD5 + `","options":[` + good + `]`,
		`{"Format":1,"id":"p","romMd5":"` + remixMD5 + `","options":[` + good + `]}`,
	} {
		if p, err := Parse([]byte(bad), "bad.json"); err == nil {
			t.Errorf("accepted %s: %+v", bad, p)
		}
	}
}

// An option that breaks a rule is skipped; the pack keeps its others.
func TestParseSkipsBadOptions(t *testing.T) {
	patch := func(fields string) string {
		return `{"id":"bad","label":"Bad","patches":[` + fields + `]}`
	}
	many := make([]string, MaxPatches+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"offset":%d,"expect":"00","bytes":"01"}`, 0x2100000+i)
	}
	big := strings.Repeat("00", 40*1024)
	for name, c := range map[string]struct{ json, why string }{
		"not an object":      {`"bad"`, "isn't an object"},
		"no id":              {`{"label":"Bad","patches":[{"offset":34603008,"expect":"00","bytes":"01"}]}`, "has no id"},
		"upper-case id":      {`{"id":"Bad","label":"Bad","patches":[{"offset":34603008,"expect":"00","bytes":"01"}]}`, "isn't valid"},
		"long id":            {`{"id":"` + strings.Repeat("a", 33) + `","label":"Bad","patches":[{"offset":34603008,"expect":"00","bytes":"01"}]}`, "isn't valid"},
		"no label":           {`{"id":"bad","patches":[{"offset":34603008,"expect":"00","bytes":"01"}]}`, "no valid label"},
		"control in label":   {`{"id":"bad","label":"Ba\u0007d","patches":[{"offset":34603008,"expect":"00","bytes":"01"}]}`, "no valid label"},
		"C1 control":         {`{"id":"bad","label":"Ba\u0085d","patches":[{"offset":34603008,"expect":"00","bytes":"01"}]}`, "no valid label"},
		"long label":         {`{"id":"bad","label":"` + strings.Repeat("é", 129) + `","patches":[{"offset":34603008,"expect":"00","bytes":"01"}]}`, "no valid label"},
		"no patches":         {`{"id":"bad","label":"Bad"}`, "has no patches"},
		"empty patches":      {`{"id":"bad","label":"Bad","patches":[]}`, "has no patches"},
		"no offset":          {patch(`{"expect":"00","bytes":"01"}`), "valid offset"},
		"negative offset":    {patch(`{"offset":-1,"expect":"00","bytes":"01"}`), "valid offset"},
		"fractional offset":  {patch(`{"offset":1.5,"expect":"00","bytes":"01"}`), "valid offset"},
		"exponent offset":    {patch(`{"offset":1e6,"expect":"00","bytes":"01"}`), "valid offset"},
		"nine hex digits":    {patch(`{"offset":"0x000100000","expect":"00","bytes":"01"}`), "valid offset"},
		"bare 0x":            {patch(`{"offset":"0x","expect":"00","bytes":"01"}`), "valid offset"},
		"offset out of ROM":  {patch(`{"offset":"0x0FC00000","expect":"00","bytes":"01"}`), "valid offset"},
		"past the end":       {patch(`{"offset":"0x0FBFFFFF","expect":"0000","bytes":"0101"}`), "past the end"},
		"odd hex":            {patch(`{"offset":34603008,"expect":"000","bytes":"010"}`), "equal-length hex"},
		"space in a byte":    {patch(`{"offset":34603008,"expect":"0 0","bytes":"0 1"}`), "equal-length hex"},
		"lengths differ":     {patch(`{"offset":34603008,"expect":"00","bytes":"0101"}`), "equal-length hex"},
		"no expect":          {patch(`{"offset":34603008,"bytes":"01"}`), "equal-length hex"},
		"empty bytes":        {patch(`{"offset":34603008,"expect":"","bytes":""}`), "equal-length hex"},
		"not hex":            {patch(`{"offset":34603008,"expect":"zz","bytes":"01"}`), "equal-length hex"},
		"overlap":            {patch(`{"offset":34603008,"expect":"0000","bytes":"0101"},{"offset":34603009,"expect":"00","bytes":"01"}`), "overlap"},
		"too many patches":   {patch(strings.Join(many, ",")), "more than 256 patches"},
		"over 64 KiB":        {patch(`{"offset":34603008,"expect":"` + big + `","bytes":"` + big + `"},{"offset":35651584,"expect":"` + big + `","bytes":"` + big + `"}`), "more than 64 KiB"},
		"boot code, no CRCs": {patch(`{"offset":"0x1000","expect":"00","bytes":"01"},{"offset":"0x10","expect":"00000000","bytes":"01010101"}`), "header CRCs"},
	} {
		if _, err := parseOption(json.RawMessage(c.json)); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: err = %v, want %q", name, err, c.why)
		}
		p := mustParse(t, pack("p", c.json, option("good", 0x200000)))
		if len(p.Options) != 1 || p.Options[0].ID != "good" {
			t.Errorf("%s: options = %d", name, len(p.Options))
		}
	}
	// a second option with the same id is skipped
	p := mustParse(t, pack("p", option("good", 0x200000), option("good", 0x300000)))
	if len(p.Options) != 1 || p.Options[0].Patches[0].Offset != 0x200000 {
		t.Fatalf("duplicate id: %+v", p.Options)
	}
}

func TestResolve(t *testing.T) {
	set := NewSet(
		mustParse(t, pack("one",
			option("rules", 0x2100000, 0x2100010),
			option("teams", 0x2100020),
			option("stages", 0x2100030),
			option("clash", 0x2100011))),
		mustParse(t, pack("two", option("rules", 0x500000), option("extra", 0x2100040))),
	)

	opts, err := set.Resolve(strings.ToLower(remixMD5), []string{"stages", "rules", "extra"})
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 3 || opts[0].ID != "stages" || opts[1].ID != "rules" || opts[2].ID != "extra" {
		t.Fatalf("order = %+v", opts)
	}
	// the first pack's "rules" wins
	if opts[1].Patches[0].Offset != 0x2100000 {
		t.Fatalf("rules from the wrong pack: %+v", opts[1])
	}
	if opts, err := set.Resolve(remixMD5, nil); err != nil || len(opts) != 0 {
		t.Fatalf("no ids = %v, %v", opts, err)
	}

	for name, ids := range map[string][]string{
		"unknown":  {"rules", "missing"},
		"twice":    {"rules", "teams", "rules"},
		"overlap":  {"rules", "clash"},
		"too many": {"a", "b", "c", "d", "e", "f", "g", "h", "i"},
	} {
		if _, err := set.Resolve(remixMD5, ids); err == nil {
			t.Errorf("%s: resolved %v", name, ids)
		}
	}
	if _, err := set.Resolve("00000000000000000000000000000000", []string{"rules"}); err == nil {
		t.Error("resolved for a ROM no pack is for")
	}

	// 64 KiB in all, across options
	half := strings.Repeat("00", 40*1024)
	big := NewSet(mustParse(t, pack("big",
		`{"id":"a","label":"A","patches":[{"offset":34603008,"expect":"`+half+`","bytes":"`+half+`"}]}`,
		`{"id":"b","label":"B","patches":[{"offset":35651584,"expect":"`+half+`","bytes":"`+half+`"}]}`)))
	if _, err := big.Resolve(remixMD5, []string{"a"}); err != nil {
		t.Fatalf("one 40 KiB option: %v", err)
	}
	if _, err := big.Resolve(remixMD5, []string{"a", "b"}); err == nil {
		t.Fatal("resolved 80 KiB of patches")
	}
}
