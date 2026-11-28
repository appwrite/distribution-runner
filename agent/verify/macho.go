package verify

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	codeSignatureCommand  = 0x1D
	superBlobMagic        = 0xFADE0CC0
	codeDirectoryMagic    = 0xFADE0C02
	entitlementsMagic     = 0xFADE7171
	requirementsMagic     = 0xFADE0C01
	signatureWrapperMagic = 0xFADE0B01
	signatureWrapperSlot  = 0x10000
	fatMagic              = 0xCAFEBABE
	adHocFlag             = 0x2
	teamVersion           = 0x20200
	codeDirectoryHeader   = 44
	teamOffsetPosition    = 48
)

// signature is what one Mach-O slice's embedded code signature declares. cms is a non-empty CMS signature blob,
// which every signature but an ad hoc one carries.
type signature struct {
	present      bool
	adHoc        bool
	cms          bool
	team         string
	entitlements []byte
}

// signatures reads the code signature of every slice of a thin or universal Mach-O executable, refusing a universal
// binary of more than architectures slices before debug/macho parses each one.
func signatures(reader io.ReaderAt, size int64, limit int64, architectures uint32) ([]signature, error) {
	header := make([]byte, 8)
	if _, err := reader.ReadAt(header, 0); err == nil && binary.BigEndian.Uint32(header) == fatMagic {
		if count := binary.BigEndian.Uint32(header[4:]); count > architectures {
			return nil, fmt.Errorf("universal binary declares %d slices, limit %d", count, architectures)
		}
	}
	fat, err := macho.NewFatFile(reader)
	if errors.Is(err, macho.ErrNotFat) {
		file, err := macho.NewFile(io.NewSectionReader(reader, 0, size))
		if err != nil {
			return nil, err
		}
		found, err := sliceSignature(file, reader, size, limit)
		if err != nil {
			return nil, err
		}
		return []signature{found}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(fat.Arches) == 0 {
		return nil, errors.New("universal binary has no slices")
	}
	found := make([]signature, 0, len(fat.Arches))
	for _, arch := range fat.Arches {
		if uint64(arch.Offset)+uint64(arch.Size) > uint64(size) {
			return nil, fmt.Errorf("slice %v runs past the end of the file", arch.Cpu)
		}
		slice := io.NewSectionReader(reader, int64(arch.Offset), int64(arch.Size))
		current, err := sliceSignature(arch.File, slice, int64(arch.Size), limit)
		if err != nil {
			return nil, fmt.Errorf("slice %v: %w", arch.Cpu, err)
		}
		found = append(found, current)
	}
	return found, nil
}

func sliceSignature(file *macho.File, slice io.ReaderAt, size int64, limit int64) (signature, error) {
	var command []byte
	for _, load := range file.Loads {
		raw := load.Raw()
		if len(raw) < 8 || file.ByteOrder.Uint32(raw) != codeSignatureCommand {
			continue
		}
		if command != nil {
			return signature{}, errors.New("more than one code signature command")
		}
		command = raw
	}
	if command == nil {
		return signature{}, nil
	}
	if len(command) < 16 {
		return signature{}, errors.New("code signature command is truncated")
	}
	offset := uint64(file.ByteOrder.Uint32(command[8:]))
	length := uint64(file.ByteOrder.Uint32(command[12:]))
	if length > uint64(limit) {
		return signature{}, fmt.Errorf("code signature of %d bytes exceeds %d", length, limit)
	}
	if offset+length > uint64(size) {
		return signature{}, errors.New("code signature runs past the end of the slice")
	}
	blob := make([]byte, length)
	if _, err := slice.ReadAt(blob, int64(offset)); err != nil {
		return signature{}, fmt.Errorf("read code signature: %w", err)
	}
	return superBlob(blob)
}

func superBlob(data []byte) (signature, error) {
	if len(data) < 12 || binary.BigEndian.Uint32(data) != superBlobMagic {
		return signature{}, errors.New("code signature is not a superblob")
	}
	length := uint64(binary.BigEndian.Uint32(data[4:]))
	if length < 12 || length > uint64(len(data)) {
		return signature{}, errors.New("superblob length does not fit the code signature")
	}
	data = data[:length]
	count := uint64(binary.BigEndian.Uint32(data[8:]))
	if count > (length-12)/8 {
		return signature{}, errors.New("superblob index does not fit")
	}
	found := signature{present: true}
	directories := 0
	for index := range count {
		offset := uint64(binary.BigEndian.Uint32(data[12+index*8+4:]))
		if offset+8 > length {
			return signature{}, errors.New("superblob entry lies outside the superblob")
		}
		size := uint64(binary.BigEndian.Uint32(data[offset+4:]))
		if size < 8 || size > length-offset {
			return signature{}, errors.New("superblob entry length does not fit")
		}
		slot := binary.BigEndian.Uint32(data[12+index*8:])
		blob := data[offset : offset+size]
		switch binary.BigEndian.Uint32(blob) {
		case codeDirectoryMagic:
			team, adHoc, err := codeDirectory(blob)
			if err != nil {
				return signature{}, err
			}
			if directories > 0 && team != found.team {
				return signature{}, errors.New("code directories name different teams")
			}
			found.team = team
			found.adHoc = found.adHoc || adHoc
			directories++
		case entitlementsMagic:
			if found.entitlements != nil {
				return signature{}, errors.New("more than one entitlements blob")
			}
			found.entitlements = blob[8:]
		case signatureWrapperMagic:
			found.cms = found.cms || (slot == signatureWrapperSlot && size > 8)
		}
	}
	if directories == 0 {
		return signature{}, errors.New("code signature has no code directory")
	}
	return found, nil
}

func codeDirectory(blob []byte) (string, bool, error) {
	if len(blob) < codeDirectoryHeader {
		return "", false, errors.New("code directory is truncated")
	}
	version := binary.BigEndian.Uint32(blob[8:])
	adHoc := binary.BigEndian.Uint32(blob[12:])&adHocFlag != 0
	if version < teamVersion {
		return "", adHoc, nil
	}
	if len(blob) < teamOffsetPosition+4 {
		return "", false, errors.New("code directory is too short for its version")
	}
	offset := uint64(binary.BigEndian.Uint32(blob[teamOffsetPosition:]))
	if offset == 0 {
		return "", adHoc, nil
	}
	if offset >= uint64(len(blob)) {
		return "", false, errors.New("team identifier lies outside the code directory")
	}
	end := bytes.IndexByte(blob[offset:], 0)
	if end < 0 {
		return "", false, errors.New("team identifier is not terminated")
	}
	return string(blob[offset : offset+uint64(end)]), adHoc, nil
}
