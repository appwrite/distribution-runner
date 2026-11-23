package verify

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/binary"
	"io/fs"
	"os"
	"time"
)

// Spec describes a synthetic IPA. With no option set, IPA accepts it for an Expected holding the same identifier,
// versions and team, and Profile as the profile for that identifier. Each option breaks it one way.
type Spec struct {
	Identifier  string
	VersionName string
	VersionCode string
	TeamID      string
	// Profile is written verbatim as embedded.mobileprovision; nil omits it.
	Profile []byte
	// Entitlements are signed into the executable; nil signs the application and team identifiers.
	Entitlements map[string]any
	// XML writes Info.plist as an XML property list instead of the binary one Xcode produces.
	XML bool
	// Universal writes an arm64 and x86_64 executable instead of a thin arm64 one.
	Universal bool
	// AdHoc flags every code directory ad hoc and names no team.
	AdHoc bool
	// Unsigned writes an executable without LC_CODE_SIGNATURE.
	Unsigned bool
	// ExtraApp adds a second Payload/*.app.
	ExtraApp bool
}

const (
	sampleName           = "App"
	sampleRoot           = "Payload/" + sampleName + ".app/"
	machHeaderSize       = 32
	codeSignatureSize    = 16
	codeDirectoryVersion = 0x20400
	codeDirectorySize    = 88
	universalAlignment   = 14
)

var sampleModified = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type architecture struct {
	cpu     uint32
	subtype uint32
}

type machOImage struct {
	target architecture
	data   []byte
}

var (
	arm64 = architecture{cpu: 0x0100000C}
	amd64 = architecture{cpu: 0x01000007, subtype: 3}
)

// Sample writes a synthetic IPA to path. Its bytes depend only on spec.
func Sample(path string, spec Spec) (err error) {
	entries, err := sampleEntries(spec)
	if err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(path)
		}
	}()
	writer := zip.NewWriter(file)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate, Modified: sampleModified}
		header.SetMode(entry.mode)
		if entry.mode.IsDir() {
			header.Method = zip.Store
		}
		target, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		if _, err := target.Write(entry.data); err != nil {
			return err
		}
	}
	return writer.Close()
}

type sampleEntry struct {
	name string
	mode fs.FileMode
	data []byte
}

func sampleEntries(spec Spec) ([]sampleEntry, error) {
	info := map[string]any{
		"CFBundleIdentifier":         spec.Identifier,
		"CFBundleShortVersionString": spec.VersionName,
		"CFBundleVersion":            spec.VersionCode,
		"CFBundleExecutable":         sampleName,
		"CFBundleName":               sampleName,
		"CFBundlePackageType":        "APPL",
		"CFBundleSupportedPlatforms": []any{"iPhoneOS"},
		"MinimumOSVersion":           "16.0",
	}
	encode := encodeBinary
	if spec.XML {
		encode = encodeXML
	}
	infoData, err := encode(info)
	if err != nil {
		return nil, err
	}
	entitlements := spec.Entitlements
	if entitlements == nil {
		entitlements = map[string]any{
			"application-identifier":              spec.TeamID + "." + spec.Identifier,
			"com.apple.developer.team-identifier": spec.TeamID,
			"get-task-allow":                      false,
		}
	}
	entitlementsData, err := encodeXML(entitlements)
	if err != nil {
		return nil, err
	}
	infoHash := sha256.Sum256(infoData)
	resources, err := encodeXML(map[string]any{
		"files":  map[string]any{},
		"files2": map[string]any{"Info.plist": map[string]any{"hash2": infoHash[:]}},
	})
	if err != nil {
		return nil, err
	}

	entries := []sampleEntry{
		{name: "Payload/", mode: fs.ModeDir | 0o755},
		{name: sampleRoot, mode: fs.ModeDir | 0o755},
		{name: sampleRoot + "_CodeSignature/", mode: fs.ModeDir | 0o755},
		{name: sampleRoot + "_CodeSignature/CodeResources", mode: 0o644, data: resources},
		{name: sampleRoot + "Info.plist", mode: 0o644, data: infoData},
		{name: sampleRoot + sampleName, mode: 0o755, data: sampleExecutable(spec, sealed{info: infoData, resources: resources, entitlements: entitlementsData})},
	}
	if spec.Profile != nil {
		entries = append(entries, sampleEntry{name: sampleRoot + "embedded.mobileprovision", mode: 0o644, data: spec.Profile})
	}
	if spec.ExtraApp {
		entries = append(entries,
			sampleEntry{name: "Payload/Extra.app/", mode: fs.ModeDir | 0o755},
			sampleEntry{name: "Payload/Extra.app/Info.plist", mode: 0o644, data: infoData},
		)
	}
	return entries, nil
}

// sealed is what a code signature binds besides the code: the special slots hash Info.plist, CodeResources and the
// entitlements blob.
type sealed struct {
	info         []byte
	resources    []byte
	entitlements []byte
}

func sampleExecutable(spec Spec, seal sealed) []byte {
	if !spec.Universal {
		return machOSlice(arm64, spec, seal)
	}
	return universal([]machOImage{
		{target: arm64, data: machOSlice(arm64, spec, seal)},
		{target: amd64, data: machOSlice(amd64, spec, seal)},
	})
}

// machOSlice writes a 64-bit MH_EXECUTE header and, unless spec.Unsigned, one LC_CODE_SIGNATURE whose superblob
// holds a code directory hashing the header and the sealed files, the entitlements and an empty CMS wrapper.
func machOSlice(target architecture, spec Spec, seal sealed) []byte {
	commands, commandsSize := uint32(0), uint32(0)
	if !spec.Unsigned {
		commands, commandsSize = 1, codeSignatureSize
	}
	codeLimit := align(machHeaderSize+commandsSize, 16)
	image := make([]byte, codeLimit)
	order := binary.LittleEndian
	order.PutUint32(image[0:], 0xFEEDFACF)
	order.PutUint32(image[4:], target.cpu)
	order.PutUint32(image[8:], target.subtype)
	order.PutUint32(image[12:], 2)
	order.PutUint32(image[16:], commands)
	order.PutUint32(image[20:], commandsSize)
	if spec.Unsigned {
		return image
	}
	order.PutUint32(image[32:], codeSignatureCommand)
	order.PutUint32(image[36:], codeSignatureSize)
	order.PutUint32(image[40:], codeLimit)
	size := len(sampleSuperBlob(spec, image, seal))
	order.PutUint32(image[44:], uint32(size))
	return append(image, sampleSuperBlob(spec, image, seal)...)
}

func sampleSuperBlob(spec Spec, code []byte, seal sealed) []byte {
	entitlements := blob(entitlementsMagic, seal.entitlements)
	requirements := binary.BigEndian.AppendUint32(blob(requirementsMagic, nil), 0)
	binary.BigEndian.PutUint32(requirements[4:], uint32(len(requirements)))
	special := [][]byte{seal.info, requirements, seal.resources, nil, entitlements}
	blobs := []struct {
		slot uint32
		data []byte
	}{
		{slot: 0, data: sampleCodeDirectory(spec, code, special)},
		{slot: 2, data: requirements},
		{slot: 5, data: entitlements},
		{slot: 0x10000, data: blob(signatureWrapperMagic, nil)},
	}
	header := uint32(12 + 8*len(blobs))
	total := header
	for _, current := range blobs {
		total += uint32(len(current.data))
	}
	data := make([]byte, header, total)
	binary.BigEndian.PutUint32(data[0:], superBlobMagic)
	binary.BigEndian.PutUint32(data[4:], total)
	binary.BigEndian.PutUint32(data[8:], uint32(len(blobs)))
	offset := header
	for index, current := range blobs {
		binary.BigEndian.PutUint32(data[12+index*8:], current.slot)
		binary.BigEndian.PutUint32(data[16+index*8:], offset)
		data = append(data, current.data...)
		offset += uint32(len(current.data))
	}
	return data
}

// sampleCodeDirectory hashes code into one page slot and each special entry into slot -(index+1), a nil entry
// leaving its slot zero.
func sampleCodeDirectory(spec Spec, code []byte, special [][]byte) []byte {
	identifier := append([]byte(spec.Identifier), 0)
	var team []byte
	if !spec.AdHoc && spec.TeamID != "" {
		team = append([]byte(spec.TeamID), 0)
	}
	identifierOffset := uint32(codeDirectorySize)
	teamOffset := uint32(0)
	if team != nil {
		teamOffset = identifierOffset + uint32(len(identifier))
	}
	hashOffset := identifierOffset + uint32(len(identifier)+len(team)+len(special)*sha256.Size)
	hash := sha256.Sum256(code)
	data := make([]byte, hashOffset, hashOffset+uint32(len(hash)))
	order := binary.BigEndian
	order.PutUint32(data[0:], codeDirectoryMagic)
	order.PutUint32(data[4:], hashOffset+uint32(len(hash)))
	order.PutUint32(data[8:], codeDirectoryVersion)
	if spec.AdHoc {
		order.PutUint32(data[12:], adHocFlag)
	}
	order.PutUint32(data[16:], hashOffset)
	order.PutUint32(data[20:], identifierOffset)
	order.PutUint32(data[24:], uint32(len(special)))
	order.PutUint32(data[28:], 1)
	order.PutUint32(data[32:], uint32(len(code)))
	data[36] = sha256.Size
	data[37] = 2
	data[39] = 12
	order.PutUint32(data[teamOffsetPosition:], teamOffset)
	order.PutUint64(data[80:], 1)
	copy(data[identifierOffset:], identifier)
	copy(data[teamOffset:], team)
	for index, content := range special {
		if content != nil {
			digest := sha256.Sum256(content)
			copy(data[hashOffset-uint32(index+1)*sha256.Size:], digest[:])
		}
	}
	return append(data, hash[:]...)
}

func blob(magic uint32, payload []byte) []byte {
	data := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(data[0:], magic)
	binary.BigEndian.PutUint32(data[4:], uint32(8+len(payload)))
	return append(data, payload...)
}

func universal(images []machOImage) []byte {
	data := make([]byte, 8+20*len(images))
	binary.BigEndian.PutUint32(data[0:], 0xCAFEBABE)
	binary.BigEndian.PutUint32(data[4:], uint32(len(images)))
	for index, image := range images {
		offset := align(uint32(len(data)), 1<<universalAlignment)
		entry := data[8+20*index:]
		binary.BigEndian.PutUint32(entry[0:], image.target.cpu)
		binary.BigEndian.PutUint32(entry[4:], image.target.subtype)
		binary.BigEndian.PutUint32(entry[8:], offset)
		binary.BigEndian.PutUint32(entry[12:], uint32(len(image.data)))
		binary.BigEndian.PutUint32(entry[16:], universalAlignment)
		data = append(data, make([]byte, offset-uint32(len(data)))...)
		data = append(data, image.data...)
	}
	return data
}

func align(value uint32, boundary uint32) uint32 {
	return (value + boundary - 1) / boundary * boundary
}
