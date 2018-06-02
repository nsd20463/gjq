// gjq is a simpler but (better be) faster alternative to jq for extracting fields from JSON input
//
// The amount of time and CPU it takes jq to do simple field extractions is impacting my life.
// This is a replacement which ought to run faster.
// Of course it doesn't support more than a fraction of what jq does. On the other hand it supports
// just what I use jq most often for when processing millions of records.
//
// Copyright 2018 Nicolas S. Dade

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"reflect"
	"strings"
)

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) != 1 {
		log.Printf("1 filter argument required")
		os.Exit(1)
	}

	filter, err := makeFilter(args[0])
	if err != nil {
		log.Printf("Can't understand filter arguments: %s\n", err)
		os.Exit(1)
	}

	out := io.Writer(os.Stdout)
	in := io.Reader(os.Stdin)
	dec := json.NewDecoder(in)
	rec_num := 0
	for {
		rec_num++
		v := reflect.New(filter.typeof())
		err := dec.Decode(v.Interface())
		if err != nil {
			log.Printf("Can't decode record %d of input: %s\n", rec_num, err)
			os.Exit(1)
		}

		filter.filter(v.Elem(), out)
	}
}

type filter interface {
	filter(reflect.Value, io.Writer) error
	typeof() reflect.Type
}

func makeFilter(arg string) (filter, error) {
	// parse the filter string
	// we don't handle the entire world. We handle
	//   .X    ... extract element X from a dict
	//   []    ... extract all elements of an array
	// and for this 1st pass, this is all. I'll add more as I need them

	if len(arg) == 0 {
		return nil, fmt.Errorf("can't parse filter %q: expected an element, found \"\"", arg)
	}

	switch arg[0] {
	default:
		return nil, fmt.Errorf("can't parse filter %q: unknown operator '%c'", arg, arg[0])
	case '.':
		var field string
		var err error
		field, arg, err = extractFieldName(arg[1:])
		if err != nil {
			return nil, err
		}

		var field_type reflect.Type
		var f filter
		if arg != "" {
			// this is the innermost field
			field_type = reflect.TypeOf(json.RawMessage{})
		} else {
			f, err = makeFilter(arg)
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
			t: reflect.StructOf([]reflect.StructField{
				reflect.StructField{
					Name: field_name,
					Type: field_type,
					Tag:  reflect.StructTag(field_tag),
				}}),
			f: f,
		}, nil

	case '[':
		if len(arg) < 2 || arg[1] != ']' {
			return nil, fmt.Errorf("can't parse filter %q: expected ']' after '['", arg)
		}
		arg = arg[2:]

		f, err := makeFilter(arg)
		if err != nil {
			return nil, err
		}

		return &array{
			t: reflect.SliceOf(f.typeof()),
			f: f,
		}, nil
	}

	return nil, nil
}

func extractFieldName(arg string) (field string, remaining_arg string, err error) {
	// scan forward until we find a non-field char
	for i, c := range arg {
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', 'A' <= c && c <= 'Z', c == '_':
			// great, keep accumulating
		default:
			// end of field name
			if i == 0 {
				return "", arg, fmt.Errorf("Expacted field name, found %q", arg)
			}
			return arg[:i], arg[i:], nil
		}
	}
	// the entire arg is the field name
	return arg, "", nil
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
	t reflect.Type // the struct type
	f filter       // the field type, or nil if this is the leaf
}

func (d *dict) typeof() reflect.Type { return d.t }
func (d *dict) filter(in reflect.Value, out io.Writer) error {
	v := in.Field(0)
	if d.f != nil {
		return d.f.filter(v, out)
	}

	// we're the leaf. we print v
	_, err := out.Write([]byte(fmt.Sprintf("%s\n", v.Interface())))

	return err
}
