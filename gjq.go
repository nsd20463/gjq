// gjq is a simpler but (better be) faster alternative to jq for extracting fields from JSON input
//
// The amount of time and CPU it takes jq to do simple field extractions is impacting my life.
// This is a replacement which ought to run faster.
// Of course it doesn't support more than a fraction of what jq does. On the other hand it supports
// just what I use jq most often for when processing millions of records.
//
// IDEAS:
//   can I to do this concurrently? The IO I can hide. I don't know if finding boundaries of objects
//   is so much faster than decoding them that I can get some concurrency out of the object parsing.
//
// Copyright 2018 Nicolas S. Dade

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"reflect"
	"runtime/pprof"
	"strings"

	"github.com/pkg/errors"
)

var RAW = false
var PRETTY = false
var COMPACT = false

var json_rawmsg_type = reflect.TypeOf(json.RawMessage{})

func main() {
	var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")
	var stdlib = flag.Bool("stdlib", false, "use stdlib encoding/json")
	var read_buf_size = flag.Int("buf", 64*1024, "size of input I/O buffer") // experiments show >64kB buffers is, strangely, counter-productive
	flag.BoolVar(&RAW, "r", RAW, `raw output for strings (unescape and remove "")`)
	flag.BoolVar(&PRETTY, "pretty", PRETTY, "pretty-print output")
	flag.BoolVar(&COMPACT, "compact", COMPACT, "compact output")
	flag.BoolVar(&COMPACT, "c", COMPACT, "compact output")

	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	args := flag.Args()
	if len(args) != 1 {
		log.Printf("1 filter argument required")
		os.Exit(1)
	}

	filter, err := makeToplevelFilter(args[0])
	if err != nil {
		log.Printf("Can't understand filter arguments: %s\n", err)
		os.Exit(1)
	}

	if *stdlib {
		// filter using the stdlib
		out := io.Writer(os.Stdout)
		in := io.Reader(os.Stdin)
		dec := json.NewDecoder(in)
		rec_num := 0
		for {
			rec_num++
			v := reflect.New(filter.typeof())
			err := dec.Decode(v.Interface())
			if err != nil {
				if err == io.EOF {
					return
				}
				log.Printf("Can't decode record %d of input: %+v\n", rec_num, err)
				os.Exit(1)
			}

			filter.filter(v.Elem(), out)
		}
	} else {
		// filter using our poorly-written scanner code
		out := io.Writer(os.Stdout)
		in := newReader(os.Stdin, *read_buf_size)
		rec_num := 0
		for {
			rec_num++
			err := filter.scan(in, out)
			if err != nil {
				if err == io.EOF {
					return
				}
				log.Printf("Can't decode record %d of input: %+v\n", rec_num, err)
				os.Exit(1)
			}
		}
	}
}

// ------------------------------------------------------------------------------------------------------------
// decoder of arbitrary JSON objects based on constructing a type with reflection, unmarshaling into an instance
// of that type, and walking down into the instance and printing out what we find.

type filter interface {
	scan(*reader, io.Writer) error
	filter(reflect.Value, io.Writer) error
	typeof() reflect.Type
}

// top level makeFilter call. Apart from whatever makeFilter handles, also parse
//   X,Y   ... extract X, then Y, both at the top level
func makeToplevelFilter(arg string) (filter, error) {
	pieces := strings.Split(arg, ",")
	if len(pieces) == 1 {
		return makeFilter(arg, 0)
	}

	// OR the pieces together. each piece must be a dict
	var f fields
	f.fields = make(map[string]int, len(pieces))
	f.dicts = make([]*dict, len(pieces))
	for i := range pieces {
		filt, err := makeFilter(pieces[i], 0)
		if err != nil {
			return nil, err
		}
		var d *dict
		var ok bool
		if d, ok = filt.(*dict); !ok {
			return nil, errors.Errorf("can't OR together non-dictionary fields like %q", pieces[i])
		}
		f.fields[d.name] = i
		f.dicts[i] = d
	}

	struct_fields := make([]reflect.StructField, len(f.dicts))
	for i := range f.dicts {
		struct_fields[i] = f.dicts[i].t.Field(0)
	}
	f.t = reflect.StructOf(struct_fields)

	return &f, nil
}

func makeFilter(arg string, pos int) (filter, error) {
	// parse the arg string
	// we don't handle the entire world. We handle
	//   .     ... extract a entire value
	//   .X    ... extract element X from a dict
	//   []    ... extract all elements of an array
	//   .[]   ... same as [] (for compatability with jq, which doesn't accept just '[]')
	// and for this 1st pass, this is all. I'll add more as I need them

	if len(arg) == pos {
		// an empty filter matches everything
		return &value{}, nil
	}

	switch arg[pos] {
	default:
		return nil, errors.Errorf("can't parse %q at index %d: unknown operator '%c'", arg, pos, arg[pos])
	case '.':
		var field string
		var err error
		if pos+1 == len(arg) {
			// '.' terminates the filter. we are to return the entire value
			return &value{}, nil
		} else if arg[pos+1] == '[' {
			// .[] case. we ignore the . and move on to the []
			return makeFilter(arg, pos+1)
		} else if field, pos, err = extractFieldName(arg, pos+1); err != nil {
			return nil, err
		} else {
			var field_type reflect.Type
			var f filter
			if len(arg) == pos {
				// this is the innermost field
				field_type = json_rawmsg_type
			} else {
				f, err = makeFilter(arg, pos)
				if err != nil {
					return nil, err
				}
				field_type = f.typeof()
			}
			field_name := strings.ToTitle(field[:1]) + field[1:]
			var field_tag string
			if field_name != field {
				field_tag = `json:"` + field + `"`
			}
			return &dict{
				name: field,
				t: reflect.StructOf([]reflect.StructField{
					reflect.StructField{
						Name: field_name,
						Type: field_type,
						Tag:  reflect.StructTag(field_tag),
					}}),
				f: f,
			}, nil
		}

	case '[':
		if len(arg) < pos+2 || arg[pos+1] != ']' {
			return nil, errors.Errorf("can't parse %q at index %d: expected ']' after '['", arg, pos)
		}
		pos += 2

		f, err := makeFilter(arg, pos)
		if err != nil {
			return nil, err
		}

		return &array{
			t: reflect.SliceOf(f.typeof()),
			f: f,
		}, nil
	}
}

func extractFieldName(arg string, pos int) (field string, remaining_pos int, err error) {
	// scan forward until we find a non-field char
	for i, c := range arg[pos:] {
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', 'A' <= c && c <= 'Z', c == '_':
			// great, keep accumulating
		default:
			// end of field name
			if i == 0 {
				return "", pos, errors.Errorf("Expacted field name at %q index %d, found %q ", arg, pos, arg[pos:])
			}
			return arg[pos : pos+i], pos + i, nil
		}
	}
	// the entire arg is the field name
	return arg[pos:], len(arg), nil
}

// JSON array (Go slice)
type array struct {
	f filter       // element type
	t reflect.Type // a slice type
}

func (a *array) typeof() reflect.Type { return a.t }
func (a *array) filter(in reflect.Value, out io.Writer) error {
	n := in.Len()
	for i := 0; i < n; i++ {
		if err := a.f.filter(in.Index(i), out); err != nil {
			return err
		}
	}
	return nil
}

// JSON dictionary (Go struct)
type dict struct {
	name string       // the field name
	f    filter       // the field type, or nil if this is the leaf
	tmp  []byte       // tmp buffer with (eventually) an appropriate capacity. avoids append reallocs and gc work
	t    reflect.Type // the struct type with one field called 'name' (or more precisely, an uppercase version of 'name')
}

func (d *dict) typeof() reflect.Type { return d.t }
func (d *dict) filter(in reflect.Value, out io.Writer) error {
	var err error
	v := in.Field(0)
	if d.f != nil {
		return d.f.filter(v, out)
	}

	// we're the leaf. we print v
	txt := []byte(fmt.Sprintf("%s\n", v.Interface()))
	if RAW && len(txt) >= 3 && txt[0] == '"' {
		txt = unescapeString(txt[1 : len(txt)-2])
		txt = append(txt, '\n')
	}

	if COMPACT {
		var b bytes.Buffer
		err = json.Compact(&b, txt)
		if err != nil {
			return err
		}
		txt = b.Bytes()
	}

	if PRETTY {
		var b bytes.Buffer
		err = json.Indent(&b, txt, "", "  ")
		if err != nil {
			return err
		}
		txt = b.Bytes()
	}

	_, err = out.Write(txt)
	return err
}

// JSON value (might be a whole JSON object) (Go json.RawMessage)
type value struct {
	tmp []byte // tmp buffer with (eventually) an appropriate capacity. avoids append reallocs and gc work
}

func (val *value) typeof() reflect.Type { return json_rawmsg_type }
func (val *value) filter(in reflect.Value, out io.Writer) error {
	// print the entire value
	_, err := out.Write([]byte(fmt.Sprintf("%s\n", in.Interface())))
	return err
}

// a set of dict fields (X,Y)
type fields struct {
	fields map[string]int // map from JSON field name -> index into dicts[] and struct field index
	dicts  []*dict
	t      reflect.Type
}

func (f *fields) typeof() reflect.Type { return f.t }

func (f *fields) filter(in reflect.Value, out io.Writer) error {
	var err error

	for i := 0; i < f.t.NumField(); i++ {
		v := in.Field(i)
		if f.dicts[i].f != nil {
			return f.dicts[i].f.filter(v, out)
		}

		// we're the leaf. we print v
		txt := []byte(fmt.Sprintf("%s\n", v.Interface()))
		if RAW && len(txt) >= 3 && txt[0] == '"' {
			txt = unescapeString(txt[1 : len(txt)-2])
			txt = append(txt, '\n')
		}

		if COMPACT {
			var b bytes.Buffer
			err = json.Compact(&b, txt)
			if err != nil {
				return err
			}
			txt = b.Bytes()
		}

		if PRETTY {
			var b bytes.Buffer
			err = json.Indent(&b, txt, "", "  ")
			if err != nil {
				return err
			}
			txt = b.Bytes()
		}

		_, err = out.Write(txt)
		if err != nil {
			return err
		}
	}
	return err
}

// ------------------------------------------------------------------------
// arbitrary JSON decoder based on a custom JSON scanner which is optimized for skipping the unwanted fields

func (a *array) scan(in *reader, out io.Writer) error {
	var c byte
	var err error
	if c, err = scanPastWhitespace(in); err != nil {
		return err
	} else if c == 'n' {
		// null?
		c, err = in.ReadByte()
		if err != nil {
			return err
		}
		if c != 'u' {
			return errors.Errorf("at %d expected null, found %c", in.pos, c)
		}

		c, err = in.ReadByte()
		if err != nil {
			return err
		}
		if c != 'l' {
			return errors.Errorf("at %d expected null, found %c", in.pos, c)
		}

		c, err = in.ReadByte()
		if err != nil {
			return err
		}
		if c != 'l' {
			return errors.Errorf("at %d expected null, found %c", in.pos, c)
		}

		// array has value 'null'
		return nil
	} else if c != '[' {
		return errors.Errorf("at %d expected '[', found %c", in.pos, c)
	}

	// scan the 1st element, or ']' if this is an empty list
	if c, err = scanPastWhitespace(in); err != nil {
		return err
	} else if c == ']' {
		return nil
	} else {
		// this is the 1st byte of the array element; put it back
		in.UnreadByte()
	}

	// scan each element
	for {
		if err = a.f.scan(in, out); err != nil {
			return err
		}

		if c, err = scanPastWhitespace(in); c == ',' {
			// ok, continue to the next element
		} else if c == ']' {
			return nil
		} else if err != nil {
			return err
		} else {
			return errors.Errorf("at %d expected ',' or ']'; found %c", in.pos, c)
		}
	}
}

func (d *dict) scan(in *reader, out io.Writer) error {
	// find the '{'
	var c byte
	var err error
	if err = scanWhitespaceToChar(in, '{'); err != nil {
		return err
	}

	for {
		// find the start of a key
		if err = scanWhitespaceToChar(in, '"'); err != nil {
			return err
		}

		var s []byte
		if s, err = scanString(in); string(s) != d.name { // go 1.10 compiler is smart enough to not copy the string in this cast+comparison
			if err != nil {
				return err
			}
			// skip ':' and the value
			if err = scanWhitespaceToChar(in, ':'); err != nil {
				return err
			}
			if err = skipValue(in); err != nil {
				return err
			}

		} else {
			// we found d.name
			if err = scanWhitespaceToChar(in, ':'); err != nil {
				return err
			}

			// scan the value
			if d.f != nil {
				d.f.scan(in, out)
			} else {
				// print the value
				if _, err = scanPastWhitespace(in); err != nil {
					return err
				}
				in.UnreadByte()

				// print the value
				var v = d.tmp
				if v, err = appendValue(in, v); err != nil {
					return err
				}
				v = append(v, '\n')
				d.tmp = v[:0] // save the slice for next time, since its capacity is probably right from here-on in, and we we won't have to zero it again

				if RAW && len(v) >= 3 && v[0] == '"' {
					v = unescapeString(v[1 : len(v)-2])
					v = append(v, '\n')
				}

				if PRETTY {
					var b bytes.Buffer
					err = json.Indent(&b, v, "", "  ")
					if err != nil {
						return err
					}
					v = b.Bytes()
				}

				if _, err = out.Write(v); err != nil {
					return err
				}
			}
		}

		if c, err = scanPastWhitespace(in); c == ',' {
			// continue to next name:value
		} else if c == '}' {
			return nil
		} else if err != nil {
			return err
		} else {
			return errors.Errorf("at %d expected ',' or '}'; found %c", in.pos, c)
		}
	}
}

func (f *fields) scan(in *reader, out io.Writer) error {
	// find the '{'
	var c byte
	var err error
	if err = scanWhitespaceToChar(in, '{'); err != nil {
		return err
	}

	for {
		// find the start of a key
		if err = scanWhitespaceToChar(in, '"'); err != nil {
			return err
		}

		var s []byte
		s, err = scanString(in)
		if err != nil {
			return err
		}
		n, ok := f.fields[string(s)]
		if !ok {
			// skip ':' and the value
			if err = scanWhitespaceToChar(in, ':'); err != nil {
				return err
			}
			if err = skipValue(in); err != nil {
				return err
			}

		} else {
			// we found the n'th field
			if err = scanWhitespaceToChar(in, ':'); err != nil {
				return err
			}

			// scan the value
			d := f.dicts[n]
			if d.f != nil {
				d.f.scan(in, out)
			} else {
				// print the value
				if _, err = scanPastWhitespace(in); err != nil {
					return err
				}
				in.UnreadByte()

				// print the value
				var v = d.tmp
				if v, err = appendValue(in, v); err != nil {
					return err
				}
				v = append(v, '\n')
				d.tmp = v[:0] // save the slice for next time, since its capacity is probably right from here-on in, and we we won't have to zero it again

				if RAW && len(v) >= 3 && v[0] == '"' {
					v = unescapeString(v[1 : len(v)-2])
					v = append(v, '\n')
				}

				if PRETTY {
					var b bytes.Buffer
					err = json.Indent(&b, v, "", "  ")
					if err != nil {
						return err
					}
					v = b.Bytes()
				}

				if _, err = out.Write(v); err != nil {
					return err
				}
			}
		}

		if c, err = scanPastWhitespace(in); c == ',' {
			// continue to next name:value
		} else if c == '}' {
			return nil
		} else if err != nil {
			return err
		} else {
			return errors.Errorf("at %d expected ',' or '}'; found %c", in.pos, c)
		}
	}
}

func (val *value) scan(in *reader, out io.Writer) error {
	var v = val.tmp
	var err error
	if v, err = appendValue(in, v); err != nil {
		return err
	}
	v = append(v, '\n')
	val.tmp = v[:0] // save the buffer for later

	if RAW && len(v) >= 3 && v[0] == '"' {
		v = unescapeString(v[1 : len(v)-2])
		v = append(v, '\n')
	}

	if PRETTY {
		var b bytes.Buffer
		err = json.Indent(&b, v, "", "  ")
		if err != nil {
			return err
		}
		v = b.Bytes()
	}

	if _, err = out.Write(v); err != nil {
		return err
	}
	return nil
}

// scan forward over whitespace until we find 'c', and stop
func scanWhitespaceToChar(in *reader, c byte) error {
	data, err := in.ReadSlice(c)
	if err == nil || (len(data) != 0 && data[len(data)-1] == c) {
		if len(data) > 1 {
			// verify that data[:len-1] contains only whitespace
			for _, x := range data[:len(data)-1] {
				if !isWhitespace(x) {
					return errors.Errorf("at %d expected '%c', found '%c'", in.pos, c, x)
				}
			}
		}
		return nil
	}
	return err
}

// scan forward over whitespace; return the first non-whitespace char
func scanPastWhitespace(in *reader) (c byte, err error) {
	for {
		c, err = in.ReadByte()
		if err != nil {
			return 0, err
		}
		if !isWhitespace(c) {
			return c, nil
		}
	}
}

func isWhitespace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t'
}

// scan and return the unescaped string. the opening '"' has been read
func scanString(in *reader) ([]byte, error) {
	data, err := in.ReadSlice('"')
	if err == nil && (len(data) < 2 || data[len(data)-2] != '\\') {
		// common case, the '"' terminates the string
		return unescapeString(data[:len(data)-1]), nil
	} else if err != nil {
		return nil, err
	}
	// the " might be escaped. or the \ might be from a \\ pair. we have to scan the entire data to know
	// IDEA: scan backwards and count how many \ in a row we find
	esc := false
	for _, c := range data[:len(data)-1] {
		if c == '\\' {
			esc = !esc
		} else {
			esc = false
		}
	}
	if !esc {
		// yup, the " isn't actually escaped
		return unescapeString(data[:len(data)-1]), nil
	}
	// the '"' is escaped. keep the '"' and keep reading
	data = append([]byte(nil), data...) // set cap so we can append safely
	for {
		j := len(data)
		data2, err := in.ReadSlice('"')
		if err != nil {
			return nil, err
		}
		data = append(data, data2[:len(data2)-1]...)
		if data[len(data)-1] != '\\' {
			return unescapeString(data), nil
		}
		esc := false
		for _, c := range data[j:] {
			if c == '\\' {
				esc = !esc
			} else {
				esc = false
			}
		}
		if !esc {
			// yup, the " isn't actually escaped
			return unescapeString(data), nil
		}
		data = append(data, '"')
	}
}

// append a escaped (\'s intact) string and the terminating '"' to value. the opening '"' has been read
func appendString(in *reader, value []byte) ([]byte, error) {
	for {
		data, err := in.ReadSlice('"')
		if err == nil && (len(data) < 2 || data[len(data)-2] != '\\') {
			// common case, the '"' terminates the string
			return append(value, data...), nil
		} else if err != nil {
			return value, err
		}
		// the " might be escaped. or the \ might be from a \\ pair. we have to scan the entire data to know
		// IDEA: scan backwards and count how many \ in a row we find
		esc := false
		for _, c := range data[:len(data)-1] {
			if c == '\\' {
				esc = !esc
			} else {
				esc = false
			}
		}
		if !esc {
			// yup, the " isn't actually escaped
			return append(value, data...), nil
		}
		// the '"' is escaped. keep the '"' and keep reading
		value = append(value, data...)
	}
}

func unescapeString(data []byte) []byte {
	i := bytes.IndexByte(data, '\\')
	if i == -1 {
		// common case, no escaping
		return data
	}

	for {
		// note: \ can't be right at the end b/c the callers checked for that already
		copy(data[i:], data[i+1:]) // O(n^2), but \ are usually rare
		data = data[:len(data)-1]
		j := bytes.IndexByte(data[i+1:], '\\')
		if j == -1 {
			return data
		}
		i = i + 1 + j
	}
}

// skip a string. the opening '"' has been read
func skipString(in *reader) error {
	for {
		data, err := in.ReadSlice('"')
		if err == nil && (len(data) < 2 || data[len(data)-2] != '\\') {
			// common case, the '"' terminates the string
			return nil
		} else if err != nil {
			return err
		}
		// the " might be escaped. or the \ might be from a \\ pair. we have to scan the entire data to know
		// IDEA: scan backwards and count how many \ in a row we find
		esc := false
		for _, c := range data[:len(data)-1] {
			if c == '\\' {
				esc = !esc
			} else {
				esc = false
			}
		}
		if !esc {
			// yup, the " isn't actually escaped
			return nil
		}
		// the '"' is escaped. keep reading until we find one which isn't escaped
	}
}

// skip the next value
func skipValue(in *reader) error {
	c, err := in.ReadByte()
	for err != nil || isWhitespace(c) {
		if err != nil {
			return err
		}
		c, err = in.ReadByte()
	}

	switch c {
	case '{':
		// skip name:values until the closing '}'
		first := true
		for {
			c, err = in.ReadByte()
			for err != nil || isWhitespace(c) {
				if err != nil {
					return err
				}
				c, err = in.ReadByte()
			}
			if c == '}' {
				return nil
			}
			if c == ',' {
				if first {
					return errors.Errorf("at %d expected a value, found '%c'", in.pos, c)
				}
			} else {
				in.UnreadByte()
			}
			first = false
			err = skipValue(in) // value ought to be a string, but we don't bother to check
			if err != nil {
				return err
			}

			c, err = in.ReadByte()
			for err != nil || isWhitespace(c) {
				if err != nil {
					return err
				}
				c, err = in.ReadByte()
			}

			if c != ':' {
				return errors.Errorf("at %d expected ':', found '%c'", in.pos, c)
			}
			err = skipValue(in)
			if err != nil {
				return err
			}
		}

	case '[':
		// skip values until the closing ']'
		first := true
		for {
			c, err = in.ReadByte()
			for err != nil || isWhitespace(c) {
				if err != nil {
					return err
				}
				c, err = in.ReadByte()
			}

			if c == ']' {
				return nil
			}
			if c == ',' {
				if first {
					return errors.Errorf("at %d expected a value, found '%c'", in.pos, c)
				}
			} else {
				in.UnreadByte()
			}
			first = false
			err = skipValue(in)
			if err != nil {
				return err
			}
		}

	case '"':
		// skip until the closing '""
		// this is the hot path in JSON skipping, so first try the common case where the first '"' is not escaped
		return skipString(in)

	default:
		// anything else is either a number or a keyword. we just skip until we find the first non-number/keyword char
		for {
			// the longest number is a double-precision float, 1+16+1+2+4 = 23 chars. So peeking ahead 32 bytes is enough
			// to find the end in the first iteration.
			buf, err := in.r.Peek(32)
			if err != nil && !((err == bufio.ErrBufferFull || err == io.EOF) && len(buf) != 0) {
				return err
			} // if it's a ErrBufferFull or EOF then buf is truncated and we want to process what we have peeked
			for i, c := range buf {
				switch {
				case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == 'E', c == '+', c == '-', c == '.': // keywords (true, false, null) are in lowercase ascii; no need to handle UTF-8. I do need to allow 'E' for the float format, since JSON allows both e and E
				default:
					// stop here
					_, err := in.Discard(i) // we know the discard succeeds b/c we just Peek()ed at least as much
					return err
				}
			}
			// if we get here then there are more than 32 chars in the keyword or number. weird. out of paranoia I'll support it
			_, err = in.Discard(len(buf))
			if err != nil {
				return err
			}
		}
	}
}

// scan and return the next value
func appendValue(in *reader, value []byte) ([]byte, error) {
	c, err := in.ReadByte()
	for err != nil || isWhitespace(c) {
		if err != nil {
			return value, err
		}
		c, err = in.ReadByte()
	}

	value = append(value, c)

	switch c {
	case '{':
		// scan name:values until the closing '}'
		first := true
		for {
			c, err = in.ReadByte()
			for err != nil || isWhitespace(c) {
				if err != nil {
					return value, err
				}
				c, err = in.ReadByte()
			}

			value = append(value, c)
			if c == '}' {
				return value, nil
			}
			if c == ',' {
				if first {
					return value, errors.Errorf("at %d expected a value, found '%c'", in.pos, c)
				}
			} else {
				in.UnreadByte()
				value = value[:len(value)-1]
			}
			first = false
			value, err = appendValue(in, value) // value better be a string, but we don't care
			if err != nil {
				return value, err
			}

			c, err = in.ReadByte()
			for err != nil || isWhitespace(c) {
				if err != nil {
					return value, err
				}
				c, err = in.ReadByte()
			}

			value = append(value, c)
			if c != ':' {
				return value, errors.Errorf("at %d expected ':', found '%c'", in.pos, c)
			}
			value, err = appendValue(in, value)
			if err != nil {
				return value, err
			}
		}

	case '[':
		// scan values until the closing ']'
		first := true
		for {
			c, err = in.ReadByte()
			for err != nil || isWhitespace(c) {
				if err != nil {
					return value, err
				}
				c, err = in.ReadByte()
			}

			value = append(value, c)
			if c == ']' {
				return value, nil
			}
			if c == ',' {
				if first {
					return value, errors.Errorf("at %d expected a value, found '%c'", in.pos, c)
				}
			} else {
				in.UnreadByte()
				value = value[:len(value)-1]
			}
			first = false
			value, err = appendValue(in, value)
			if err != nil {
				return value, err
			}
		}

	case '"':
		return appendString(in, value)

	default:
		// anything else is either a number or a keyword. we just scan until we find the first non-number/keyword char
		for {
			// the longest number is a double-precision float, 1+16+1+2+4 = 23 chars. So peeking ahead 32 bytes is enough
			// to find the end in the first iteration.
			buf, err := in.r.Peek(32)
			if err != nil && !((err == bufio.ErrBufferFull || err == io.EOF) && len(buf) != 0) {
				return value, err
			} // if it's a ErrBufferFull or EOF then buf is truncated and we want to process what we have peeked
			for i, c := range buf {
				switch {
				case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == 'E', c == '+', c == '-', c == '.': // keywords (true, false, null) are in lowercase ascii; no need to handle UTF-8. I do need to allow 'E' for the float format, since JSON allows both e and E
				default:
					// stop here
					value = append(value, buf[:i]...)
					_, err := in.Discard(i) // we know the discard succeeds b/c we just Peek()ed at least as much
					return value, err
				}
			}
			// if we get here then there are more than 32 chars in the keyword or number. weird. out of paranoia I'll support it
			value = append(value, buf...)
			_, err = in.Discard(len(buf))
			if err != nil {
				return value, err
			}
		}
	}
}

// ------------------------------------------------------------------------------------------------------
// a wrapper around bufio.Reader which counts the bytes read, so we can report where in the input we were when an error happened
type reader struct {
	r   *bufio.Reader
	pos int
}

func newReader(in io.Reader, size int) *reader {
	return &reader{
		r:   bufio.NewReaderSize(in, size),
		pos: 0,
	}
}

func (r *reader) ReadByte() (byte, error) {
	c, err := r.r.ReadByte()
	if err == nil {
		r.pos++
	}
	return c, err
}

func (r *reader) UnreadByte() error {
	err := r.r.UnreadByte()
	if err == nil {
		r.pos--
	}
	return err
}

func (r *reader) ReadSlice(delim byte) ([]byte, error) {
	d, err := r.r.ReadSlice(delim)
	r.pos += len(d)
	return d, err
}

func (r *reader) Discard(n int) (int, error) {
	a, err := r.r.Discard(n)
	r.pos += a
	return a, err
}

// -----------------------------------------------------------------------------------------------
