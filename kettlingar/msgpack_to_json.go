package kettlingar

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"
)

// MsgpackJsonConverter handles the transformation of Msgpack binary streams to JSON.
type MsgpackJsonConverter struct {
	// ExtensionNames maps extension type codes to human-readable strings.
	ExtensionNames map[int8]string
}

// NewConverter initializes a converter with the default standard mappings.
func NewJsonConverter() *MsgpackJsonConverter {
	return &MsgpackJsonConverter{
		ExtensionNames: map[int8]string{
			-1: "ts",
		},
	}
}

// Convert reads the entire Msgpack stream from r and writes JSON to w.
func (c *MsgpackJsonConverter) Convert(r io.Reader, w io.Writer) error {
	for {
		err := c.decodeNext(r, w)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprint(w, "\n")
	}
}

func (c *MsgpackJsonConverter) decodeNext(r io.Reader, w io.Writer) error {
	prefix := make([]byte, 1)
	if _, err := r.Read(prefix); err != nil {
		return err
	}

	p := prefix[0]

	switch {
	// Positive FixInt (0xxxxxxx)
	case p <= 0x7f:
		fmt.Fprintf(w, "%d", p)

	// FixMap (1000xxxx)
	case p >= 0x80 && p <= 0x8f:
		return c.decodeMap(r, w, int(p&0x0f))

	// FixArray (1001xxxx)
	case p >= 0x90 && p <= 0x9f:
		return c.decodeArray(r, w, int(p&0x0f))

	// FixStr (101xxxxx)
	case p >= 0xa0 && p <= 0xbf:
		return c.readAndPrintStr(r, w, int(p&0x1f))

	// Nil, Booleans, Floats, and Integers...
	case p == 0xc0:
		fmt.Fprint(w, "null")
	case p == 0xc2:
		fmt.Fprint(w, "false")
	case p == 0xc3:
		fmt.Fprint(w, "true")
	case p == 0xca:
		fmt.Fprintf(w, "%g", c.readFloat32(r))
	case p == 0xcb:
		fmt.Fprintf(w, "%g", c.readFloat64(r))

	// Unsigned / Signed Integers
	case p == 0xcc:
		fmt.Fprintf(w, "%d", c.readUint8(r))
	case p == 0xcd:
		fmt.Fprintf(w, "%d", c.readUint16(r))
	case p == 0xce:
		fmt.Fprintf(w, "%d", c.readUint32(r))
	case p == 0xcf:
		fmt.Fprintf(w, "%d", c.readUint64(r))
	case p == 0xd0:
		fmt.Fprintf(w, "%d", int8(c.readUint8(r)))
	case p == 0xd1:
		fmt.Fprintf(w, "%d", int16(c.readUint16(r)))
	case p == 0xd2:
		fmt.Fprintf(w, "%d", int32(c.readUint32(r)))
	case p == 0xd3:
		fmt.Fprintf(w, "%d", int64(c.readUint64(r)))

	// Variable Strings, Arrays, Maps
	case p == 0xd9:
		return c.readAndPrintStr(r, w, int(c.readUint8(r)))
	case p == 0xda:
		return c.readAndPrintStr(r, w, int(c.readUint16(r)))
	case p == 0xdb:
		return c.readAndPrintStr(r, w, int(c.readUint32(r)))
	case p == 0xdc:
		return c.decodeArray(r, w, int(c.readUint16(r)))
	case p == 0xdd:
		return c.decodeArray(r, w, int(c.readUint32(r)))
	case p == 0xde:
		return c.decodeMap(r, w, int(c.readUint16(r)))
	case p == 0xdf:
		return c.decodeMap(r, w, int(c.readUint32(r)))

	case p == 0xc4: // bin 8
		return c.readAndPrintBin(r, w, int(c.readUint8(r)))
	case p == 0xc5: // bin 16
		return c.readAndPrintBin(r, w, int(c.readUint16(r)))
	case p == 0xc6: // bin 32
		return c.readAndPrintBin(r, w, int(c.readUint32(r)))

	// Extension Packets
	case p >= 0xd4 && p <= 0xd8:
		sizes := map[byte]int{0xd4: 1, 0xd5: 2, 0xd6: 4, 0xd7: 8, 0xd8: 16}
		return c.decodeExtension(r, w, sizes[p])
	case p == 0xc7:
		return c.decodeExtension(r, w, int(c.readUint8(r)))
	case p == 0xc8:
		return c.decodeExtension(r, w, int(c.readUint16(r)))
	case p == 0xc9:
		return c.decodeExtension(r, w, int(c.readUint32(r)))

	// Negative FixInt
	case p >= 0xe0:
		fmt.Fprintf(w, "%d", int8(p))

	default:
		return fmt.Errorf("unknown prefix: 0x%x", p)
	}
	return nil
}

func (c *MsgpackJsonConverter) decodeArray(r io.Reader, w io.Writer, size int) error {
	fmt.Fprint(w, "[")
	for i := 0; i < size; i++ {
		if i > 0 {
			fmt.Fprint(w, ", ")
		}
		if err := c.decodeNext(r, w); err != nil {
			return err
		}
	}
	fmt.Fprint(w, "]")
	return nil
}

func (c *MsgpackJsonConverter) decodeMap(r io.Reader, w io.Writer, size int) error {
	fmt.Fprint(w, "{")
	for i := 0; i < size; i++ {
		if i > 0 {
			fmt.Fprint(w, ", ")
		}
		if err := c.decodeNext(r, w); err != nil {
			return err
		}
		fmt.Fprint(w, ": ")
		if err := c.decodeNext(r, w); err != nil {
			return err
		}
	}
	fmt.Fprint(w, "}")
	return nil
}

func (c *MsgpackJsonConverter) readAndPrintStr(r io.Reader, w io.Writer, size int) error {
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	fmt.Fprintf(w, "%q", string(buf))
	return nil
}

func (c *MsgpackJsonConverter) decodeExtension(r io.Reader, w io.Writer, size int) error {
	typBuf := make([]byte, 1)
	if _, err := io.ReadFull(r, typBuf); err != nil {
		return err
	}
	typ := int8(typBuf[0])

	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}

	// Determine label (Friendly name or raw code)
	var label interface{} = typ
	if name, ok := c.ExtensionNames[typ]; ok {
		label = name
	}

	// Handle standard Timestamp extension (Type -1)
	if typ == -1 {
		var sec, nsec int64
		switch size {
		case 4:
			sec = int64(binary.BigEndian.Uint32(data))
		case 8:
			val := binary.BigEndian.Uint64(data)
			nsec = int64(val >> 34)
			sec = int64(val & 0x00000003ffffffff)
		case 12:
			nsec = int64(binary.BigEndian.Uint32(data[:4]))
			sec = int64(binary.BigEndian.Uint64(data[4:]))
		default:
			return fmt.Errorf("invalid timestamp size: %d", size)
		}
		t := time.Unix(sec, nsec).UTC()
		fmt.Fprintf(w, "%q", t.Format(time.RFC3339Nano))
		return nil
	}

	fmt.Fprintf(w, "[%#v, \"%x\"]", label, data)
	return nil
}

// Binary primitive readers
func (c *MsgpackJsonConverter) readUint8(r io.Reader) uint8 {
	b := make([]byte, 1)
	r.Read(b)
	return b[0]
}
func (c *MsgpackJsonConverter) readUint16(r io.Reader) uint16 {
	b := make([]byte, 2)
	io.ReadFull(r, b)
	return binary.BigEndian.Uint16(b)
}
func (c *MsgpackJsonConverter) readUint32(r io.Reader) uint32 {
	b := make([]byte, 4)
	io.ReadFull(r, b)
	return binary.BigEndian.Uint32(b)
}
func (c *MsgpackJsonConverter) readUint64(r io.Reader) uint64 {
	b := make([]byte, 8)
	io.ReadFull(r, b)
	return binary.BigEndian.Uint64(b)
}
func (c *MsgpackJsonConverter) readFloat32(r io.Reader) float32 {
	bits := c.readUint32(r)
	return math.Float32frombits(bits)
}
func (c *MsgpackJsonConverter) readFloat64(r io.Reader) float64 {
	bits := c.readUint64(r)
	return math.Float64frombits(bits)
}
func (c *MsgpackJsonConverter) readAndPrintBin(r io.Reader, w io.Writer, size int) error {
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	fmt.Fprintf(w, "\"%x\"", buf)
	return nil
}
