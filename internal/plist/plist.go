// Package plist reads and writes the XML property lists macOS tools speak: `ioreg -a` (the console session's lock
// state), `codesign -d --entitlements - --xml` (a binary's entitlements) and launchd job files. It handles the
// value types those use — dict, array, string, integer, real, true/false, data, date — and nothing more.
package plist

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Decode parses one XML property list. Dicts come back as map[string]any, arrays as []any, strings as string,
// integers as int64, reals as float64, booleans as bool, data as []byte and dates as their text.
func Decode(b []byte) (any, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	for {
		tok, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("plist: no <plist> element")
			}
			return nil, fmt.Errorf("plist: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			if se.Name.Local != "plist" {
				return nil, fmt.Errorf("plist: root element <%s>, want <plist>", se.Name.Local)
			}
			v, end, err := value(d)
			if err != nil {
				return nil, err
			}
			if end {
				return nil, errors.New("plist: empty <plist>")
			}
			return v, nil
		}
	}
}

// value reads the next value element. end is true when the enclosing element closed instead.
func value(d *xml.Decoder) (v any, end bool, err error) {
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, false, fmt.Errorf("plist: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return nil, true, nil
		case xml.StartElement:
			v, err := element(d, t)
			return v, false, err
		}
	}
}

func element(d *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		m := map[string]any{}
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, fmt.Errorf("plist: %w", err)
			}
			switch t := tok.(type) {
			case xml.EndElement:
				return m, nil
			case xml.StartElement:
				if t.Name.Local != "key" {
					return nil, fmt.Errorf("plist: <%s> in a dict where a <key> belongs", t.Name.Local)
				}
				k, err := text(d)
				if err != nil {
					return nil, err
				}
				v, end, err := value(d)
				if err != nil {
					return nil, err
				}
				if end {
					return nil, fmt.Errorf("plist: key %q has no value", k)
				}
				m[k] = v
			}
		}
	case "array":
		a := []any{}
		for {
			v, end, err := value(d)
			if err != nil {
				return nil, err
			}
			if end {
				return a, nil
			}
			a = append(a, v)
		}
	case "true", "false":
		if err := d.Skip(); err != nil {
			return nil, fmt.Errorf("plist: %w", err)
		}
		return se.Name.Local == "true", nil
	case "string", "date":
		return text(d)
	case "integer":
		s, err := text(d)
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 0, 64)
		if err != nil {
			// ioreg prints large unsigned values in hex or past int64.
			u, uerr := strconv.ParseUint(strings.TrimSpace(s), 0, 64)
			if uerr != nil {
				return nil, fmt.Errorf("plist: integer %q: %w", s, err)
			}
			n = int64(u)
		}
		return n, nil
	case "real":
		s, err := text(d)
		if err != nil {
			return nil, err
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("plist: real %q: %w", s, err)
		}
		return f, nil
	case "data":
		s, err := text(d)
		if err != nil {
			return nil, err
		}
		b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
		if err != nil {
			return nil, fmt.Errorf("plist: data: %w", err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("plist: unknown element <%s>", se.Name.Local)
}

// text reads character data up to the current element's end.
func text(d *xml.Decoder) (string, error) {
	var sb strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			return "", fmt.Errorf("plist: %w", err)
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.EndElement:
			return sb.String(), nil
		case xml.StartElement:
			return "", fmt.Errorf("plist: <%s> inside a text element", t.Name.Local)
		}
	}
}

const header = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
`

// Encode writes v as an XML property list, keys of each dict in sorted order and one tab per level, the layout
// plutil and launchd's own files use. v is built from map[string]any, []any, []string, string, bool and int.
func Encode(v any) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(header)
	if err := encode(&b, v, 0); err != nil {
		return nil, err
	}
	b.WriteString("</plist>\n")
	return b.Bytes(), nil
}

func encode(b *bytes.Buffer, v any, depth int) error {
	ind := strings.Repeat("\t", depth)
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			b.WriteString(ind + "<dict/>\n")
			return nil
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(ind + "<dict>\n")
		for _, k := range keys {
			b.WriteString(ind + "\t<key>" + escape(k) + "</key>\n")
			if err := encode(b, t[k], depth+1); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
		b.WriteString(ind + "</dict>\n")
	case []string:
		a := make([]any, len(t))
		for i, s := range t {
			a[i] = s
		}
		return encode(b, a, depth)
	case []any:
		if len(t) == 0 {
			b.WriteString(ind + "<array/>\n")
			return nil
		}
		b.WriteString(ind + "<array>\n")
		for _, e := range t {
			if err := encode(b, e, depth+1); err != nil {
				return err
			}
		}
		b.WriteString(ind + "</array>\n")
	case string:
		b.WriteString(ind + "<string>" + escape(t) + "</string>\n")
	case bool:
		if t {
			b.WriteString(ind + "<true/>\n")
		} else {
			b.WriteString(ind + "<false/>\n")
		}
	case int:
		b.WriteString(ind + "<integer>" + strconv.Itoa(t) + "</integer>\n")
	default:
		return fmt.Errorf("plist: cannot encode %T", v)
	}
	return nil
}

func escape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
