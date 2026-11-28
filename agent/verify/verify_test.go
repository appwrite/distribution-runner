package verify

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	testIdentifier  = "io.appwrite.distribution.sample"
	testVersionName = "1.2.3"
	testVersionCode = "45"
	testTeam        = "ABCDE12345"
	widgetRoot      = sampleRoot + "PlugIns/Widget.appex/"
)

var (
	testProfile   = []byte("profile for " + testIdentifier)
	widgetProfile = []byte("profile for " + testIdentifier + ".widget")
	sentinels     = []error{
		ErrArchive, ErrEntries, ErrSize, ErrBundle, ErrInfo, ErrExecutable, ErrUnsigned,
		ErrProfileMissing, ErrProfile, ErrIdentifier, ErrVersionName, ErrVersionCode, ErrTeam,
	}
)

func testSpec() Spec {
	return Spec{
		Identifier:  testIdentifier,
		VersionName: testVersionName,
		VersionCode: testVersionCode,
		TeamID:      testTeam,
		Profile:     testProfile,
	}
}

func testExpected() Expected {
	return Expected{
		Identifier:  testIdentifier,
		VersionName: testVersionName,
		VersionCode: testVersionCode,
		TeamID:      testTeam,
		Profiles: map[string][]byte{
			testIdentifier:             testProfile,
			testIdentifier + ".widget": widgetProfile,
		},
	}
}

func writeSample(t *testing.T, spec Spec) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.ipa")
	if err := Sample(path, spec); err != nil {
		t.Fatalf("sample: %v", err)
	}
	return path
}

type rawEntry struct {
	header zip.FileHeader
	data   []byte
}

// rewrite copies the IPA at source without the entries drop rejects, then appends entries.
func rewrite(t *testing.T, source string, drop func(string) bool, entries ...sampleEntry) string {
	t.Helper()
	raws := make([]rawEntry, 0, len(entries))
	for _, entry := range entries {
		header := zip.FileHeader{
			Name:               entry.name,
			Method:             zip.Store,
			CRC32:              crc32.ChecksumIEEE(entry.data),
			CompressedSize64:   uint64(len(entry.data)),
			UncompressedSize64: uint64(len(entry.data)),
		}
		raws = append(raws, rawEntry{header: header, data: entry.data})
	}
	return rewriteRaw(t, source, drop, raws...)
}

// rewriteRaw appends entries whose headers are written as given, so they may lie about their sizes.
func rewriteRaw(t *testing.T, source string, drop func(string) bool, entries ...rawEntry) string {
	t.Helper()
	reader, err := zip.OpenReader(source)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	path := filepath.Join(t.TempDir(), "rewritten.ipa")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := zip.NewWriter(file)
	for _, entry := range reader.File {
		if drop != nil && drop(entry.Name) {
			continue
		}
		if err := writer.Copy(entry); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range entries {
		target, err := writer.CreateRaw(&entry.header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := target.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func named(names ...string) func(string) bool {
	return func(name string) bool {
		for _, candidate := range names {
			if name == candidate {
				return true
			}
		}
		return false
	}
}

func assertErrors(t *testing.T, err error, expected ...error) {
	t.Helper()
	if err == nil {
		t.Fatalf("verified, want %v", expected)
	}
	for _, sentinel := range sentinels {
		wanted := false
		for _, candidate := range expected {
			wanted = wanted || candidate == sentinel
		}
		if errors.Is(err, sentinel) != wanted {
			t.Fatalf("errors.Is(%v, %v) = %v, want %v", err, sentinel, !wanted, wanted)
		}
	}
}

func TestSampleVerifies(t *testing.T) {
	cases := map[string]func(*Spec){
		"binary info":  func(*Spec) {},
		"xml info":     func(spec *Spec) { spec.XML = true },
		"universal":    func(spec *Spec) { spec.Universal = true },
		"entitlements": func(spec *Spec) { spec.Entitlements = map[string]any{"aps-environment": "production"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := testSpec()
			mutate(&spec)
			path := writeSample(t, spec)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)

			result, err := IPA(path, testExpected(), t.TempDir())
			if err != nil {
				t.Fatalf("verify: %v", err)
			}

			entitlements := spec.Entitlements
			if entitlements == nil {
				entitlements = map[string]any{
					"application-identifier":              testTeam + "." + testIdentifier,
					"com.apple.developer.team-identifier": testTeam,
					"get-task-allow":                      false,
				}
			}
			expected := Result{
				Digest:       hex.EncodeToString(digest[:]),
				Size:         int64(len(data)),
				Identifier:   testIdentifier,
				VersionName:  testVersionName,
				VersionCode:  testVersionCode,
				TeamID:       testTeam,
				Signed:       true,
				Entitlements: entitlements,
			}
			if !reflect.DeepEqual(result, expected) {
				t.Fatalf("result %#v\nwant %#v", result, expected)
			}
		})
	}
}

func TestSampleIsDeterministic(t *testing.T) {
	first, err := os.ReadFile(writeSample(t, testSpec()))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(writeSample(t, testSpec()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two samples of one spec differ")
	}
}

func TestSampleInfoIsBinaryByDefault(t *testing.T) {
	reader, err := zip.OpenReader(writeSample(t, testSpec()))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	file, err := reader.Open(sampleRoot + "Info.plist")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	header := make([]byte, len(binaryMagic))
	if _, err := file.Read(header); err != nil || !bytes.Equal(header, binaryMagic) {
		t.Fatalf("Info.plist starts %q, want a binary property list", header)
	}
}

func TestSampleFailsOnUnwritablePath(t *testing.T) {
	if err := Sample(filepath.Join(t.TempDir(), "missing", "app.ipa"), testSpec()); err == nil {
		t.Fatal("wrote into a missing directory")
	}
}

func TestMismatch(t *testing.T) {
	cases := []struct {
		name     string
		spec     func(*Spec)
		expected func(*Expected)
		errors   []error
		signed   bool
		check    func(*testing.T, Result)
	}{
		{
			name:   "wrong identifier",
			spec:   func(spec *Spec) { spec.Identifier = "io.example.other" },
			errors: []error{ErrIdentifier},
			signed: true,
			check: func(t *testing.T, result Result) {
				if result.Identifier != "io.example.other" {
					t.Fatalf("result identifier %q", result.Identifier)
				}
			},
		},
		{name: "wrong version name", spec: func(spec *Spec) { spec.VersionName = "1.2.4" }, errors: []error{ErrVersionName}, signed: true},
		{name: "wrong version code", spec: func(spec *Spec) { spec.VersionCode = "46" }, errors: []error{ErrVersionCode}, signed: true},
		{
			name:     "no project version to keep",
			spec:     func(spec *Spec) { spec.VersionName = "" },
			expected: func(expected *Expected) { expected.VersionName = "" },
			errors:   []error{ErrVersionName},
			signed:   true,
		},
		{
			name:     "project version too long to keep",
			spec:     func(spec *Spec) { spec.VersionName = strings.Repeat("1", VersionNameBytes+1) },
			expected: func(expected *Expected) { expected.VersionName = "" },
			errors:   []error{ErrVersionName},
			signed:   true,
		},
		{
			name:     "lease without a version name still checks the version code",
			spec:     func(spec *Spec) { spec.VersionCode = "46" },
			expected: func(expected *Expected) { expected.VersionName = "" },
			errors:   []error{ErrVersionCode},
			signed:   true,
		},
		{
			name:   "wrong team",
			spec:   func(spec *Spec) { spec.TeamID = "ZYXWV98765" },
			errors: []error{ErrTeam},
			signed: true,
			check: func(t *testing.T, result Result) {
				if result.TeamID != "ZYXWV98765" {
					t.Fatalf("result team %q", result.TeamID)
				}
			},
		},
		{name: "wrong profile", spec: func(spec *Spec) { spec.Profile = []byte("someone else's profile") }, errors: []error{ErrProfile}, signed: true},
		{name: "empty profile", spec: func(spec *Spec) { spec.Profile = []byte{} }, errors: []error{ErrProfile}, signed: true},
		{
			name:     "lease without a profile for the identifier",
			expected: func(expected *Expected) { delete(expected.Profiles, testIdentifier) },
			errors:   []error{ErrProfile},
			signed:   true,
		},
		{name: "missing profile", spec: func(spec *Spec) { spec.Profile = nil }, errors: []error{ErrProfileMissing}},
		{name: "ad hoc", spec: func(spec *Spec) { spec.AdHoc = true }, errors: []error{ErrUnsigned, ErrTeam}},
		{name: "universal ad hoc", spec: func(spec *Spec) { spec.AdHoc, spec.Universal = true, true }, errors: []error{ErrUnsigned, ErrTeam}},
		{name: "no code signature", spec: func(spec *Spec) { spec.Unsigned = true }, errors: []error{ErrUnsigned, ErrTeam}},
		{name: "no cms signature", spec: func(spec *Spec) { spec.EmptyCMS = true }, errors: []error{ErrUnsigned}},
		{name: "signed without a team", spec: func(spec *Spec) { spec.TeamID = "" }, errors: []error{ErrTeam}, signed: true},
		{
			name:   "every mismatch at once",
			spec:   func(spec *Spec) { *spec = Spec{Identifier: "a", VersionName: "b", VersionCode: "c", AdHoc: true} },
			errors: []error{ErrIdentifier, ErrVersionName, ErrVersionCode, ErrUnsigned, ErrTeam, ErrProfileMissing},
		},
		{name: "two apps", spec: func(spec *Spec) { spec.ExtraApp = true }, errors: []error{ErrBundle}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			spec, expected := testSpec(), testExpected()
			if test.spec != nil {
				test.spec(&spec)
			}
			if test.expected != nil {
				test.expected(&expected)
			}
			result, err := IPA(writeSample(t, spec), expected, t.TempDir())
			assertErrors(t, err, test.errors...)
			if result.Digest == "" || result.Size == 0 {
				t.Fatal("result has no digest or size")
			}
			if result.Signed != test.signed {
				t.Fatalf("signed %v, want %v", result.Signed, test.signed)
			}
			if test.check != nil {
				test.check(t, result)
			}
		})
	}
}

func TestKeepsTheProjectVersion(t *testing.T) {
	for _, name := range []string{"7.0", strings.Repeat("9", VersionNameBytes)} {
		t.Run(name, func(t *testing.T) {
			spec, expected := testSpec(), testExpected()
			spec.VersionName = name
			expected.VersionName = ""

			result, err := IPA(writeSample(t, spec), expected, t.TempDir())

			if err != nil {
				t.Fatalf("IPA: %v", err)
			}
			if result.VersionName != name || result.VersionCode != testVersionCode {
				t.Fatalf("result version %q (%q), want the ipa's %q (%q)", result.VersionName, result.VersionCode, name, testVersionCode)
			}
		})
	}
}

func TestUniversalSlicesMustAgree(t *testing.T) {
	other := testSpec()
	other.TeamID = "ZYXWV98765"
	adHoc := testSpec()
	adHoc.AdHoc = true
	entitlements, err := encodeXML(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		second Spec
		errors []error
	}{
		"second slice from another team": {second: other, errors: []error{ErrTeam}},
		"second slice ad hoc":            {second: adHoc, errors: []error{ErrUnsigned, ErrTeam}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			executable := universal([]machOImage{
				{target: arm64, data: machOSlice(arm64, testSpec(), sealed{entitlements: entitlements})},
				{target: amd64, data: machOSlice(amd64, test.second, sealed{entitlements: entitlements})},
			})
			path := rewrite(t, writeSample(t, testSpec()), named(sampleRoot+sampleName), sampleEntry{name: sampleRoot + sampleName, data: executable})
			_, err := IPA(path, testExpected(), t.TempDir())
			assertErrors(t, err, test.errors...)
		})
	}
}

func TestNestedProfiles(t *testing.T) {
	widgetInfo := func(identifier string) []byte {
		data, err := encodeBinary(map[string]any{"CFBundleIdentifier": identifier})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	cases := map[string]struct {
		profile []byte
		info    []byte
		errors  []error
	}{
		"profile from the lease":           {profile: widgetProfile, info: widgetInfo(testIdentifier + ".widget")},
		"profile not from the lease":       {profile: []byte("foreign"), info: widgetInfo(testIdentifier + ".widget"), errors: []error{ErrProfile}},
		"another bundle's profile":         {profile: testProfile, info: widgetInfo(testIdentifier + ".widget"), errors: []error{ErrProfile}},
		"bundle the lease has no profile":  {profile: widgetProfile, info: widgetInfo(testIdentifier + ".other"), errors: []error{ErrProfile}},
		"bundle without an Info.plist":     {profile: widgetProfile, errors: []error{ErrProfile}},
		"bundle without a bundle identity": {profile: widgetProfile, info: widgetInfo(""), errors: []error{ErrProfile}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			entries := []sampleEntry{{name: widgetRoot + "embedded.mobileprovision", data: test.profile}}
			if test.info != nil {
				entries = append(entries, sampleEntry{name: widgetRoot + "Info.plist", data: test.info})
			}
			path := rewrite(t, writeSample(t, testSpec()), nil, entries...)
			_, err := IPA(path, testExpected(), t.TempDir())
			if test.errors == nil {
				if err != nil {
					t.Fatalf("verify: %v", err)
				}
				return
			}
			assertErrors(t, err, test.errors...)
		})
	}
}

func TestStoredExecutableIsReadInPlace(t *testing.T) {
	source := writeSample(t, testSpec())
	reader, err := zip.OpenReader(source)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	path := filepath.Join(t.TempDir(), "stored.ipa")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, entry := range reader.File {
		header := entry.FileHeader
		header.Method = zip.Store
		target, err := writer.CreateHeader(&header)
		if err != nil {
			t.Fatal(err)
		}
		content, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := target.Write(mustRead(t, content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := IPA(path, testExpected(), t.TempDir()); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func mustRead(t *testing.T, reader interface{ Read([]byte) (int, error) }) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(reader); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func infoWith(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	info := map[string]any{
		"CFBundleIdentifier":         testIdentifier,
		"CFBundleShortVersionString": testVersionName,
		"CFBundleVersion":            testVersionCode,
		"CFBundleExecutable":         sampleName,
	}
	mutate(info)
	data, err := encodeBinary(info)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMalformedBundle(t *testing.T) {
	info := sampleRoot + "Info.plist"
	executable := sampleRoot + sampleName
	truncated := machOSlice(arm64, testSpec(), sealed{})
	truncated = truncated[:len(truncated)-40]
	badEntitlements := machOSlice(arm64, testSpec(), sealed{entitlements: []byte("not a plist")})
	cases := []struct {
		name    string
		drop    func(string) bool
		entries []sampleEntry
		errors  []error
	}{
		{name: "no app", drop: func(name string) bool { return strings.HasPrefix(name, sampleRoot) }, errors: []error{ErrBundle}},
		{name: "app outside payload", drop: func(name string) bool { return strings.HasPrefix(name, "Payload/") }, entries: []sampleEntry{{name: "App.app/Info.plist"}}, errors: []error{ErrBundle}},
		{name: "missing Info.plist", drop: named(info), errors: []error{ErrInfo}},
		{name: "garbage Info.plist", drop: named(info), entries: []sampleEntry{{name: info, data: []byte("garbage")}}, errors: []error{ErrInfo}},
		{name: "array Info.plist", drop: named(info), entries: []sampleEntry{{name: info, data: mustEncode(t, []any{"a"})}}, errors: []error{ErrInfo}},
		{
			name:    "missing CFBundleVersion",
			drop:    named(info),
			entries: []sampleEntry{{name: info, data: infoWith(t, func(info map[string]any) { delete(info, "CFBundleVersion") })}},
			errors:  []error{ErrInfo},
		},
		{
			name:    "integer CFBundleVersion",
			drop:    named(info),
			entries: []sampleEntry{{name: info, data: infoWith(t, func(info map[string]any) { info["CFBundleVersion"] = int64(45) })}},
			errors:  []error{ErrInfo},
		},
		{
			name:    "executable path escapes the bundle",
			drop:    named(info),
			entries: []sampleEntry{{name: info, data: infoWith(t, func(info map[string]any) { info["CFBundleExecutable"] = "../App" })}},
			errors:  []error{ErrInfo},
		},
		{name: "missing executable", drop: named(executable), errors: []error{ErrExecutable}},
		{name: "executable is not Mach-O", drop: named(executable), entries: []sampleEntry{{name: executable, data: []byte("#!/bin/sh\necho hi\n")}}, errors: []error{ErrExecutable}},
		{name: "code signature past the end", drop: named(executable), entries: []sampleEntry{{name: executable, data: truncated}}, errors: []error{ErrExecutable}},
		{name: "entitlements are not a plist", drop: named(executable), entries: []sampleEntry{{name: executable, data: badEntitlements}}, errors: []error{ErrExecutable}},
		{name: "no CodeResources", drop: named(sampleRoot + "_CodeSignature/CodeResources"), errors: []error{ErrUnsigned}},
		{name: "duplicate entry", entries: []sampleEntry{{name: info, data: infoWith(t, func(map[string]any) {})}}, errors: []error{ErrArchive}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := rewrite(t, writeSample(t, testSpec()), test.drop, test.entries...)
			_, err := IPA(path, testExpected(), t.TempDir())
			assertErrors(t, err, test.errors...)
		})
	}
}

func mustEncode(t *testing.T, value any) []byte {
	t.Helper()
	data, err := encodeBinary(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDamagedArchive(t *testing.T) {
	valid, err := os.ReadFile(writeSample(t, testSpec()))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(valid), int64(len(valid)))
	if err != nil {
		t.Fatal(err)
	}
	corrupted := bytes.Clone(valid)
	for _, entry := range reader.File {
		if entry.Name == sampleRoot+"Info.plist" {
			offset, err := entry.DataOffset()
			if err != nil {
				t.Fatal(err)
			}
			for index := range entry.CompressedSize64 {
				corrupted[offset+int64(index)] ^= 0x55
			}
		}
	}
	cases := map[string][]byte{
		"truncated to half":         valid[:len(valid)/2],
		"truncated by ten bytes":    valid[:len(valid)-10],
		"corrupted Info.plist data": corrupted,
		"not a zip":                 []byte("plain text, not an archive"),
		"empty file":                {},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "damaged.ipa")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := IPA(path, testExpected(), t.TempDir())
			assertErrors(t, err, ErrArchive)
			digest := sha256.Sum256(data)
			if result.Digest != hex.EncodeToString(digest[:]) || result.Size != int64(len(data)) {
				t.Fatal("a damaged archive still reports its digest and size")
			}
		})
	}
}

func TestEntryCeiling(t *testing.T) {
	path := writeSample(t, testSpec())
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	count := len(reader.File)
	reader.Close()

	ceiling := defaults
	ceiling.entries = count
	if _, err := verify(path, testExpected(), t.TempDir(), ceiling); err != nil {
		t.Fatalf("an IPA at the entry ceiling failed: %v", err)
	}
	ceiling.entries = count - 1
	_, err = verify(path, testExpected(), t.TempDir(), ceiling)
	assertErrors(t, err, ErrEntries)
}

func TestDefaultEntryCeiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "many.ipa")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for index := range defaults.entries + 1 {
		if _, err := writer.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("%s%d", sampleRoot, index), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = IPA(path, testExpected(), t.TempDir())
	assertErrors(t, err, ErrEntries)
}

func TestUncompressedSizeCeiling(t *testing.T) {
	source := writeSample(t, testSpec())
	declared := func(sizes ...uint64) string {
		entries := make([]rawEntry, 0, len(sizes))
		for index, size := range sizes {
			header := zip.FileHeader{Name: fmt.Sprintf("%sAssets%d", sampleRoot, index), Method: zip.Store, UncompressedSize64: size}
			entries = append(entries, rawEntry{header: header})
		}
		return rewriteRaw(t, source, nil, entries...)
	}
	for name, path := range map[string]string{
		"one entry over the ceiling": declared(defaults.size + 1),
		"entries summing past it":    declared(defaults.size/2, defaults.size/2),
		"sizes overflowing 64 bits":  declared(^uint64(0), ^uint64(0)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := IPA(path, testExpected(), t.TempDir())
			assertErrors(t, err, ErrSize)
		})
	}
}

func TestEntryCeilings(t *testing.T) {
	cases := map[string]struct {
		limit  func(*limits)
		errors []error
	}{
		"Info.plist":     {limit: func(ceiling *limits) { ceiling.plist = 64 }, errors: []error{ErrSize}},
		"profile":        {limit: func(ceiling *limits) { ceiling.profile = 8 }, errors: []error{ErrSize}},
		"executable":     {limit: func(ceiling *limits) { ceiling.executable = 64 }, errors: []error{ErrSize}},
		"code signature": {limit: func(ceiling *limits) { ceiling.signature = 64 }, errors: []error{ErrExecutable}},
	}
	path := writeSample(t, testSpec())
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			ceiling := defaults
			test.limit(&ceiling)
			_, err := verify(path, testExpected(), t.TempDir(), ceiling)
			assertErrors(t, err, test.errors...)
		})
	}
}

func TestEntryInflatingPastItsDeclaredSize(t *testing.T) {
	info := infoWith(t, func(info map[string]any) { info["Padding"] = strings.Repeat("p", 4096) })
	var deflated bytes.Buffer
	compressor, err := flate.NewWriter(&deflated, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compressor.Write(info); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	header := zip.FileHeader{
		Name:               sampleRoot + "Info.plist",
		Method:             zip.Deflate,
		CRC32:              crc32.ChecksumIEEE(info),
		CompressedSize64:   uint64(deflated.Len()),
		UncompressedSize64: 100,
	}
	path := rewriteRaw(t, writeSample(t, testSpec()), named(sampleRoot+"Info.plist"), rawEntry{header: header, data: deflated.Bytes()})

	_, err = IPA(path, testExpected(), t.TempDir())
	assertErrors(t, err, ErrArchive)
}

func TestRequiresExpectedValues(t *testing.T) {
	path := writeSample(t, testSpec())
	for _, clear := range []func(*Expected){
		func(expected *Expected) { expected.Identifier = "" },
		func(expected *Expected) { expected.VersionCode = "" },
		func(expected *Expected) { expected.TeamID = "" },
	} {
		expected := testExpected()
		clear(&expected)
		if _, err := IPA(path, expected, t.TempDir()); err == nil {
			t.Fatalf("verified against an incomplete lease %+v", expected)
		}
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := IPA(filepath.Join(t.TempDir(), "absent.ipa"), testExpected(), t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want fs.ErrNotExist", err)
	}
}

func TestCentralDirectoryBudget(t *testing.T) {
	entries := make([]sampleEntry, 200)
	for index := range entries {
		entries[index] = sampleEntry{name: fmt.Sprintf("Payload/App.app/Resources/resource-%03d.txt", index), data: []byte("x")}
	}
	path := rewrite(t, writeSample(t, testSpec()), nil, entries...)
	ceiling := defaults
	ceiling.directory = 8 << 10

	_, err := verify(path, testExpected(), t.TempDir(), ceiling)

	if !errors.Is(err, ErrEntries) {
		t.Fatalf("verify = %v, want the central directory budget to refuse it", err)
	}
	ceiling.directory = 1 << 20
	if _, err := verify(path, testExpected(), t.TempDir(), ceiling); err != nil {
		t.Fatalf("verify within the budget: %v", err)
	}
}

// TestCentralDirectoryBudgetIgnoresTheEndRecord rewrites the end record to declare one entry in a tiny directory:
// archive/zip still reads every header, so only the read budget stops it.
func TestCentralDirectoryBudgetIgnoresTheEndRecord(t *testing.T) {
	entries := make([]sampleEntry, 65536)
	for index := range entries {
		entries[index] = sampleEntry{name: fmt.Sprintf("Payload/App.app/r/%05d", index)}
	}
	path := rewrite(t, writeSample(t, testSpec()), nil, entries...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	end := bytes.LastIndex(data, []byte{'P', 'K', 5, 6})
	total := binary.LittleEndian.Uint16(data[end+10:])
	binary.LittleEndian.PutUint16(data[end+8:], total)
	binary.LittleEndian.PutUint32(data[end+12:], 64)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ceiling := defaults
	ceiling.directory = 1 << 20

	_, err = verify(path, testExpected(), t.TempDir(), ceiling)

	if !errors.Is(err, ErrEntries) {
		t.Fatalf("verify = %v, want the read budget to refuse a directory its end record understates", err)
	}
}

func TestUniversalBinaryArchitectureLimit(t *testing.T) {
	header := binary.BigEndian.AppendUint32(nil, fatMagic)
	header = binary.BigEndian.AppendUint32(header, 1_000_000)
	_, err := signatures(bytes.NewReader(header), int64(len(header)), 1<<20, 8)
	if err == nil || !strings.Contains(err.Error(), "limit 8") {
		t.Fatalf("signatures = %v, want the architecture limit", err)
	}
}

func TestEntitlementsLimit(t *testing.T) {
	spec := testSpec()
	spec.Entitlements = map[string]any{"padding": strings.Repeat("a", 2048)}
	ceiling := defaults
	ceiling.entitlements = 1024

	_, err := verify(writeSample(t, spec), testExpected(), t.TempDir(), ceiling)

	if !errors.Is(err, ErrSize) {
		t.Fatalf("verify = %v, want the entitlements limit", err)
	}
}

func TestExecutableSpoolsIntoTheGivenDirectory(t *testing.T) {
	spool := t.TempDir()
	directory := filepath.Join(spool, "missing")

	_, err := IPA(writeSample(t, testSpec()), testExpected(), directory)

	if err == nil || !strings.Contains(err.Error(), "spool executable") {
		t.Fatalf("IPA = %v, want the spool to use the given directory", err)
	}
}
