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

// Rejected reports whether err is the IPA failing verification, rather than the host failing to read it. Only a
// rejection describes the IPA; any other error may carry host paths.
func Rejected(err error) bool {
	for _, sentinel := range []error{
		ErrArchive, ErrEntries, ErrSize, ErrBundle, ErrInfo, ErrExecutable, ErrUnsigned, ErrProfileMissing, ErrProfile,
		ErrIdentifier, ErrVersionName, ErrVersionCode, ErrTeam,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

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
	entries       int
	directory     int64
	size          uint64
	plist         int64
	profile       int64
	executable    int64
	signature     int64
	entitlements  int
	architectures uint32
}

// defaults bound what a hostile IPA can make the verifier read or allocate. directory bounds the bytes
// archive/zip may read to find the end record and index the central directory, which it parses until a header fails
// to decode whatever the end record declares; entitlements is the event size the result travels in.
var defaults = limits{
	entries:       100_000,
	directory:     16 << 20,
	size:          16_000_000_000,
	plist:         1 << 20,
	profile:       1 << 20,
	executable:    1 << 30,
	signature:     16 << 20,
	entitlements:  1 << 20,
	architectures: 8,
}

// IPA verifies the IPA at path against the lease, inflating a compressed executable into a temporary file under
// spool (the system temporary directory when empty). On a mismatch the error matches every failed check through
// errors.Is and the result still carries the digest and whatever the IPA declared, for reporting.
func IPA(path string, expected Expected, spool string) (Result, error) {
	return verify(path, expected, spool, defaults)
}

func verify(path string, expected Expected, spool string, limits limits) (Result, error) {
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

	ipa, err := open(file, size, spool, limits)
	if err != nil {
		return result, err
	}
	bundle, err := ipa.info()
	if err != nil {
		return result, err
	}
	result.Identifier = bundle.identifier
	result.VersionName = bundle.versionName
	result.VersionCode = bundle.versionCode

	slices, err := ipa.signatures(bundle.executable)
	if err != nil {
		return result, err
	}
	result.TeamID = slices[0].team
	if length := len(slices[0].entitlements); length > limits.entitlements {
		return result, fmt.Errorf("%w: entitlements of %d bytes, limit %d", ErrSize, length, limits.entitlements)
	}
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
	if bundle.identifier != expected.Identifier {
		failures = append(failures, fmt.Errorf("%w: ipa has %q, lease has %q", ErrIdentifier, bundle.identifier, expected.Identifier))
	}
	if bundle.versionName != expected.VersionName {
		failures = append(failures, fmt.Errorf("%w: ipa has %q, lease has %q", ErrVersionName, bundle.versionName, expected.VersionName))
	}
	if bundle.versionCode != expected.VersionCode {
		failures = append(failures, fmt.Errorf("%w: ipa has %q, lease has %q", ErrVersionCode, bundle.versionCode, expected.VersionCode))
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
	spool   string
	limits  limits
}

func open(reader io.ReaderAt, size int64, spool string, limits limits) (*archive, error) {
	budget := &budgetReader{reader: reader, remaining: limits.directory}
	zipped, err := zip.NewReader(budget, size)
	budget.lift()
	if errors.Is(err, errDirectory) {
		return nil, fmt.Errorf("%w: the central directory exceeds %d bytes", ErrEntries, limits.directory)
	}
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
	return &archive{reader: reader, size: size, files: zipped.File, entries: entries, root: root, spool: spool, limits: limits}, nil
}

var errDirectory = errors.New("verify: central directory read budget exhausted")

// budgetReader fails every read past its budget until lifted. It is not safe for concurrent use.
type budgetReader struct {
	reader    io.ReaderAt
	remaining int64
	lifted    bool
}

func (b *budgetReader) ReadAt(buffer []byte, offset int64) (int, error) {
	if !b.lifted {
		if int64(len(buffer)) > b.remaining {
			return 0, errDirectory
		}
		b.remaining -= int64(len(buffer))
	}
	return b.reader.ReadAt(buffer, offset)
}

func (b *budgetReader) lift() {
	b.lifted = true
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
	dictionary, err := a.plist(a.root)
	if err != nil {
		return bundleInfo{}, err
	}
	var bundle bundleInfo
	for key, target := range map[string]*string{
		"CFBundleIdentifier":         &bundle.identifier,
		"CFBundleShortVersionString": &bundle.versionName,
		"CFBundleVersion":            &bundle.versionCode,
		"CFBundleExecutable":         &bundle.executable,
	} {
		value, ok := dictionary[key].(string)
		if !ok {
			return bundleInfo{}, fmt.Errorf("%w: %s is missing or not a string", ErrInfo, key)
		}
		*target = value
	}
	if bundle.executable == "" || bundle.executable == "." || bundle.executable == ".." || strings.Contains(bundle.executable, "/") {
		return bundleInfo{}, fmt.Errorf("%w: CFBundleExecutable %q is not a file name", ErrInfo, bundle.executable)
	}
	return bundle, nil
}

// plist decodes the Info.plist of the bundle at directory, a path inside the archive ending in "/".
func (a *archive) plist(directory string) (map[string]any, error) {
	data, err := a.read(directory+"Info.plist", a.limits.plist)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s has no Info.plist", ErrInfo, directory)
	}
	if err != nil {
		return nil, err
	}
	dictionary, err := decodeDictionary(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInfo, directory, err)
	}
	return dictionary, nil
}

func (a *archive) signatures(executable string) ([]signature, error) {
	reader, size, release, err := a.executable(a.root + executable)
	if err != nil {
		return nil, err
	}
	defer release()
	found, err := signatures(reader, size, a.limits.signature, a.limits.architectures)
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
	temporary, err := os.CreateTemp(a.spool, "verify-executable-*")
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
		if !slice.cms {
			return "signature is not ad hoc but carries no CMS signature"
		}
	}
	if _, ok := a.entries[a.root+"_CodeSignature/CodeResources"]; !ok {
		return "bundle has no _CodeSignature/CodeResources"
	}
	return ""
}

// nestedProfiles checks that every extension's or watch app's embedded profile is the one the lease provided for
// that bundle's own identifier.
func (a *archive) nestedProfiles(profiles map[string][]byte) ([]error, error) {
	var failures []error
	for _, entry := range a.files {
		name := entry.Name
		directory, nested := strings.CutSuffix(name, "embedded.mobileprovision")
		if !nested || !strings.HasPrefix(name, a.root) || directory == a.root || !strings.HasSuffix(directory, "/") {
			continue
		}
		data, err := a.read(name, a.limits.profile)
		if err != nil {
			return nil, err
		}
		dictionary, err := a.plist(directory)
		if err != nil {
			failures = append(failures, fmt.Errorf("%w: %s: %v", ErrProfile, name, err))
			continue
		}
		identifier, _ := dictionary["CFBundleIdentifier"].(string)
		leased, ok := profiles[identifier]
		switch {
		case identifier == "":
			failures = append(failures, fmt.Errorf("%w: %sInfo.plist has no CFBundleIdentifier", ErrProfile, directory))
		case !ok:
			failures = append(failures, fmt.Errorf("%w: lease has no profile for %q, which %s embeds", ErrProfile, identifier, directory))
		case !bytes.Equal(data, leased):
			failures = append(failures, fmt.Errorf("%w: %s differs from the profile for %q", ErrProfile, name, identifier))
		}
	}
	return failures, nil
}
