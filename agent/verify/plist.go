package verify

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

var errPlist = errors.New("invalid property list")

const (
	plistDepth = 256
	plistNodes = 1 << 20
	// decodedRatio and decodedFloor set the decoded-size budget of a binary property list: decodedRatio times its
	// length plus decodedFloor. Objects are shared by reference, so without it a small file decodes into a huge tree.
	decodedRatio = 16
	decodedFloor = 1 << 20
	// valueWeight is what every decoded value and every container slot counts against that budget, besides its bytes.
	valueWeight = 8
	// referenceUnix is 2001-01-01T00:00:00Z, the epoch of binary property list dates.
	referenceUnix = 978307200
)

var (
	binaryMagic = []byte("bplist00")
	utf8Mark    = []byte("\xef\xbb\xbf")
)

// decodePlist decodes an XML or bplist00 property list into dictionaries (map[string]any), arrays ([]any), string,
// int64 (uint64 above math.MaxInt64), float64, bool, time.Time (UTC) and []byte.
func decodePlist(data []byte) (any, error) {
	if bytes.HasPrefix(data, binaryMagic) {
		return decodeBinary(data)
	}
	return decodeXML(data)
}

func decodeDictionary(data []byte) (map[string]any, error) {
	value, err := decodePlist(data)
	if err != nil {
		return nil, err
	}
	dictionary, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: top level is %T, not a dictionary", errPlist, value)
	}
	return dictionary, nil
}

// binaryPlist decodes each object once and hands every further reference the same value, so shared objects cost
// their memory once; the budget still counts each reference in full, bounding the tree a caller walks.
type binaryPlist struct {
	data          []byte
	offsets       []uint64
	referenceSize int
	visiting      []bool
	decoded       []decodedObject
	nodes         int
	budget        uint64
}

type decodedObject struct {
	value  any
	weight uint64
	done   bool
}

func decodeBinary(data []byte) (any, error) {
	const trailerSize = 32
	if len(data) < len(binaryMagic)+trailerSize {
		return nil, fmt.Errorf("%w: binary property list is truncated", errPlist)
	}
	trailer := data[len(data)-trailerSize:]
	offsetSize := int(trailer[6])
	referenceSize := int(trailer[7])
	count := binary.BigEndian.Uint64(trailer[8:])
	top := binary.BigEndian.Uint64(trailer[16:])
	table := binary.BigEndian.Uint64(trailer[24:])
	end := uint64(len(data) - trailerSize)
	if !integerWidth(offsetSize) || !integerWidth(referenceSize) {
		return nil, fmt.Errorf("%w: offset size %d or reference size %d", errPlist, offsetSize, referenceSize)
	}
	if count == 0 || top >= count || table < uint64(len(binaryMagic)) || table > end || count > (end-table)/uint64(offsetSize) {
		return nil, fmt.Errorf("%w: trailer does not describe the file", errPlist)
	}
	offsets := make([]uint64, count)
	for index := range offsets {
		start := table + uint64(index*offsetSize)
		offset := readUnsigned(data[start : start+uint64(offsetSize)])
		if offset < uint64(len(binaryMagic)) || offset >= table {
			return nil, fmt.Errorf("%w: object %d lies outside the object area", errPlist, index)
		}
		offsets[index] = offset
	}
	decoder := &binaryPlist{
		data:          data[:table],
		offsets:       offsets,
		referenceSize: referenceSize,
		visiting:      make([]bool, count),
		decoded:       make([]decodedObject, count),
		budget:        decodedRatio*uint64(len(data)) + decodedFloor,
	}
	value, _, err := decoder.object(top, 0)
	return value, err
}

func integerWidth(size int) bool {
	return size == 1 || size == 2 || size == 4 || size == 8
}

func readUnsigned(data []byte) uint64 {
	var value uint64
	for _, octet := range data {
		value = value<<8 | uint64(octet)
	}
	return value
}

// object returns the decoded value of an object and its weight: valueWeight per value and container slot plus the
// bytes of every string and data value in its tree.
func (p *binaryPlist) object(index uint64, depth int) (any, uint64, error) {
	if index >= uint64(len(p.offsets)) {
		return nil, 0, fmt.Errorf("%w: reference %d out of range", errPlist, index)
	}
	if depth > plistDepth {
		return nil, 0, fmt.Errorf("%w: nested deeper than %d", errPlist, plistDepth)
	}
	p.nodes++
	if p.nodes > plistNodes {
		return nil, 0, fmt.Errorf("%w: more than %d values", errPlist, plistNodes)
	}
	if cached := p.decoded[index]; cached.done {
		return cached.value, cached.weight, p.spend(cached.weight)
	}
	if p.visiting[index] {
		return nil, 0, fmt.Errorf("%w: object %d contains itself", errPlist, index)
	}
	p.visiting[index] = true
	defer func() { p.visiting[index] = false }()
	value, weight, err := p.decode(p.offsets[index], depth)
	if err != nil {
		return nil, 0, err
	}
	if _, container := value.([]any); !container {
		if _, container = value.(map[string]any); !container {
			if err := p.spend(weight); err != nil {
				return nil, 0, err
			}
		}
	}
	p.decoded[index] = decodedObject{value: value, weight: weight, done: true}
	return value, weight, nil
}

// spend charges weight to the budget. A container's own slots and children are charged as they are decoded.
func (p *binaryPlist) spend(weight uint64) error {
	if weight > p.budget {
		return fmt.Errorf("%w: decodes to more than %d times its size", errPlist, decodedRatio)
	}
	p.budget -= weight
	return nil
}

func (p *binaryPlist) decode(offset uint64, depth int) (any, uint64, error) {
	marker := p.data[offset]
	kind, detail := marker>>4, marker&0x0F
	body := offset + 1
	switch kind {
	case 0x0:
		switch marker {
		case 0x08:
			return false, valueWeight, nil
		case 0x09:
			return true, valueWeight, nil
		}
	case 0x1:
		value, err := p.integer(body, detail)
		return value, valueWeight, err
	case 0x2:
		value, err := p.real(body, detail)
		return value, valueWeight, err
	case 0x3:
		if marker == 0x33 {
			value, err := p.date(body)
			return value, valueWeight, err
		}
	case 0x4:
		length, start, err := p.length(body, detail)
		if err != nil {
			return nil, 0, err
		}
		data, err := p.bytes(start, length)
		if err != nil {
			return nil, 0, err
		}
		return bytes.Clone(data), valueWeight + length, nil
	case 0x5:
		length, start, err := p.length(body, detail)
		if err != nil {
			return nil, 0, err
		}
		data, err := p.bytes(start, length)
		if err != nil {
			return nil, 0, err
		}
		text := latin1(data)
		return text, valueWeight + uint64(len(text)), nil
	case 0x6:
		length, start, err := p.length(body, detail)
		if err != nil {
			return nil, 0, err
		}
		if length > uint64(len(p.data))/2 {
			return nil, 0, fmt.Errorf("%w: string of %d code units", errPlist, length)
		}
		data, err := p.bytes(start, length*2)
		if err != nil {
			return nil, 0, err
		}
		units := make([]uint16, length)
		for unit := range units {
			units[unit] = binary.BigEndian.Uint16(data[unit*2:])
		}
		text := string(utf16.Decode(units))
		return text, valueWeight + uint64(len(text)), nil
	case 0xA:
		return p.array(body, detail, depth)
	case 0xD:
		return p.dictionary(body, detail, depth)
	}
	return nil, 0, fmt.Errorf("%w: unsupported marker 0x%02x", errPlist, marker)
}

func (p *binaryPlist) bytes(start uint64, length uint64) ([]byte, error) {
	size := uint64(len(p.data))
	if start > size || length > size-start {
		return nil, fmt.Errorf("%w: value runs past the object area", errPlist)
	}
	return p.data[start : start+length], nil
}

func (p *binaryPlist) length(body uint64, detail byte) (uint64, uint64, error) {
	if detail != 0x0F {
		return uint64(detail), body, nil
	}
	header, err := p.bytes(body, 1)
	if err != nil {
		return 0, 0, err
	}
	if header[0]>>4 != 0x1 || header[0]&0x0F > 3 {
		return 0, 0, fmt.Errorf("%w: invalid length marker 0x%02x", errPlist, header[0])
	}
	size := uint64(1) << (header[0] & 0x0F)
	data, err := p.bytes(body+1, size)
	if err != nil {
		return 0, 0, err
	}
	return readUnsigned(data), body + 1 + size, nil
}

func (p *binaryPlist) integer(start uint64, detail byte) (any, error) {
	if detail > 4 {
		return nil, fmt.Errorf("%w: integer of 2^%d bytes", errPlist, detail)
	}
	data, err := p.bytes(start, uint64(1)<<detail)
	if err != nil {
		return nil, err
	}
	switch len(data) {
	case 1, 2, 4:
		return int64(readUnsigned(data)), nil
	case 8:
		return int64(binary.BigEndian.Uint64(data)), nil
	}
	high, low := binary.BigEndian.Uint64(data), binary.BigEndian.Uint64(data[8:])
	switch {
	case high == 0 && low > math.MaxInt64:
		return low, nil
	case high == 0:
		return int64(low), nil
	case high == math.MaxUint64 && low > math.MaxInt64:
		return int64(low), nil
	}
	return nil, fmt.Errorf("%w: integer exceeds 64 bits", errPlist)
}

func (p *binaryPlist) real(start uint64, detail byte) (any, error) {
	switch detail {
	case 2:
		data, err := p.bytes(start, 4)
		if err != nil {
			return nil, err
		}
		return float64(math.Float32frombits(binary.BigEndian.Uint32(data))), nil
	case 3:
		data, err := p.bytes(start, 8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(data)), nil
	}
	return nil, fmt.Errorf("%w: real of 2^%d bytes", errPlist, detail)
}

func (p *binaryPlist) date(start uint64) (any, error) {
	data, err := p.bytes(start, 8)
	if err != nil {
		return nil, err
	}
	seconds := math.Float64frombits(binary.BigEndian.Uint64(data))
	if math.IsNaN(seconds) || math.Abs(seconds) > 1e14 {
		return nil, fmt.Errorf("%w: date %v out of range", errPlist, seconds)
	}
	whole, fraction := math.Modf(seconds)
	return time.Unix(referenceUnix+int64(whole), int64(fraction*1e9)).UTC(), nil
}

func (p *binaryPlist) references(body uint64, detail byte, multiple uint64) ([]uint64, error) {
	count, start, err := p.length(body, detail)
	if err != nil {
		return nil, err
	}
	if count > uint64(len(p.data))/multiple {
		return nil, fmt.Errorf("%w: container of %d values", errPlist, count)
	}
	size := uint64(p.referenceSize)
	data, err := p.bytes(start, count*multiple*size)
	if err != nil {
		return nil, err
	}
	references := make([]uint64, count*multiple)
	for index := range references {
		offset := uint64(index) * size
		references[index] = readUnsigned(data[offset : offset+size])
	}
	return references, nil
}

func (p *binaryPlist) array(body uint64, detail byte, depth int) (any, uint64, error) {
	references, err := p.references(body, detail, 1)
	if err != nil {
		return nil, 0, err
	}
	weight := valueWeight * uint64(1+len(references))
	if err := p.spend(weight); err != nil {
		return nil, 0, err
	}
	array := make([]any, len(references))
	for index, reference := range references {
		value, element, err := p.object(reference, depth+1)
		if err != nil {
			return nil, 0, err
		}
		array[index] = value
		weight += element
	}
	return array, weight, nil
}

func (p *binaryPlist) dictionary(body uint64, detail byte, depth int) (any, uint64, error) {
	references, err := p.references(body, detail, 2)
	if err != nil {
		return nil, 0, err
	}
	count := len(references) / 2
	weight := valueWeight * uint64(1+len(references))
	if err := p.spend(weight); err != nil {
		return nil, 0, err
	}
	dictionary := make(map[string]any, count)
	for index := range count {
		key, keyWeight, err := p.object(references[index], depth+1)
		if err != nil {
			return nil, 0, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, 0, fmt.Errorf("%w: dictionary key is %T", errPlist, key)
		}
		if _, duplicate := dictionary[name]; duplicate {
			return nil, 0, fmt.Errorf("%w: duplicate key %q", errPlist, name)
		}
		value, valueWeight, err := p.object(references[count+index], depth+1)
		if err != nil {
			return nil, 0, err
		}
		dictionary[name] = value
		weight += keyWeight + valueWeight
	}
	return dictionary, weight, nil
}

func latin1(data []byte) string {
	for _, octet := range data {
		if octet >= 0x80 {
			runes := make([]rune, len(data))
			for index, value := range data {
				runes[index] = rune(value)
			}
			return string(runes)
		}
	}
	return string(data)
}

type xmlPlist struct {
	decoder *xml.Decoder
}

func decodeXML(data []byte) (any, error) {
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, utf8Mark)))
	decoder.Strict = true
	p := xmlPlist{decoder: decoder}
	root, err := p.next()
	if err != nil || root == nil || root.Name.Local != "plist" {
		return nil, fmt.Errorf("%w: no plist element", errPlist)
	}
	element, err := p.next()
	if err != nil || element == nil {
		return nil, fmt.Errorf("%w: plist element is empty", errPlist)
	}
	value, err := p.value(*element, 0)
	if err != nil {
		return nil, err
	}
	if closing, err := p.next(); err != nil || closing != nil {
		return nil, fmt.Errorf("%w: plist element holds more than one value", errPlist)
	}
	if _, err := p.next(); err != io.EOF {
		return nil, fmt.Errorf("%w: content after the plist element", errPlist)
	}
	return value, nil
}

// next returns the next start element, nil at the end of the enclosing element, or io.EOF at the end of the document.
func (p *xmlPlist) next() (*xml.StartElement, error) {
	for {
		token, err := p.decoder.Token()
		if err == io.EOF {
			return nil, io.EOF
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errPlist, err)
		}
		switch typed := token.(type) {
		case xml.StartElement:
			return &typed, nil
		case xml.EndElement:
			return nil, nil
		case xml.CharData:
			if len(bytes.TrimSpace(typed)) > 0 {
				return nil, fmt.Errorf("%w: unexpected text %q", errPlist, typed)
			}
		}
	}
}

func (p *xmlPlist) text() (string, error) {
	var text strings.Builder
	for {
		token, err := p.decoder.Token()
		if err != nil {
			return "", fmt.Errorf("%w: %w", errPlist, err)
		}
		switch typed := token.(type) {
		case xml.CharData:
			text.Write(typed)
		case xml.EndElement:
			return text.String(), nil
		case xml.StartElement:
			return "", fmt.Errorf("%w: element %s inside a scalar", errPlist, typed.Name.Local)
		}
	}
}

func (p *xmlPlist) value(element xml.StartElement, depth int) (any, error) {
	if depth > plistDepth {
		return nil, fmt.Errorf("%w: nested deeper than %d", errPlist, plistDepth)
	}
	switch element.Name.Local {
	case "dict":
		return p.dictionary(depth)
	case "array":
		return p.array(depth)
	case "string":
		return p.text()
	}
	text, err := p.text()
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	switch element.Name.Local {
	case "integer":
		return parseInteger(text)
	case "real":
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: real %q", errPlist, text)
		}
		return value, nil
	case "true", "false":
		if text != "" {
			return nil, fmt.Errorf("%w: %s holds text", errPlist, element.Name.Local)
		}
		return element.Name.Local == "true", nil
	case "date":
		value, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return nil, fmt.Errorf("%w: date %q", errPlist, text)
		}
		return value.UTC(), nil
	case "data":
		value, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(text), ""))
		if err != nil {
			return nil, fmt.Errorf("%w: data is not base64", errPlist)
		}
		return value, nil
	}
	return nil, fmt.Errorf("%w: unknown element %s", errPlist, element.Name.Local)
}

func (p *xmlPlist) dictionary(depth int) (any, error) {
	dictionary := map[string]any{}
	for {
		key, err := p.next()
		if err != nil {
			return nil, err
		}
		if key == nil {
			return dictionary, nil
		}
		if key.Name.Local != "key" {
			return nil, fmt.Errorf("%w: %s where a key belongs", errPlist, key.Name.Local)
		}
		name, err := p.text()
		if err != nil {
			return nil, err
		}
		element, err := p.next()
		if err != nil || element == nil {
			return nil, fmt.Errorf("%w: key %q has no value", errPlist, name)
		}
		if _, duplicate := dictionary[name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate key %q", errPlist, name)
		}
		if dictionary[name], err = p.value(*element, depth+1); err != nil {
			return nil, err
		}
	}
}

func (p *xmlPlist) array(depth int) (any, error) {
	array := []any{}
	for {
		element, err := p.next()
		if err != nil {
			return nil, err
		}
		if element == nil {
			return array, nil
		}
		value, err := p.value(*element, depth+1)
		if err != nil {
			return nil, err
		}
		array = append(array, value)
	}
}

func parseInteger(text string) (any, error) {
	digits := strings.TrimPrefix(text, "+")
	negative := false
	if rest, ok := strings.CutPrefix(text, "-"); ok {
		digits, negative = rest, true
	}
	base := 10
	if rest, ok := strings.CutPrefix(digits, "0x"); ok {
		digits, base = rest, 16
	} else if rest, ok := strings.CutPrefix(digits, "0X"); ok {
		digits, base = rest, 16
	}
	magnitude, err := strconv.ParseUint(digits, base, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: integer %q", errPlist, text)
	}
	switch {
	case negative && magnitude > 1<<63:
		return nil, fmt.Errorf("%w: integer %q exceeds 64 bits", errPlist, text)
	case negative:
		return int64(-magnitude), nil
	case magnitude > math.MaxInt64:
		return magnitude, nil
	}
	return int64(magnitude), nil
}
