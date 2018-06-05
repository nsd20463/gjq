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
	"runtime"
	"runtime/pprof"
	"strings"

	"github.com/pkg/errors"
)

const debug = false

var LF = []byte{'\n'}

func main() {
	var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")
	var stdlib = flag.Bool("stdlib", false, "use stdlib encoding/json")
	var read_buf_size = flag.Int("buf", 64*1024, "size of input I/O buffer") // experiments show >64kB buffers is, strangely, counter-productive

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

	filter, err := makeFilter(args[0], 0)
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
	filter(reflect.Value, io.Writer) error
	typeof() reflect.Type
	scan(*reader, io.Writer) error
}

func makeFilter(arg string, pos int) (filter, error) {
	// parse the arg string
	// we don't handle the entire world. We handle
	//   .     ... extract a entire dict
	//   .X    ... extract element X from a dict
	//   []    ... extract all elements of an array
	// and for this 1st pass, this is all. I'll add more as I need them

	if len(arg) == pos {
		return nil, errors.Errorf("can't parse %q: expected an element, found the end", arg)
	}

	switch arg[pos] {
	default:
		return nil, errors.Errorf("can't parse %q at index %d: unknown operator '%c'", arg, pos, arg[pos])
	case '.':
		var field string
		var err error
		if pos+1 == len(arg) {
			// '.' terminates the filter. we are to return the entire dict
			return &dict{
				t: reflect.TypeOf(json.RawMessage{}),
			}, nil
		} else if field, pos, err = extractFieldName(arg, pos+1); err != nil {
			return nil, err
		} else {
			var field_type reflect.Type
			var f filter
			if len(arg) == pos {
				// this is the innermost field
				field_type = reflect.TypeOf(json.RawMessage{})
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
				name: field_name,
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

type array struct {
	t reflect.Type // a slice type
	f filter       // element type
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

type dict struct {
	name string       // the field name, or "" if we are to output the entire dict
	t    reflect.Type // the struct type, or the json.RawMessage type if we're outputting the entire dict
	f    filter       // the field type, or nil if this is the leaf
	tmp  []byte       // tmp buffer with (eventually) an appropriate capacity. avoids append reallocs and gc work
}

func (d *dict) typeof() reflect.Type { return d.t }
func (d *dict) filter(in reflect.Value, out io.Writer) error {
	var err error
	if d.name != "" {
		v := in.Field(0)
		if d.f != nil {
			return d.f.filter(v, out)
		}

		// we're the leaf. we print v
		_, err = out.Write([]byte(fmt.Sprintf("%s\n", v.Interface())))
	} else {
		// print the entire dict
		_, err = out.Write([]byte(fmt.Sprintf("%s\n", in.Interface())))
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

	if d.name == "" {
		// we're matching the entire dict
		in.UnreadByte()
		var v = d.tmp
		if v, err = appendValue(in, v); err != nil {
			return err
		}
		if _, err = out.Write(v); err != nil {
			return err
		}
		d.tmp = v[:0] // save the slice for next time, since its capacity is probably right from here-on in, and we we won't have to zero it again
		if _, err = out.Write(LF); err != nil {
			return err
		}
		return nil
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
				if _, err = out.Write(v); err != nil {
					return err
				}
				d.tmp = v[:0] // save the slice for next time, since its capacity is probably right from here-on in, and we we won't have to zero it again
				if _, err = out.Write(LF); err != nil {
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
	// TODO unicode!
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
	// TODO unicode!
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
	// TODO unicode!
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
	c, err := scanPastWhitespace(in)
	if err != nil {
		return err
	}

	switch c {
	case '{':
		// skip name:values until the closing '}'
		first := true
		for {
			c, err := scanPastWhitespace(in)
			if err != nil {
				return err
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
			c, err = scanPastWhitespace(in)
			if err != nil {
				return err
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
			c, err := scanPastWhitespace(in)
			if err != nil {
				return err
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
			c, err = in.ReadByte()
			if err != nil {
				return err
			}
			switch {
			case 'a' <= c && c <= 'z', '0' <= c && c <= '9', 'A' <= c && c <= 'Z', c == '_', c == '+', c == '-', c == '.':
				continue
			default:
				in.UnreadByte()
				return nil
			}
		}
	}
}

// scan and return the next value
func appendValue(in *reader, value []byte) ([]byte, error) {
	c, err := scanPastWhitespace(in)
	if err != nil {
		return value, err
	}
	value = append(value, c)

	switch c {
	case '{':
		// scan name:values until the closing '}'
		first := true
		for {
			c, err := scanPastWhitespace(in)
			if err != nil {
				return value, err
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
			c, err = scanPastWhitespace(in)
			if err != nil {
				return value, err
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
			c, err := scanPastWhitespace(in)
			if err != nil {
				return value, err
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
			c, err = in.ReadByte()
			if err != nil {
				return value, err
			}
			switch {
			case 'a' <= c && c <= 'z', '0' <= c && c <= '9', 'A' <= c && c <= 'Z', c == '_', c == '+', c == '-', c == '.':
				value = append(value, c)
				continue
			default:
				in.UnreadByte()
				return value, nil
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
		if debug {
			_, _, line1, _ := runtime.Caller(1)
			_, _, line2, _ := runtime.Caller(2)
			log.Printf("%d:%d ReadByte() -> %c", line1, line2, c)
		}
		r.pos++
	}
	return c, err
}

func (r *reader) UnreadByte() error {
	err := r.r.UnreadByte()
	if err == nil {
		if debug {
			_, _, line1, _ := runtime.Caller(1)
			_, _, line2, _ := runtime.Caller(2)
			log.Printf("%d:%d UnreadByte()", line1, line2)
		}
		r.pos--
	}
	return err
}

func (r *reader) ReadSlice(delim byte) ([]byte, error) {
	d, err := r.r.ReadSlice(delim)
	r.pos += len(d)
	if err == nil {
		if debug {
			_, _, line1, _ := runtime.Caller(1)
			_, _, line2, _ := runtime.Caller(2)
			log.Printf("%d:%d ReadSlice() -> [%d] %q", line1, line2, len(d), d)
		}
	}
	return d, err
}

// -----------------------------------------------------------------------------------------------
