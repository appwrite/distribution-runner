// Package verify checks an IPA a build VM produced against the lease it was built for, reading the archive itself
// and never the VM's own result.json.
package verify

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

var (
	ErrArchive        = errors.New("verify: ipa is not a readable zip archive")
	ErrEntries        = errors.New("verify: ipa has too many entries")
	ErrSize           = errors.New("verify: ipa exceeds a size ceiling")
	ErrBundle         = errors.New("verify: ipa must hold exactly one Payload/*.app")
	ErrInfo           = errors.New("verify: Info.plist is missing or invalid")
	ErrExecutable     = errors.New("verify: executable is missing or not a valid Mach-O file")
	ErrUnsigned       = errors.New("verify: ipa is not signed for distribution")
	ErrProfileMissing = errors.New("verify: ipa has no embedded provisioning profile")
	ErrProfile        = errors.New("verify: provisioning profile is not the lease's")
	ErrIdentifier     = errors.New("verify: bundle identifier differs from the lease")
	ErrVersionName    = errors.New("verify: version name differs from the lease")
	ErrVersionCode    = errors.New("verify: version code differs from the lease")
	ErrTeam           = errors.New("verify: signing team differs from the lease")
)

// Expected holds the lease values an IPA must match. Profiles maps bundle identifiers to provisioning profile bytes.
type Expected struct {
	Identifier  string
	VersionName string
	VersionCode string
	TeamID      string
	Profiles    map[string][]byte
}

// Result is what the agent read from the IPA. Digest is the lowercase hex SHA-256 of the file. Signed means every
// slice carries a code signature that is not ad hoc, the bundle is sealed and it embeds a profile, as RFC §14 defines it.
type Result struct {
	Digest       string         `json:"digest"`
	Size         int64          `json:"size"`
	Identifier   string         `json:"identifier"`
	VersionName  string         `json:"versionName"`
	VersionCode  string         `json:"versionCode"`
	TeamID       string         `json:"teamId"`
	Signed       bool           `json:"signed"`
	Entitlements map[string]any `json:"entitlements"`
}

type limits struct {
	entries    int
	size       uint64
	plist      int64
	profile    int64
	executable int64
	signature  int64
}

var defaults = limits{
	entries:    100_000,
	size:       16_000_000_000,
	plist:      1 << 20,
	profile:    1 << 20,
	executable: 1 << 30,
	signature:  16 << 20,
}

// IPA verifies the IPA at path against the lease. On a mismatch the error matches every failed check through
// errors.Is and the result still carries the digest and whatever the IPA declared, for reporting.
func IPA(path string, expected Expected) (Result, error) {
	return verify(path, expected, defaults)
}

func verify(path string, expected Expected, limits limits) (Result, error) {
	if expected.Identifier == "" || expected.VersionName == "" || expected.VersionCode == "" || expected.TeamID == "" {
		return Result{}, errors.New("verify: expected identifier, version name, version code and team id are required")
	}
	file, err := os.Open(path)
	if err != nil {
		return Result{}, fmt.Errorf("verify: open ipa: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return Result{}, fmt.Errorf("verify: read ipa: %w", err)
	}
	result := Result{Digest: hex.EncodeToString(hash.Sum(nil)), Size: size}

	ipa, err := open(file, size, limits)
	if err != nil {
		return result, err
	}
	info, err := ipa.info()
	if err != nil {
		return result, err
	}
	result.Identifier = info.identifier
	result.VersionName = info.versionName
	result.VersionCode = info.versionCode

	slices, err := ipa.signatures(info.executable)
	if err != nil {
		return result, err
	}
	result.TeamID = slices[0].team
	if slices[0].entitlements != nil {
		if result.Entitlements, err = decodeDictionary(slices[0].entitlements); err != nil {
			return result, fmt.Errorf("%w: entitlements: %w", ErrExecutable, err)
		}
	}
	profile, err := ipa.read(ipa.root+"embedded.mobileprovision", limits.profile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return result, err
	}
	embedded := err == nil
	reason := ipa.unsigned(slices)

	var failures []error
	if info.identifier != expected.Identifier {
		failures = append(failures, fmt.Errorf("%w: ipa has %q, lease has %q", ErrIdentifier, info.identifier, expected.Identifier))
	}
	if info.versionName != expected.VersionName {
		failures = append(failures, fmt.Errorf("%w: ipa has %q, lease has %q", ErrVersionName, info.versionName, expected.VersionName))
	}
	if info.versionCode != expected.VersionCode {
		failures = append(failures, fmt.Errorf("%w: ipa has %q, lease has %q", ErrVersionCode, info.versionCode, expected.VersionCode))
	}
	if reason != "" {
		failures = append(failures, fmt.Errorf("%w: %s", ErrUnsigned, reason))
	}
	for _, slice := range slices {
		if slice.team != expected.TeamID {
			failures = append(failures, fmt.Errorf("%w: ipa is signed by %q, lease has %q", ErrTeam, slice.team, expected.TeamID))
			break
		}
	}
	if !embedded {
		failures = append(failures, ErrProfileMissing)
	} else if leased, ok := expected.Profiles[expected.Identifier]; !ok {
		failures = append(failures, fmt.Errorf("%w: lease has no profile for %q", ErrProfile, expected.Identifier))
	} else if !bytes.Equal(profile, leased) {
		failures = append(failures, fmt.Errorf("%w: embedded.mobileprovision differs from the profile for %q", ErrProfile, expected.Identifier))
	}
	nested, err := ipa.nestedProfiles(expected.Profiles)
	if err != nil {
		return result, err
	}
	failures = append(failures, nested...)

	result.Signed = embedded && reason == ""
	return result, errors.Join(failures...)
}

type archive struct {
	reader  io.ReaderAt
	size    int64
	files   []*zip.File
	entries map[string]*zip.File
	root    string
	limits  limits
}

func open(reader io.ReaderAt, size int64, limits limits) (*archive, error) {
	zipped, err := zip.NewReader(reader, size)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrArchive, err)
	}
	if len(zipped.File) > limits.entries {
		return nil, fmt.Errorf("%w: %d entries, limit %d", ErrEntries, len(zipped.File), limits.entries)
	}
	entries := make(map[string]*zip.File, len(zipped.File))
	applications := map[string]struct{}{}
	var total uint64
	for _, entry := range zipped.File {
		if _, duplicate := entries[entry.Name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate entry %q", ErrArchive, entry.Name)
		}
		entries[entry.Name] = entry
		if entry.UncompressedSize64 > limits.size-total {
			return nil, fmt.Errorf("%w: uncompressed size exceeds %d bytes", ErrSize, limits.size)
		}
		total += entry.UncompressedSize64
		if application, ok := application(entry.Name); ok {
			applications[application] = struct{}{}
		}
	}
	if len(applications) != 1 {
		return nil, fmt.Errorf("%w: found %d", ErrBundle, len(applications))
	}
	var root string
	for application := range applications {
		root = "Payload/" + application + "/"
	}
	return &archive{reader: reader, size: size, files: zipped.File, entries: entries, root: root, limits: limits}, nil
}

func application(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "Payload/")
	if !ok {
		return "", false
	}
	application, _, nested := strings.Cut(rest, "/")
	if !nested || len(application) <= len(".app") || !strings.HasSuffix(application, ".app") {
		return "", false
	}
	return application, true
}

// read returns an entry's bytes, refusing it when it declares or inflates past limit. A missing entry is fs.ErrNotExist.
func (a *archive) read(name string, limit int64) ([]byte, error) {
	entry, ok := a.entries[name]
	if !ok {
		return nil, fmt.Errorf("verify: %s: %w", name, fs.ErrNotExist)
	}
	if entry.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("%w: %s declares %d bytes, limit %d", ErrSize, name, entry.UncompressedSize64, limit)
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrArchive, name, err)
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrArchive, name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %s inflates past %d bytes", ErrSize, name, limit)
	}
	return data, nil
}

type bundleInfo struct {
	identifier  string
	versionName string
	versionCode string
	executable  string
}

func (a *archive) info() (bundleInfo, error) {
	data, err := a.read(a.root+"Info.plist", a.limits.plist)
	if errors.Is(err, fs.ErrNotExist) {
		return bundleInfo{}, fmt.Errorf("%w: %s has no Info.plist", ErrInfo, a.root)
	}
	if err != nil {
		return bundleInfo{}, err
	}
	dictionary, err := decodeDictionary(data)
	if err != nil {
		return bundleInfo{}, fmt.Errorf("%w: %w", ErrInfo, err)
	}
	var info bundleInfo
	for key, target := range map[string]*string{
		"CFBundleIdentifier":         &info.identifier,
		"CFBundleShortVersionString": &info.versionName,
		"CFBundleVersion":            &info.versionCode,
		"CFBundleExecutable":         &info.executable,
	} {
		value, ok := dictionary[key].(string)
		if !ok {
			return bundleInfo{}, fmt.Errorf("%w: %s is missing or not a string", ErrInfo, key)
		}
		*target = value
	}
	if info.executable == "" || info.executable == "." || info.executable == ".." || strings.Contains(info.executable, "/") {
		return bundleInfo{}, fmt.Errorf("%w: CFBundleExecutable %q is not a file name", ErrInfo, info.executable)
	}
	return info, nil
}

func (a *archive) signatures(executable string) ([]signature, error) {
	reader, size, release, err := a.executable(a.root + executable)
	if err != nil {
		return nil, err
	}
	defer release()
	found, err := signatures(reader, size, a.limits.signature)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrExecutable, err)
	}
	return found, nil
}

// executable exposes an entry as a ReaderAt for debug/macho: a stored entry is read in place, a compressed one is
// inflated into a temporary file so a large binary never sits in memory.
func (a *archive) executable(name string) (io.ReaderAt, int64, func(), error) {
	entry, ok := a.entries[name]
	if !ok {
		return nil, 0, nil, fmt.Errorf("%w: %s is not in the ipa", ErrExecutable, name)
	}
	if entry.UncompressedSize64 > uint64(a.limits.executable) {
		return nil, 0, nil, fmt.Errorf("%w: %s declares %d bytes, limit %d", ErrSize, name, entry.UncompressedSize64, a.limits.executable)
	}
	size := int64(entry.UncompressedSize64)
	if entry.Method == zip.Store && entry.CompressedSize64 == entry.UncompressedSize64 {
		if offset, err := entry.DataOffset(); err == nil && offset >= 0 && offset <= a.size-size {
			return io.NewSectionReader(a.reader, offset, size), size, func() {}, nil
		}
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("%w: %s: %w", ErrArchive, name, err)
	}
	defer reader.Close()
	temporary, err := os.CreateTemp("", "verify-executable-*")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("verify: spool executable: %w", err)
	}
	release := func() {
		temporary.Close()
		os.Remove(temporary.Name())
	}
	written, err := io.Copy(temporary, archiveReader{io.LimitReader(reader, a.limits.executable+1)})
	if err != nil {
		release()
		if errors.Is(err, ErrArchive) {
			return nil, 0, nil, fmt.Errorf("%s: %w", name, err)
		}
		return nil, 0, nil, fmt.Errorf("verify: spool executable: %w", err)
	}
	if written > a.limits.executable {
		release()
		return nil, 0, nil, fmt.Errorf("%w: %s inflates past %d bytes", ErrSize, name, a.limits.executable)
	}
	return temporary, written, release, nil
}

// archiveReader marks read failures as archive damage, telling them apart from failures writing the spool file.
type archiveReader struct {
	reader io.Reader
}

func (r archiveReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	if err != nil && err != io.EOF {
		err = fmt.Errorf("%w: %w", ErrArchive, err)
	}
	return count, err
}

func (a *archive) unsigned(slices []signature) string {
	for _, slice := range slices {
		if !slice.present {
			return "executable has no code signature"
		}
		if slice.adHoc {
			return "executable is ad hoc signed"
		}
	}
	if _, ok := a.entries[a.root+"_CodeSignature/CodeResources"]; !ok {
		return "bundle has no _CodeSignature/CodeResources"
	}
	return ""
}

// nestedProfiles checks that every extension's or watch app's embedded profile is one the lease provided.
func (a *archive) nestedProfiles(profiles map[string][]byte) ([]error, error) {
	var failures []error
	for _, entry := range a.files {
		name := entry.Name
		if !strings.HasPrefix(name, a.root) || name == a.root+"embedded.mobileprovision" || !strings.HasSuffix(name, "/embedded.mobileprovision") {
			continue
		}
		data, err := a.read(name, a.limits.profile)
		if err != nil {
			return nil, err
		}
		leased := false
		for _, profile := range profiles {
			if bytes.Equal(data, profile) {
				leased = true
				break
			}
		}
		if !leased {
			failures = append(failures, fmt.Errorf("%w: %s is not one of the lease's profiles", ErrProfile, name))
		}
	}
	return failures, nil
}
