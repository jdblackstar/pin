package app

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

func TestInitRejectsInvalidUTF8WithoutWritingConfig(t *testing.T) {
	for _, key := range []string{"name", "entrypoint", "source", "requirements", "inject", "branch", "remote", "preflight", "verify"} {
		for _, invalid := range []string{"\x8d", "\xff", "\xc0\xaf"} {
			t.Run(key+"/"+strings.ToValidUTF8(invalid, "invalid"), func(t *testing.T) {
				root := t.TempDir()
				value := "bad" + invalid
				if key == "source" {
					value += ".py"
				}
				args := []string{"init", "--name", "demo", "--source", "tool.py", "--branch", "main", "--remote", "origin", "--" + key, value, root}
				result := runPin(t, t.TempDir(), args...)
				requireCode(t, result, 2)
				requireContains(t, result.stderr, `pin.toml key "`+key+`" must be valid UTF-8`)
				if _, err := os.Stat(filepath.Join(root, configName)); !os.IsNotExist(err) {
					t.Fatalf("invalid input wrote pin.toml: %v", err)
				}
			})
		}
	}
	if _, err := renderInitConfig(initOptions{branch: "main", remote: "origin"}, "/disposable/bad\xff"); err == nil {
		t.Fatal("accepted invalid UTF-8 in inferred name")
	}
}

// These targets retain the parser assessment's properties and 64-KiB input
// bound. The byte target also covers the UTF-8-only target without filtering
// the invalid inputs that originally exposed the serializer failure.
const parserFuzzCap = 64 << 10

func fuzzInitOptions(field byte, value string) initOptions {
	o := initOptions{name: "demo", source: "tool.py", branch: "main", remote: "origin"}
	switch field % 9 {
	case 0:
		o.name = value
	case 1:
		o.entrypoint = value
	case 2:
		o.source = value
	case 3:
		o.requirements = value
	case 4:
		o.inject = repeatedStringFlag{value}
	case 5:
		o.branch = value
	case 6:
		o.remote = value
	case 7:
		o.preflight = repeatedStringFlag{value}
	case 8:
		o.verify = repeatedStringFlag{value}
	}
	// Branch discovery is exercised by CLI tests, not a subprocess per fuzz input.
	if o.branch == "" {
		o.branch = "main"
	}
	return o
}

func FuzzInitRoundTrip(f *testing.F) {
	for field := byte(0); field < 9; field++ {
		for _, value := range []string{"demo", "é", "", "\xff", "\xc0\xaf"} {
			f.Add(field, value)
		}
	}
	// Original minimized failure: entrypoint is the single byte 0x8d.
	f.Add(byte(1), "\x8d")
	f.Add(byte(8), `python3 -c "print('hello')"`)
	f.Add(byte(2), "a/../tool.py")
	f.Add(byte(4), "tokens/../state")
	f.Fuzz(func(t *testing.T, field byte, value string) {
		if len(value) > parserFuzzCap {
			t.Skip()
		}
		o := fuzzInitOptions(field, value)
		text, err := renderInitConfig(o, "/disposable/demo")
		if !utf8.ValidString(value) && err == nil {
			t.Fatalf("accepted invalid UTF-8: field=%d value=%q", field%9, value)
		}
		if err != nil {
			return
		}
		c, err := decodeConfig([]byte(text), "/disposable/demo", configName)
		if err != nil {
			t.Fatalf("accepted init field=%d value=%q cannot decode: %v", field%9, value, err)
		}
		name := o.name
		if name == "" {
			name = "demo"
		}
		entrypoint := o.entrypoint
		if entrypoint == "" {
			entrypoint = name
		}
		if c.name != name || c.entrypoint != entrypoint || c.branch != o.branch || c.remote != o.remote {
			t.Fatalf("changed scalar strings: field=%d value=%q config=%#v", field%9, value, c)
		}
		source := o.source
		if source == "" {
			source = "."
		}
		if c.source.path != filepath.Clean(source) || c.requirements != filepath.Clean(o.requirements) && o.requirements != "" {
			t.Fatalf("changed source or requirements: %#v", c)
		}
		if len(o.inject) != 0 && (len(c.inject) != 1 || c.inject[0] != filepath.Clean(o.inject[0])) {
			t.Fatalf("changed injection: %q -> %q", o.inject, c.inject)
		}
		for key, input := range map[string]repeatedStringFlag{"preflight": o.preflight, "verify": o.verify} {
			if len(input) == 0 {
				continue
			}
			want, err := splitCommand(input[0])
			if err != nil {
				t.Fatal(err)
			}
			got := c.preflight
			if key == "verify" {
				got = c.verify
			}
			if !reflect.DeepEqual(got, [][]string{want}) {
				t.Fatalf("%s changed: got=%q want=%q", key, got, want)
			}
		}
	})
}

func FuzzConfigRoundTrip(f *testing.F) {
	for _, seed := range []string{
		"name='demo'\nsource='.'\n",
		"name='d'\nsource='tool.py'\nrequirements='requirements.txt'\ninject=['.env','config/local']\npreflight=['echo hello', ['python3','-c','print(1)']]\nverify=[]\n",
		"name='d'\nsource='.'\nverify=[['echo','','a b']]\n",
		"name='d'\nsource='.'\nunknown={x=1979-05-27T07:32:00Z}\n",
		"name='d'\nsource='.'\nverify=[[1]]\n",
		"name='d'\nsource='../escape'\n",
		"name='d'\nsource='.'\ninject=['a','a/b']\n",
		"name='d'\nsource='.'\nname='duplicate'\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > parserFuzzCap {
			t.Skip()
		}
		c, err := decodeConfig(data, "/disposable/demo", configName)
		if err != nil {
			return
		}
		var b bytes.Buffer
		if err := toml.NewEncoder(&b).Encode(c.raw); err != nil {
			t.Fatalf("accepted TOML cannot encode: %v", err)
		}
		d, err := decodeConfig(b.Bytes(), c.sourcePath, c.configPath)
		if err != nil {
			t.Fatalf("accepted config cannot redecode: %v", err)
		}
		c.raw, d.raw = nil, nil
		if !reflect.DeepEqual(c, d) {
			t.Fatalf("decode/encode changed semantics: input=%q", data)
		}
	})
}

func FuzzCommandQuoting(f *testing.F) {
	for _, pair := range [][2]string{{"", ""}, {"echo", "a b"}, {"é", "'\\\"\n\t"}, {"a\x00b", "tail"}} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		if len(a)+len(b) > parserFuzzCap || !utf8.ValidString(a) || !utf8.ValidString(b) {
			t.Skip()
		}
		literal := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
		input := literal(a) + " " + literal(b)
		got, err := splitCommand(input)
		if err != nil || !reflect.DeepEqual(got, []string{a, b}) {
			t.Fatalf("quoted strings changed: got=%q err=%v want=%q", got, err, []string{a, b})
		}
		argv, err := splitCommand(a)
		if err != nil || len(argv) == 0 {
			return
		}
		x, xerr := parseCommands([]any{a}, "verify")
		y, yerr := parseCommands([][]string{argv}, "verify")
		if xerr != nil || yerr != nil || !reflect.DeepEqual(x, y) {
			t.Fatalf("string/argv differ: %q (%v) != %q (%v)", x, xerr, y, yerr)
		}
	})
}

func FuzzRuntimePaths(f *testing.F) {
	for _, p := range []string{"tokens", "tokens/file", "tokens2/file", "a/../tokens", "../escape", "/absolute", ".pin/file", ".venv", ".cache/file", ".", "a\x00b", "a\\..\\b"} {
		f.Add(p, "tokens")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		if len(a)+len(b) > parserFuzzCap {
			t.Skip()
		}
		for _, p := range []string{a, b} {
			clean, err := validateRelativePath(p, "fuzz input")
			if err != nil {
				continue
			}
			if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				t.Fatalf("unsafe normalized path %q from %q", clean, p)
			}
			again, err := validateRelativePath(clean, "fuzz input")
			if err != nil || clean != again {
				t.Fatal("normalization not idempotent")
			}
		}
		initPaths, initErr := validateRuntimePaths(repeatedStringFlag{a, b}, "inject")
		decodePaths, decodeErr := optionalRelativePathList(map[string]any{"inject": []string{a, b}}, "inject", nil)
		if (initErr == nil) != (decodeErr == nil) || !reflect.DeepEqual(initPaths, decodePaths) {
			t.Fatalf("init/decode path validation mismatch: %q,%q", a, b)
		}
	})
}
