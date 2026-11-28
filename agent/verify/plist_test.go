package verify

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func goldenInfo() map[string]any {
	return map[string]any{
		"CFBundleIdentifier":         "io.appwrite.distribution.golden",
		"CFBundleShortVersionString": "2.4.1",
		"CFBundleVersion":            "241",
		"CFBundleExecutable":         "Golden",
		"CFBundleDisplayName":        "Café ☕ 日本 🚀",
		"CFBundleSupportedPlatforms": []any{"iPhoneOS"},
		"LSRequiresIPhoneOS":         true,
		"UIRequiresFullScreen":       false,
		"BuildDate":                  time.Date(2026, 11, 23, 9, 30, 15, 0, time.UTC),
		"Checksum":                   []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01, 0x02, 0x03, 0xFF},
		"Counts": map[string]any{
			"small":    int64(7),
			"byte":     int64(200),
			"short":    int64(65535),
			"word":     int64(4294967296),
			"negative": int64(-42),
			"maximum":  int64(math.MaxInt64),
			"unsigned": uint64(math.MaxUint64),
			"ratio":    0.5,
			"pi":       3.14159,
		},
		"UIApplicationSceneManifest": map[string]any{
			"UISceneConfigurations": map[string]any{
				"UIWindowSceneSessionRoleApplication": []any{
					map[string]any{
						"UISceneConfigurationName": "Default Configuration",
						"UISceneDelegateClassName": "Golden.SceneDelegate",
					},
					[]any{int64(1), "nested", []any{}, map[string]any{}},
				},
			},
		},
		"Empty":      "",
		"LongString": "abcdefghijklmnopqrstuvwxyz0123456789",
	}
}

func goldenEntitlements() map[string]any {
	return map[string]any{
		"application-identifier":              "ABCDE12345.io.appwrite.distribution.golden",
		"com.apple.developer.team-identifier": "ABCDE12345",
		"aps-environment":                     "production",
		"get-task-allow":                      false,
		"keychain-access-groups": []any{
			"ABCDE12345.io.appwrite.distribution.golden",
			"ABCDE12345.shared",
		},
		"com.apple.security.application-groups": []any{"group.io.appwrite.distribution.golden"},
	}
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDecodeGolden(t *testing.T) {
	cases := []struct {
		file     string
		binary   bool
		expected map[string]any
	}{
		{file: "Info.binary.plist", binary: true, expected: goldenInfo()},
		{file: "Info.xml.plist", expected: goldenInfo()},
		{file: "entitlements.binary.plist", binary: true, expected: goldenEntitlements()},
	}
	for _, test := range cases {
		t.Run(test.file, func(t *testing.T) {
			data := readGolden(t, test.file)
			if bytes.HasPrefix(data, binaryMagic) != test.binary {
				t.Fatalf("%s is not the expected format", test.file)
			}
			decoded, err := decodeDictionary(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(decoded, test.expected) {
				t.Fatalf("decoded %#v\nwant %#v", decoded, test.expected)
			}
		})
	}
}

func TestGoldenBinaryUsesUTF16AndWideIntegers(t *testing.T) {
	data := readGolden(t, "Info.binary.plist")
	if !bytes.Contains(data, []byte{0x00, 0x43, 0x00, 0x61, 0x00, 0x66, 0x00, 0xE9}) {
		t.Fatal("golden binary plist has no UTF-16 string, so the UTF-16 path is untested")
	}
	if !bytes.Contains(data, append([]byte{0x14}, bytes.Repeat([]byte{0}, 8)...)) {
		t.Fatal("golden binary plist has no 16-byte integer, so the wide integer path is untested")
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	long := make([]any, 300)
	for index := range long {
		long[index] = int64(index * 1000)
	}
	value := map[string]any{
		"golden":   goldenInfo(),
		"long":     long,
		"text":     strings.Repeat("x", 70000),
		"unicode":  strings.Repeat("ü🚀", 20),
		"data":     bytes.Repeat([]byte{0xAB}, 300),
		"minimum":  int64(math.MinInt64),
		"fraction": time.Date(1999, 12, 31, 23, 59, 59, 500_000_000, time.UTC),
		"empty":    map[string]any{},
	}
	for name, encode := range map[string]func(any) ([]byte, error){"binary": encodeBinary, "xml": encodeXML} {
		t.Run(name, func(t *testing.T) {
			data, err := encode(value)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodePlist(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			expected := value
			if name == "xml" {
				expected = cloneWithout(value, "fraction")
				decoded = cloneWithout(decoded.(map[string]any), "fraction")
			}
			if !reflect.DeepEqual(decoded, expected) {
				t.Fatal("round trip changed the value")
			}
		})
	}
}

func cloneWithout(value map[string]any, key string) map[string]any {
	clone := make(map[string]any, len(value))
	for name, element := range value {
		if name != key {
			clone[name] = element
		}
	}
	return clone
}

func TestEncodeAcceptedByPlutil(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil is only on macOS")
	}
	for name, encode := range map[string]func(any) ([]byte, error){"binary": encodeBinary, "xml": encodeXML} {
		t.Run(name, func(t *testing.T) {
			data, err := encode(goldenInfo())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "Info.plist")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command(plutil, "-convert", "xml1", "-o", "-", path).CombinedOutput()
			if err != nil {
				t.Fatalf("plutil rejected the encoding: %v\n%s", err, output)
			}
			if !bytes.Equal(output, readGolden(t, "Info.xml.plist")) {
				t.Fatalf("plutil reads a different value:\n%s", output)
			}
		})
	}
}

// rawBinaryPlist builds a bplist00 file from raw objects, the first being the top object, with one-byte offsets and references.
func rawBinaryPlist(objects ...[]byte) []byte {
	data := bytes.Clone(binaryMagic)
	offsets := make([]byte, 0, len(objects))
	for _, object := range objects {
		offsets = append(offsets, byte(len(data)))
		data = append(data, object...)
	}
	table := len(data)
	data = append(data, offsets...)
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 1, 1
	binary.BigEndian.PutUint64(trailer[8:], uint64(len(objects)))
	binary.BigEndian.PutUint64(trailer[24:], uint64(table))
	return append(data, trailer...)
}

func TestDecodeBinaryRaw(t *testing.T) {
	cases := []struct {
		name     string
		data     []byte
		expected any
	}{
		{name: "one byte integer", data: rawBinaryPlist([]byte{0x10, 0xFF}), expected: int64(255)},
		{name: "two byte integer", data: rawBinaryPlist([]byte{0x11, 0x01, 0x00}), expected: int64(256)},
		{name: "four byte integer", data: rawBinaryPlist([]byte{0x12, 0x80, 0, 0, 0}), expected: int64(1 << 31)},
		{name: "negative integer", data: rawBinaryPlist([]byte{0x13, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFE}), expected: int64(-2)},
		{name: "float32", data: rawBinaryPlist([]byte{0x22, 0x3F, 0xC0, 0, 0}), expected: 1.5},
		{name: "date", data: rawBinaryPlist([]byte{0x33, 0, 0, 0, 0, 0, 0, 0, 0}), expected: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "latin1 ascii string", data: rawBinaryPlist([]byte{0x52, 'h', 0xE9}), expected: "hé"},
		{name: "extended length data", data: rawBinaryPlist(append([]byte{0x4F, 0x10, 16}, bytes.Repeat([]byte{7}, 16)...)), expected: bytes.Repeat([]byte{7}, 16)},
		{name: "surrogate pair", data: rawBinaryPlist([]byte{0x62, 0xD8, 0x3D, 0xDE, 0x80}), expected: "🚀"},
		{name: "shared reference", data: rawBinaryPlist([]byte{0xA2, 1, 1}, []byte{0x51, 'a'}), expected: []any{"a", "a"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			decoded, err := decodePlist(test.data)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, test.expected) {
				t.Fatalf("decoded %#v, want %#v", decoded, test.expected)
			}
		})
	}
}

func TestDecodeBinaryRejects(t *testing.T) {
	valid := rawBinaryPlist([]byte{0xD1, 1, 2}, []byte{0x51, 'k'}, []byte{0x51, 'v'})
	withTrailer := func(mutate func([]byte)) []byte {
		data := bytes.Clone(valid)
		mutate(data[len(data)-32:])
		return data
	}
	var nested any = false
	for range plistDepth + 2 {
		nested = []any{nested}
	}
	deep, err := encodeBinary(nested)
	if err != nil {
		t.Fatal(err)
	}
	headerOffset := bytes.Clone(valid)
	headerOffset[len(valid)-32-3] = 2
	cases := map[string][]byte{
		"truncated":                valid[:20],
		"offset size zero":         withTrailer(func(trailer []byte) { trailer[6] = 0 }),
		"reference size three":     withTrailer(func(trailer []byte) { trailer[7] = 3 }),
		"no objects":               withTrailer(func(trailer []byte) { binary.BigEndian.PutUint64(trailer[8:], 0) }),
		"object count overflow":    withTrailer(func(trailer []byte) { binary.BigEndian.PutUint64(trailer[8:], math.MaxUint64) }),
		"top object out of range":  withTrailer(func(trailer []byte) { binary.BigEndian.PutUint64(trailer[16:], 3) }),
		"table past end":           withTrailer(func(trailer []byte) { binary.BigEndian.PutUint64(trailer[24:], math.MaxUint64) }),
		"offset into header":       headerOffset,
		"self reference":           rawBinaryPlist([]byte{0xA1, 0}),
		"mutual reference":         rawBinaryPlist([]byte{0xA1, 1}, []byte{0xD1, 2, 0}, []byte{0x51, 'k'}),
		"reference out of range":   rawBinaryPlist([]byte{0xA1, 9}),
		"non string key":           rawBinaryPlist([]byte{0xD1, 1, 1}, []byte{0x10, 1}),
		"duplicate key":            rawBinaryPlist([]byte{0xD2, 1, 1, 2, 2}, []byte{0x51, 'k'}, []byte{0x09}),
		"string past object area":  rawBinaryPlist([]byte{0x5E, 'a'}),
		"utf16 past object area":   rawBinaryPlist([]byte{0x6E, 0, 'a'}),
		"huge container":           rawBinaryPlist([]byte{0xAF, 0x13, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}),
		"bad length marker":        rawBinaryPlist([]byte{0x5F, 0x51, 'a'}),
		"integer of 32 bytes":      rawBinaryPlist([]byte{0x15, 0}),
		"integer beyond 64 bits":   rawBinaryPlist(append([]byte{0x14, 0, 0, 0, 0, 0, 0, 0, 1}, make([]byte, 8)...)),
		"real of two bytes":        rawBinaryPlist([]byte{0x21, 0, 0}),
		"nan date":                 rawBinaryPlist([]byte{0x33, 0x7F, 0xF8, 0, 0, 0, 0, 0, 0}),
		"null":                     rawBinaryPlist([]byte{0x00}),
		"uid":                      rawBinaryPlist([]byte{0x80, 1}),
		"set":                      rawBinaryPlist([]byte{0xC0}),
		"nested past depth limit":  deep,
		"truncated integer":        rawBinaryPlist([]byte{0x13, 1}),
		"truncated extended count": rawBinaryPlist([]byte{0x4F, 0x13}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodePlist(data); !errors.Is(err, errPlist) {
				t.Fatalf("got %v, want errPlist", err)
			}
		})
	}
}

func TestDecodeBinaryExpansionBudget(t *testing.T) {
	objects := make([][]byte, 0, 24)
	for index := range 23 {
		objects = append(objects, []byte{0xA2, byte(index + 1), byte(index + 1)})
	}
	objects = append(objects, []byte{0x09})
	if _, err := decodePlist(rawBinaryPlist(objects...)); !errors.Is(err, errPlist) || !strings.Contains(err.Error(), "times its size") {
		t.Fatalf("a 2^23 node expansion decoded: %v", err)
	}
}

// widePlist builds a bplist00 file like rawBinaryPlist, with eight-byte offsets so objects may be large.
func widePlist(objects ...[]byte) []byte {
	data := bytes.Clone(binaryMagic)
	var offsets []byte
	for _, object := range objects {
		offsets = binary.BigEndian.AppendUint64(offsets, uint64(len(data)))
		data = append(data, object...)
	}
	table := len(data)
	data = append(data, offsets...)
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 8, 1
	binary.BigEndian.PutUint64(trailer[8:], uint64(len(objects)))
	binary.BigEndian.PutUint64(trailer[24:], uint64(table))
	return append(data, trailer...)
}

// sharedString is an array of count references to one string of size bytes.
func sharedString(size int, count int) []byte {
	array := binary.BigEndian.AppendUint32([]byte{0xAF, 0x12}, uint32(count))
	array = append(array, bytes.Repeat([]byte{1}, count)...)
	text := binary.BigEndian.AppendUint32([]byte{0x5F, 0x12}, uint32(size))
	text = append(text, bytes.Repeat([]byte{'a'}, size)...)
	return widePlist(array, text)
}

// TestDecodeBinarySharedStringIsBounded is the review's reproduction: 110 KB that referenced one 100 KB string
// 10,000 times allocated about 1 GB before shared objects were decoded once and the decoded size was budgeted.
func TestDecodeBinarySharedStringIsBounded(t *testing.T) {
	data := sharedString(100_000, 10_000)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	_, err := decodePlist(data)

	runtime.ReadMemStats(&after)
	if !errors.Is(err, errPlist) || !strings.Contains(err.Error(), "times its size") {
		t.Fatalf("decode = %v, want the decoded-size budget to refuse it", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("decoding %d bytes allocated %d bytes", len(data), allocated)
	}
}

func TestDecodeBinarySharesDecodedObjects(t *testing.T) {
	data := sharedString(1000, 50)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	decoded, err := decodePlist(data)

	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	array, ok := decoded.([]any)
	if !ok || len(array) != 50 || array[49] != strings.Repeat("a", 1000) {
		t.Fatalf("decoded %T of %d", decoded, len(array))
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 20_000 {
		t.Fatalf("decoding 50 references to one 1000-byte string allocated %d bytes, want the string once", allocated)
	}
}

func TestDecodeXMLRejects(t *testing.T) {
	wrap := func(body string) []byte {
		return []byte(xmlPrologue + body + "\n</plist>\n")
	}
	deep := strings.Repeat("<array>", plistDepth+2) + strings.Repeat("</array>", plistDepth+2)
	cases := map[string][]byte{
		"empty":                 []byte(""),
		"not xml":               []byte("\x00\x01garbage"),
		"no plist root":         []byte("<dict/>"),
		"empty plist":           wrap(""),
		"two values":            wrap("<string>a</string><string>b</string>"),
		"content after plist":   append(wrap("<true/>"), []byte("<true/>")...),
		"unknown element":       wrap("<object/>"),
		"key outside dict":      wrap("<key>a</key>"),
		"value without key":     wrap("<dict><string>a</string></dict>"),
		"key without value":     wrap("<dict><key>a</key></dict>"),
		"duplicate key":         wrap("<dict><key>a</key><true/><key>a</key><false/></dict>"),
		"bad integer":           wrap("<integer>12a</integer>"),
		"integer overflow":      wrap("<integer>18446744073709551616</integer>"),
		"negative overflow":     wrap("<integer>-9223372036854775809</integer>"),
		"bad real":              wrap("<real>pi</real>"),
		"bad date":              wrap("<date>yesterday</date>"),
		"bad data":              wrap("<data>!!!</data>"),
		"text in true":          wrap("<true>yes</true>"),
		"element in string":     wrap("<string><true/></string>"),
		"stray text":            wrap("<dict>text<key>a</key><true/></dict>"),
		"unclosed":              []byte(xmlPrologue + "<dict>"),
		"undefined entity":      wrap("<string>&secret;</string>"),
		"nested past the limit": wrap(deep),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodePlist(data); !errors.Is(err, errPlist) {
				t.Fatalf("got %v, want errPlist", err)
			}
		})
	}
}

func TestDecodeXMLScalars(t *testing.T) {
	cases := map[string]any{
		"<integer> 0x1F </integer>":               int64(31),
		"<integer>+5</integer>":                   int64(5),
		"<integer>010</integer>":                  int64(10),
		"<integer>-9223372036854775808</integer>": int64(math.MinInt64),
		"<real>-1.5e3</real>":                     -1500.0,
		"<data>\n\t3q2+\n\t7w==\n</data>":         []byte{0xDE, 0xAD, 0xBE, 0xEF},
		"<string>a &amp; b &lt;c&gt;</string>":    "a & b <c>",
		"<string><!-- note -->kept</string>":      "kept",
		"<date>2026-11-23T09:30:15Z</date>":       time.Date(2026, 11, 23, 9, 30, 15, 0, time.UTC),
	}
	for body, expected := range cases {
		t.Run(body, func(t *testing.T) {
			decoded, err := decodePlist([]byte("\xef\xbb\xbf" + xmlPrologue + body + "</plist>"))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, expected) {
				t.Fatalf("decoded %#v, want %#v", decoded, expected)
			}
		})
	}
}

func FuzzDecodePlist(f *testing.F) {
	for _, name := range []string{"Info.binary.plist", "Info.xml.plist", "entitlements.binary.plist"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		value, err := decodePlist(data)
		if err != nil {
			return
		}
		if _, err := encodeBinary(value); err != nil {
			t.Fatalf("decoded a value the encoder cannot hold: %v", err)
		}
	})
}
