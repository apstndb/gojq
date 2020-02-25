package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/fatih/color"
	"github.com/itchyny/go-flags"
	"github.com/mattn/go-isatty"
	"gopkg.in/yaml.v3"

	"github.com/itchyny/gojq"
)

const name = "gojq"

const version = "0.7.0"

var revision = "HEAD"

const (
	exitCodeOK = iota
	exitCodeErr
)

type cli struct {
	inStream  io.Reader
	outStream io.Writer
	errStream io.Writer

	outputCompact bool
	outputRaw     bool
	outputJoin    bool
	outputYAML    bool
	inputRaw      bool
	inputSlurp    bool
	inputYAML     bool

	argnames  []string
	argvalues []interface{}

	outputYAMLSeparator bool
}

type flagopts struct {
	OutputCompact bool              `short:"c" long:"compact-output" description:"compact output"`
	OutputRaw     bool              `short:"r" long:"raw-output" description:"output raw strings"`
	OutputJoin    bool              `short:"j" long:"join-output" description:"stop printing a newline after each output"`
	OutputColor   bool              `short:"C" long:"color-output" description:"colorize output even if piped"`
	OutputMono    bool              `short:"M" long:"monochrome-output" description:"stop colorizing output"`
	OutputYAML    bool              `long:"yaml-output" description:"output by YAML"`
	InputNull     bool              `short:"n" long:"null-input" description:"use null as input value"`
	InputRaw      bool              `short:"R" long:"raw-input" description:"read input as raw strings"`
	InputSlurp    bool              `short:"s" long:"slurp" description:"read all inputs into an array"`
	InputYAML     bool              `long:"yaml-input" description:"read input as YAML"`
	FromFile      string            `short:"f" long:"from-file" description:"load query from file"`
	ModulePaths   []string          `short:"L" description:"directory to search modules from"`
	Args          map[string]string `long:"arg" description:"set variable to string value" count:"2" unquote:"false"`
	ArgsJSON      map[string]string `long:"argjson" description:"set variable to JSON value" count:"2" unquote:"false"`
	SlurpFile     map[string]string `long:"slurpfile" description:"set variable to the JSON contents of the file" count:"2" unquote:"false"`
	RawFile       map[string]string `long:"rawfile" description:"set variable to the contents of the file" count:"2" unquote:"false"`
	Version       bool              `short:"v" long:"version" description:"print version"`
}

var addDefaultModulePath = true

var argNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func (cli *cli) run(args []string) int {
	if err := cli.runInternal(args); err != nil {
		if errs, ok := err.(errors); ok {
			for _, err := range errs {
				fmt.Fprintf(cli.errStream, "%s: %s\n", name, err)
			}
		} else {
			fmt.Fprintf(cli.errStream, "%s: %s\n", name, err)
		}
		return exitCodeErr
	}
	return exitCodeOK
}

func (cli *cli) runInternal(args []string) error {
	var opts flagopts
	args, err := flags.NewParser(
		&opts, flags.HelpFlag|flags.PassDoubleDash,
	).ParseArgs(args)
	if err != nil {
		if err, ok := err.(*flags.Error); ok && err.Type == flags.ErrHelp {
			fmt.Fprintf(cli.outStream, `%[1]s - Go implementation of jq

Version: %s (rev: %s/%s)

Synopsis:
  %% echo '{"foo": 128}' | %[1]s '.foo'

`,
				name, version, revision, runtime.Version())
			fmt.Fprintln(cli.outStream, err.Error())
			return nil
		}
		return err
	}
	if opts.Version {
		fmt.Fprintf(cli.outStream, "%s %s (rev: %s/%s)\n", name, version, revision, runtime.Version())
		return nil
	}
	cli.outputCompact, cli.outputRaw, cli.outputJoin, cli.outputYAML =
		opts.OutputCompact, opts.OutputRaw, opts.OutputJoin, opts.OutputYAML
	if opts.OutputColor || opts.OutputMono {
		defer func(x bool) { color.NoColor = x }(color.NoColor)
		color.NoColor = opts.OutputMono
	} else {
		defer func(x bool) { color.NoColor = x }(color.NoColor)
		color.NoColor = !isTTY(cli.outStream)
	}
	cli.inputRaw, cli.inputSlurp, cli.inputYAML = opts.InputRaw, opts.InputSlurp, opts.InputYAML
	for k, v := range opts.Args {
		if !argNameRe.MatchString(k) {
			return &variableNameError{k}
		}
		cli.argnames = append(cli.argnames, "$"+k)
		cli.argvalues = append(cli.argvalues, v)
	}
	for k, v := range opts.ArgsJSON {
		if !argNameRe.MatchString(k) {
			return &variableNameError{k}
		}
		var val interface{}
		if err := json.Unmarshal([]byte(v), &val); err != nil {
			return &jsonParseError{"$" + k, v, err}
		}
		cli.argnames = append(cli.argnames, "$"+k)
		cli.argvalues = append(cli.argvalues, val)
	}
	for k, v := range opts.SlurpFile {
		if !argNameRe.MatchString(k) {
			return &variableNameError{k}
		}
		vals, err := slurpFile(v)
		if err != nil {
			return err
		}
		cli.argnames = append(cli.argnames, "$"+k)
		cli.argvalues = append(cli.argvalues, vals)
	}
	for k, v := range opts.RawFile {
		if !argNameRe.MatchString(k) {
			return &variableNameError{k}
		}
		val, err := ioutil.ReadFile(v)
		if err != nil {
			return err
		}
		cli.argnames = append(cli.argnames, "$"+k)
		cli.argvalues = append(cli.argvalues, string(val))
	}
	var arg, fname string
	if opts.FromFile != "" {
		src, err := ioutil.ReadFile(opts.FromFile)
		if err != nil {
			return err
		}
		arg, fname = string(src), opts.FromFile
	} else if len(args) == 0 {
		arg = "."
	} else {
		arg, fname = strings.TrimSpace(args[0]), "<arg>"
		args = args[1:]
	}
	query, err := gojq.Parse(arg)
	if err != nil {
		return &queryParseError{fname, arg, err}
	}
	modulePaths := opts.ModulePaths
	if len(modulePaths) == 0 && addDefaultModulePath {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		modulePaths = []string{filepath.Join(homeDir, ".jq")}
	}
	code, err := gojq.Compile(query,
		gojq.WithModuleLoader(&moduleLoader{modulePaths}),
		gojq.WithVariables(cli.argnames))
	if err != nil {
		return &compileError{err}
	}
	if opts.InputNull {
		cli.inputRaw, cli.inputSlurp = false, false
		return cli.process("<null>", bytes.NewReader([]byte("null")), code)
	}

	if len(args) == 0 {
		return cli.process("<stdin>", cli.inStream, code)
	}
	for _, arg := range args {
		if err := cli.processFile(arg, code); err != nil {
			return err
		}
	}
	return nil
}

func slurpFile(name string) ([]interface{}, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var vals []interface{}
	var buf bytes.Buffer
	dec := json.NewDecoder(io.TeeReader(f, &buf))
	for {
		var val interface{}
		if err := dec.Decode(&val); err != nil {
			if err == io.EOF {
				break
			}
			return nil, &jsonParseError{name, buf.String(), err}
		}
		vals = append(vals, val)
	}
	return vals, nil
}

func (cli *cli) processFile(fname string, code *gojq.Code) error {
	f, err := os.Open(fname)
	if err != nil {
		return err
	}
	defer f.Close()
	return cli.process(fname, f, code)
}

func (cli *cli) process(fname string, in io.Reader, code *gojq.Code) error {
	if cli.inputRaw {
		return cli.processRaw(fname, in, code)
	}
	if cli.inputYAML {
		return cli.processYAML(fname, in, code)
	}
	return cli.processJSON(fname, in, code)
}

func (cli *cli) processRaw(fname string, in io.Reader, code *gojq.Code) error {
	if cli.inputSlurp {
		xs, err := ioutil.ReadAll(in)
		if err != nil {
			return err
		}
		return cli.printValue(code.Run(string(xs), cli.argvalues...))
	}
	s := bufio.NewScanner(in)
	var errs errors
	for s.Scan() {
		if err := cli.printValue(code.Run(s.Text(), cli.argvalues...)); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.Err(); err != nil {
		return err
	}
	if errs != nil {
		return errs
	}
	return nil
}

func (cli *cli) processJSON(fname string, in io.Reader, code *gojq.Code) error {
	var buf bytes.Buffer
	dec := json.NewDecoder(io.TeeReader(in, &buf))
	dec.UseNumber()
	var vs []interface{}
	for {
		var v interface{}
		if err := dec.Decode(&v); err != nil {
			if err == io.EOF {
				if cli.inputSlurp {
					return cli.printValue(code.Run(vs, cli.argvalues...))
				}
				return nil
			}
			return &jsonParseError{fname, buf.String(), err}
		}
		if cli.inputSlurp {
			vs = append(vs, v)
			continue
		}
		if err := cli.printValue(code.Run(v, cli.argvalues...)); err != nil {
			return err
		}
	}
}

func (cli *cli) processYAML(fname string, in io.Reader, code *gojq.Code) error {
	var buf bytes.Buffer
	dec := yaml.NewDecoder(io.TeeReader(in, &buf))
	var vs []interface{}
	for {
		var v interface{}
		if err := dec.Decode(&v); err != nil {
			if err == io.EOF {
				if cli.inputSlurp {
					return cli.printValue(code.Run(vs, cli.argvalues...))
				}
				return nil
			}
			return &yamlParseError{fname, buf.String(), err}
		}
		v = fixMapKeyToString(v) // Workaround for https://github.com/go-yaml/yaml/issues/139
		if cli.inputSlurp {
			vs = append(vs, v)
			continue
		}
		if err := cli.printValue(code.Run(v, cli.argvalues...)); err != nil {
			return err
		}
	}
}

func (cli *cli) printValue(v gojq.Iter) error {
	m := cli.createMarshaler()
	for {
		m, outStream := m, cli.outStream
		x, ok := v.Next()
		if !ok {
			break
		}
		switch v := x.(type) {
		case error:
			return v
		case [2]interface{}:
			if s, ok := v[0].(string); ok {
				if s == "HALT:" {
					return nil
				}
				outStream = cli.errStream
				compact := cli.outputCompact
				cli.outputCompact = true
				m = cli.createMarshaler()
				cli.outputCompact = compact
				if s == "STDERR:" {
					x = v[1]
				}
			}
		}
		xs, err := m.Marshal(x)
		if err != nil {
			return err
		}
		if cli.outputYAMLSeparator {
			outStream.Write([]byte("---\n"))
		}
		outStream.Write(xs)
		cli.outputYAMLSeparator = cli.outputYAML
		if !cli.outputJoin && !cli.outputYAML {
			outStream.Write([]byte{'\n'})
		}
	}
	return nil
}

func (cli *cli) createMarshaler() marshaler {
	if cli.outputYAML {
		return yamlFormatter()
	}
	f := jsonFormatter()
	if cli.outputCompact {
		f.Indent = 0
		f.Newline = ""
	}
	if cli.outputRaw || cli.outputJoin {
		return &rawMarshaler{f}
	}
	return f
}

// isTTY attempts to determine whether an output is a TTY.
func isTTY(w io.Writer) bool {
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(interface{ Fd() uintptr })
	return ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd()))
}
