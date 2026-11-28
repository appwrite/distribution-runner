package verify

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"maps"
	"math"
	"math/bits"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

const xmlPrologue = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
`

// encodeXML writes the value types decodePlist returns, plus int, as an XML property list.
func encodeXML(value any) ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteString(xmlPrologue)
	if err := writeXML(&buffer, value, 0); err != nil {
		return nil, err
	}
	buffer.WriteString("</plist>\n")
	return buffer.Bytes(), nil
}

func writeXML(buffer *bytes.Buffer, value any, depth int) error {
	indent := strings.Repeat("\t", depth)
	buffer.WriteString(indent)
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 0 {
			buffer.WriteString("<dict/>\n")
			return nil
		}
		buffer.WriteString("<dict>\n")
		for _, key := range slices.Sorted(maps.Keys(typed)) {
			buffer.WriteString(indent + "\t<key>")
			if err := xml.EscapeText(buffer, []byte(key)); err != nil {
				return err
			}
			buffer.WriteString("</key>\n")
			if err := writeXML(buffer, typed[key], depth+1); err != nil {
				return err
			}
		}
		buffer.WriteString(indent + "</dict>\n")
		return nil
	case []any:
		if len(typed) == 0 {
			buffer.WriteString("<array/>\n")
			return nil
		}
		buffer.WriteString("<array>\n")
		for _, element := range typed {
			if err := writeXML(buffer, element, depth+1); err != nil {
				return err
			}
		}
		buffer.WriteString(indent + "</array>\n")
		return nil
	case string:
		buffer.WriteString("<string>")
		if err := xml.EscapeText(buffer, []byte(typed)); err != nil {
			return err
		}
		buffer.WriteString("</string>\n")
	case bool:
		buffer.WriteString("<" + strconv.FormatBool(typed) + "/>\n")
	case int:
		fmt.Fprintf(buffer, "<integer>%d</integer>\n", typed)
	case int64:
		fmt.Fprintf(buffer, "<integer>%d</integer>\n", typed)
	case uint64:
		fmt.Fprintf(buffer, "<integer>%d</integer>\n", typed)
	case float64:
		buffer.WriteString("<real>" + strconv.FormatFloat(typed, 'g', -1, 64) + "</real>\n")
	case time.Time:
		buffer.WriteString("<date>" + typed.UTC().Format("2006-01-02T15:04:05Z") + "</date>\n")
	case []byte:
		buffer.WriteString("<data>" + base64.StdEncoding.EncodeToString(typed) + "</data>\n")
	default:
		return fmt.Errorf("property list cannot hold %T", value)
	}
	return nil
}

type binaryEncoder struct {
	objects    []any
	references [][]uint64
}

// encodeBinary writes the value types decodePlist returns, plus int, as a bplist00 property list.
func encodeBinary(value any) ([]byte, error) {
	encoder := &binaryEncoder{}
	if _, err := encoder.flatten(value); err != nil {
		return nil, err
	}
	referenceSize := widthFor(uint64(len(encoder.objects) - 1))
	buffer := bytes.NewBuffer(slices.Clone(binaryMagic))
	offsets := make([]uint64, len(encoder.objects))
	for index, object := range encoder.objects {
		offsets[index] = uint64(buffer.Len())
		encoder.write(buffer, object, encoder.references[index], referenceSize)
	}
	table := uint64(buffer.Len())
	offsetSize := widthFor(table)
	for _, offset := range offsets {
		writeWidth(buffer, offset, offsetSize)
	}
	trailer := make([]byte, 32)
	trailer[6] = byte(offsetSize)
	trailer[7] = byte(referenceSize)
	binary.BigEndian.PutUint64(trailer[8:], uint64(len(encoder.objects)))
	binary.BigEndian.PutUint64(trailer[24:], table)
	buffer.Write(trailer)
	return buffer.Bytes(), nil
}

func (e *binaryEncoder) flatten(value any) (uint64, error) {
	index := uint64(len(e.objects))
	e.objects = append(e.objects, value)
	e.references = append(e.references, nil)
	switch typed := value.(type) {
	case map[string]any:
		keys := slices.Sorted(maps.Keys(typed))
		references := make([]uint64, 0, 2*len(keys))
		for _, key := range keys {
			reference, _ := e.flatten(key)
			references = append(references, reference)
		}
		for _, key := range keys {
			reference, err := e.flatten(typed[key])
			if err != nil {
				return 0, err
			}
			references = append(references, reference)
		}
		e.references[index] = references
	case []any:
		references := make([]uint64, 0, len(typed))
		for _, element := range typed {
			reference, err := e.flatten(element)
			if err != nil {
				return 0, err
			}
			references = append(references, reference)
		}
		e.references[index] = references
	case string, bool, int, int64, uint64, float64, time.Time, []byte:
	default:
		return 0, fmt.Errorf("property list cannot hold %T", value)
	}
	return index, nil
}

func (e *binaryEncoder) write(buffer *bytes.Buffer, value any, references []uint64, referenceSize int) {
	switch typed := value.(type) {
	case map[string]any:
		writeHeader(buffer, 0xD, uint64(len(references)/2))
		for _, reference := range references {
			writeWidth(buffer, reference, referenceSize)
		}
	case []any:
		writeHeader(buffer, 0xA, uint64(len(references)))
		for _, reference := range references {
			writeWidth(buffer, reference, referenceSize)
		}
	case string:
		if ascii(typed) {
			writeHeader(buffer, 0x5, uint64(len(typed)))
			buffer.WriteString(typed)
			return
		}
		units := utf16.Encode([]rune(typed))
		writeHeader(buffer, 0x6, uint64(len(units)))
		for _, unit := range units {
			writeWidth(buffer, uint64(unit), 2)
		}
	case bool:
		if typed {
			buffer.WriteByte(0x09)
		} else {
			buffer.WriteByte(0x08)
		}
	case int:
		writeInteger(buffer, int64(typed))
	case int64:
		writeInteger(buffer, typed)
	case uint64:
		if typed <= math.MaxInt64 {
			writeInteger(buffer, int64(typed))
			return
		}
		buffer.WriteByte(0x14)
		writeWidth(buffer, 0, 8)
		writeWidth(buffer, typed, 8)
	case float64:
		buffer.WriteByte(0x23)
		writeWidth(buffer, math.Float64bits(typed), 8)
	case time.Time:
		seconds := float64(typed.Unix()-referenceUnix) + float64(typed.Nanosecond())/1e9
		buffer.WriteByte(0x33)
		writeWidth(buffer, math.Float64bits(seconds), 8)
	case []byte:
		writeHeader(buffer, 0x4, uint64(len(typed)))
		buffer.Write(typed)
	}
}

func writeHeader(buffer *bytes.Buffer, kind byte, count uint64) {
	if count < 0x0F {
		buffer.WriteByte(kind<<4 | byte(count))
		return
	}
	buffer.WriteByte(kind<<4 | 0x0F)
	writeInteger(buffer, int64(count))
}

func writeInteger(buffer *bytes.Buffer, value int64) {
	if value < 0 {
		buffer.WriteByte(0x13)
		writeWidth(buffer, uint64(value), 8)
		return
	}
	width := widthFor(uint64(value))
	buffer.WriteByte(0x10 | byte(bits.TrailingZeros(uint(width))))
	writeWidth(buffer, uint64(value), width)
}

func widthFor(maximum uint64) int {
	switch {
	case maximum <= math.MaxUint8:
		return 1
	case maximum <= math.MaxUint16:
		return 2
	case maximum <= math.MaxUint32:
		return 4
	}
	return 8
}

func writeWidth(buffer *bytes.Buffer, value uint64, width int) {
	for shift := (width - 1) * 8; shift >= 0; shift -= 8 {
		buffer.WriteByte(byte(value >> shift))
	}
}

func ascii(text string) bool {
	for index := range len(text) {
		if text[index] >= 0x80 {
			return false
		}
	}
	return true
}
