package main

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

func utf16Bytes(s string, order binary.ByteOrder, bom bool) []byte {
	u := utf16.Encode([]rune(s))
	if bom {
		u = append([]uint16{0xfeff}, u...)
	}
	b := make([]byte, 2*len(u))
	for i, c := range u {
		order.PutUint16(b[2*i:], c)
	}
	return b
}

func TestParseToken(t *testing.T) {
	const token = "abc123XYZ"
	for name, data := range map[string][]byte{
		"plain":          []byte(token),
		"newline":        []byte(token + "\n"),
		"crlf":           []byte(token + "\r\n"),
		"utf8 bom":       []byte("\xef\xbb\xbf" + token + "\r\n"),
		"utf16le bom":    utf16Bytes(token+"\r\n", binary.LittleEndian, true),
		"utf16be bom":    utf16Bytes(token+"\r\n", binary.BigEndian, true),
		"utf16le no bom": utf16Bytes(token, binary.LittleEndian, false),
	} {
		got, err := parseToken(data)
		if err != nil || got != token {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}
	for name, data := range map[string][]byte{
		"nul":   []byte("abc\x00def"),
		"space": []byte("abc def"),
	} {
		if _, err := parseToken(data); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if got, err := parseToken(nil); err != nil || got != "" {
		t.Errorf("empty: got %q, %v", got, err)
	}
}
