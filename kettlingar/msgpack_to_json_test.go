package kettlingar

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

func TestDecodeMsgPack(t *testing.T) {
	converter := NewJsonConverter()
	tests := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{
			name:     "Simple Integer",
			input:    42,
			expected: "42",
		},
		{
			name:     "Simple Float",
			input:    0.42,
			expected: "0.42",
		},
		{
			name:     "Nested Structure",
			input:    map[string]interface{}{"score": 100, "tags": []string{"pro", "ai"}},
			expected: `{"score": 100, "tags": ["pro", "ai"]}`,
		},
		{
			name:     "Boolean and Nil",
			input:    []interface{}{true, false, nil},
			expected: "[true, false, null]",
		},
		{
			name:     "Deeply Nested Map",
			input:    map[string]interface{}{"a": map[string]int{"b": 1}},
			expected: `{"a": {"b": 1}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := msgpack.NewEncoder(&buf)
			enc.SetSortMapKeys(true)
			err := enc.Encode(tt.input)
			if err != nil {
				t.Fatalf("Failed to encode test data: %v", err)
			}

			var out bytes.Buffer
			// Using the new struct method
			err = converter.decodeNext(&buf, &out)
			if err != nil {
				t.Fatalf("Decoder returned error: %v", err)
			}

			got := out.String()
			if !compareJSON(got, tt.expected) {
				t.Errorf("\ngot:      %s\nexpected: %s", got, tt.expected)
			}
		})
	}
}

func TestExtension(t *testing.T) {
	converter := NewJsonConverter()
	// Adding a custom mapping for this test
	converter.ExtensionNames[1] = "coffee"

	var buf bytes.Buffer
	buf.WriteByte(0xd5) // fixext 2
	buf.WriteByte(0x01) // type 1
	buf.Write([]byte{0xca, 0xfe})

	var out bytes.Buffer
	err := converter.decodeNext(&buf, &out)
	if err != nil {
		t.Fatal(err)
	}

	// Because of %#v and the mapping, label becomes "coffee"
	expected := `["coffee", "cafe"]`
	if out.String() != expected {
		t.Errorf("Extension failed. Got %s, want %s", out.String(), expected)
	}
}

func TestTimestampRoundTrip(t *testing.T) {
	converter := NewJsonConverter()
	tests := []struct {
		name string
		time time.Time
	}{
		{
			name: "32-bit format (Year 2023)",
			time: time.Unix(1700000000, 0).UTC(),
		},
		{
			name: "64-bit format (Year 2026 with Nsec)",
			time: time.Unix(1772383758, 500000).UTC(),
		},
		{
			name: "96-bit format (Year 3000)",
			time: time.Unix(32503680000, 999).UTC(),
		},
		{
			name: "96-bit format (Pre-1970)",
			time: time.Unix(-1000000, 500).UTC(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := msgpack.NewEncoder(&buf)
			enc.SetSortMapKeys(true)
			if err := enc.Encode(tt.time); err != nil {
				t.Fatalf("Encoding failed: %v", err)
			}

			var out bytes.Buffer
			if err := converter.decodeNext(&buf, &out); err != nil {
				t.Fatalf("Decoding failed: %v", err)
			}

			expected := tt.time.Format(time.RFC3339Nano)
			//expected := fmt.Sprintf("[\"ts\", %q]", tttf)

			if out.String() != expected {
				t.Errorf("\nGot:      %s\nExpected: %s", out.String(), expected)
			}
		})
	}
}

func compareJSON(actual, expected string) bool {
	replacer := strings.NewReplacer(" ", "", "\n", "")
	return replacer.Replace(actual) == replacer.Replace(expected)
}
