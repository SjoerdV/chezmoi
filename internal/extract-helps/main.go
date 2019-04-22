package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"text/template"

	"github.com/kr/text"
	"github.com/russross/blackfriday/v2"
)

var (
	debug      = flag.Bool("debug", false, "debug")
	inputFile  = flag.String("i", "", "input file")
	outputFile = flag.String("o", "", "output file")
	width      = flag.Int("width", 80, "width")

	funcs = template.FuncMap{
		"printMultiLineString": printMultiLineString,
	}

	outputTemplate = template.Must(template.New("output").Funcs(funcs).Parse(`//go:generate go run github.com/twpayne/chezmoi/internal/extract-helps -i {{ .InputFile }} -o {{ .OutputFile }}

package cmd

type help struct {
	long    string
	example string
}

var helps = map[string]help{
{{- range $command, $help := .Helps }}
	"{{ $command }}": help{
{{- if $help.Example }}
		long:    {{ printMultiLineString $help.Long "\t\t\t" }},
		example: {{ printMultiLineString $help.Example "\t\t\t" }},
{{- else }}
		long: {{ printMultiLineString $help.Long "\t\t\t" }},
{{- end }}
	},
{{- end }}
}
`))
	debugTemplate = template.Must(template.New("debug").Parse(`
InputFile: {{ .InputFile }}
OuputFile: {{ .OutputFile }}

{{- range $command, $help := .Helps -}}
# {{ $command }}
{{ $help.Long }}

Examples:
{{ $help.Example }}

{{ end -}}
`))

	doubleQuote = []byte("\"")
	indent      = []byte("  ")
	newline     = []byte("\n")
	space       = []byte(" ")
	tab         = []byte("\t")

	renderers = map[blackfriday.NodeType]func(io.Writer, *blackfriday.Node) error{
		blackfriday.Heading:   renderHeading,
		blackfriday.CodeBlock: renderCodeBlock,
		blackfriday.Paragraph: renderParagraph,
		blackfriday.Table:     renderTable,
	}
)

type help struct {
	Long    string
	Example string
}

type errUnsupportedNodeType blackfriday.NodeType

func (e errUnsupportedNodeType) Error() string {
	return fmt.Sprintf("unsupported node type: %s", e)
}

func printMultiLineString(s, indent string) string {
	if len(s) == 0 {
		return `""`
	}
	b := &strings.Builder{}
	b.WriteString("\"\" +\n" + indent)
	for i, line := range strings.SplitAfter(s, "\n") {
		if line == "" {
			continue
		}
		if i != 0 {
			b.WriteString(" +\n" + indent)
		}
		fmt.Fprintf(b, "%q", line)
	}
	return b.String()
}

func literalText(node *blackfriday.Node) ([]byte, error) {
	b := &bytes.Buffer{}
	var err error
	node.Walk(func(node *blackfriday.Node, entering bool) blackfriday.WalkStatus {
		switch node.Type {
		case blackfriday.Code:
			if _, err = b.Write(doubleQuote); err != nil {
				return blackfriday.Terminate
			}
			if _, err = b.Write(bytes.ReplaceAll(node.Literal, newline, space)); err != nil {
				return blackfriday.Terminate
			}
			if _, err = b.Write(doubleQuote); err != nil {
				return blackfriday.Terminate
			}
		case blackfriday.Text:
			if _, err = b.Write(bytes.ReplaceAll(node.Literal, newline, space)); err != nil {
				return blackfriday.Terminate
			}
		}
		return blackfriday.GoToNext
	})
	return b.Bytes(), err
}

func renderCodeBlock(w io.Writer, codeBlock *blackfriday.Node) error {
	if codeBlock.Type != blackfriday.CodeBlock {
		return errUnsupportedNodeType(codeBlock.Type)
	}
	return renderIndented(w, codeBlock.Literal)
}

func renderExample(start, end *blackfriday.Node) (string, error) {
	s, err := render(start, end)
	return strings.TrimSuffix(s, "\n"+string(indent)), err
}

func renderHeading(w io.Writer, heading *blackfriday.Node) error {
	if heading.Type != blackfriday.Heading {
		return errUnsupportedNodeType(heading.Type)
	}
	t, err := literalText(heading)
	if err != nil {
		return err
	}
	if _, err := w.Write(t); err != nil {
		return err
	}
	_, err = w.Write(newline)
	return err
}

func renderIndented(w io.Writer, b []byte) error {
	for _, line := range bytes.SplitAfter(b, newline) {
		if _, err := w.Write(indent); err != nil {
			return err
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
	}
	return nil
}

func renderLong(start, end *blackfriday.Node) (string, error) {
	return render(start, end)
}

func renderParagraph(w io.Writer, paragraph *blackfriday.Node) error {
	if paragraph.Type != blackfriday.Paragraph {
		return errUnsupportedNodeType(paragraph.Type)
	}
	t, err := literalText(paragraph)
	if err != nil {
		return err
	}
	if _, err := w.Write(text.WrapBytes(t, *width)); err != nil {
		return err
	}
	_, err = w.Write(newline)
	return err
}

func renderTable(w io.Writer, table *blackfriday.Node) error {
	if table.Type != blackfriday.Table {
		return errUnsupportedNodeType(table.Type)
	}
	b := &bytes.Buffer{}
	tw := tabwriter.NewWriter(b, 0, 8, 1, ' ', 0)
	for rowGroup := table.FirstChild; rowGroup != nil; rowGroup = rowGroup.Next {
		if rowGroup.Type != blackfriday.TableHead && rowGroup.Type != blackfriday.TableBody {
			return errUnsupportedNodeType(rowGroup.Type)
		}
		for row := rowGroup.FirstChild; row != nil; row = row.Next {
			if row.Type != blackfriday.TableRow {
				return errUnsupportedNodeType(row.Type)
			}
			for cell := row.FirstChild; cell != nil; cell = cell.Next {
				if cell.Type != blackfriday.TableCell {
					return errUnsupportedNodeType(cell.Type)
				}
				t, err := literalText(cell)
				if err != nil {
					return err
				}
				if _, err := tw.Write(t); err != nil {
					return err
				}
				if _, err := tw.Write(tab); err != nil {
					return err
				}
			}
			if _, err := tw.Write(newline); err != nil {
				return err
			}
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return renderIndented(w, b.Bytes())
}

func render(start, end *blackfriday.Node) (string, error) {
	b := &bytes.Buffer{}
	for node := start; node != nil && node != end; node = node.Next {
		if node != start {
			if _, err := b.Write(newline); err != nil {
				return "", err
			}
		}
		renderer, ok := renderers[node.Type]
		if !ok {
			return "", errUnsupportedNodeType(node.Type)
		}
		if err := renderer(b, node); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

func extractHelps(r io.Reader) (map[string]*help, error) {
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	root := blackfriday.New(blackfriday.WithExtensions(blackfriday.Tables)).Parse(data)
	var commandsNode *blackfriday.Node
	for node := root.FirstChild; node != nil; node = node.Next {
		if node.Type == blackfriday.Heading &&
			node.HeadingData.Level == 2 &&
			node.FirstChild != nil &&
			node.FirstChild.Type == blackfriday.Text &&
			bytes.Equal(node.FirstChild.Literal, []byte("Commands")) {
			commandsNode = node
			break
		}
	}
	if commandsNode == nil {
		return nil, errors.New("cannot find \"Commands\" node")
	}
	var endCommandsNode *blackfriday.Node
	for node := commandsNode.Next; node != nil; node = node.Next {
		if node.Type == blackfriday.Heading && node.HeadingData.Level <= 2 {
			endCommandsNode = node
			break
		}
	}
	if endCommandsNode == nil {
		return nil, errors.New("cannot find end \"Commands\" node")
	}

	helps := make(map[string]*help)
	state := 0
	var h *help
	var start *blackfriday.Node
	for node := commandsNode.Next; node != endCommandsNode; node = node.Next {
		switch {
		case node.Type == blackfriday.Heading &&
			node.HeadingData.Level < 3:
			break
		case node.Type == blackfriday.Heading &&
			node.HeadingData.Level == 3 &&
			node.FirstChild != nil &&
			node.FirstChild.Type == blackfriday.Text &&
			node.FirstChild.Next != nil &&
			node.FirstChild.Next.Type == blackfriday.Code:
			switch state {
			case 1:
				if h.Long, err = renderLong(start, node); err != nil {
					return nil, err
				}
			case 2:
				if h.Example, err = renderExample(start, node); err != nil {
					return nil, err
				}
			}
			command := string(node.FirstChild.Next.Literal)
			var ok bool
			h, ok = helps[command]
			if !ok {
				h = &help{}
				helps[command] = h
			}
			start = node.Next
			state = 1
		case node.Type == blackfriday.Heading &&
			node.HeadingData.Level == 4 &&
			node.FirstChild != nil &&
			node.FirstChild.Type == blackfriday.Text &&
			node.FirstChild.Next != nil &&
			node.FirstChild.Next.Type == blackfriday.Code &&
			node.FirstChild.Next.Next != nil &&
			node.FirstChild.Next.Next.Type == blackfriday.Text &&
			bytes.Equal(node.FirstChild.Next.Next.Literal, []byte(" examples")):
			switch state {
			case 1:
				if h.Long, err = renderLong(start, node); err != nil {
					return nil, err
				}
			case 2:
				if h.Example, err = renderExample(start, node); err != nil {
					return nil, err
				}
			}
			command := string(node.FirstChild.Next.Literal)
			var ok bool
			h, ok = helps[command]
			if !ok {
				h = &help{}
				helps[command] = h
			}
			start = node.Next
			state = 2
		}
	}
	switch state {
	case 1:
		if h.Long, err = renderLong(start, endCommandsNode); err != nil {
			return nil, err
		}
	case 2:
		if h.Example, err = renderExample(start, endCommandsNode); err != nil {
			return nil, err
		}
	}
	return helps, err
}

func run() error {
	flag.Parse()

	var r io.Reader
	if *inputFile == "" {
		r = os.Stdin
	} else {
		fr, err := os.Open(*inputFile)
		if err != nil {
			return err
		}
		defer fr.Close()
		r = fr
	}

	helps, err := extractHelps(r)
	if err != nil {
		return err
	}

	var w io.Writer
	if *outputFile == "" {
		w = os.Stdout
	} else {
		fw, err := os.Create(*outputFile)
		if err != nil {
			return err
		}
		defer fw.Close()
		w = fw
	}

	data := struct {
		Helps      map[string]*help
		InputFile  string
		OutputFile string
	}{
		Helps:      helps,
		InputFile:  *inputFile,
		OutputFile: *outputFile,
	}

	if *debug {
		return debugTemplate.ExecuteTemplate(w, "debug", data)
	}

	buf := &bytes.Buffer{}
	if err := outputTemplate.ExecuteTemplate(buf, "output", data); err != nil {
		return err
	}

	cmd := exec.Command("gofmt", "-s")
	cmd.Stdin = buf
	cmd.Stdout = w
	return cmd.Run()
}

func main() {
	if err := run(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
