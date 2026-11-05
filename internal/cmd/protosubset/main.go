// Command protosubset cuts a pinned subset out of Substrate's ateapi.proto: the named methods of service Control,
// every message and enum they reach, and the annotation extensions. Nothing else is rewritten except go_package and,
// with -additions, the fields herd adds to upstream messages (ADR 0001, PLT-330).
//
//	protosubset -upstream testdata/upstream-ateapi.proto -methods subset.txt -additions herd-additions.txt \
//	    -source <url@commit> -go-package 'github.com/everdict/herd/api/ateapi/v1;ateapi' -out ateapi.proto
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

func main() {
	upstream := flag.String("upstream", "", "upstream ateapi.proto")
	methods := flag.String("methods", "", "file listing the Control methods to keep, one per line")
	additions := flag.String("additions", "", "file of message blocks whose fields are added to the subset's messages of the same name")
	source := flag.String("source", "", "where the upstream file came from (repository, path and commit)")
	goPackage := flag.String("go-package", "", "go_package option of the subset")
	out := flag.String("out", "", "output file")
	flag.Parse()
	if err := run(*upstream, *methods, *additions, *source, *goPackage, *out); err != nil {
		fmt.Fprintln(os.Stderr, "protosubset:", err)
		os.Exit(1)
	}
}

func run(upstreamPath, methodsPath, additionsPath, source, goPackage, out string) error {
	src, err := os.ReadFile(upstreamPath)
	if err != nil {
		return err
	}
	list, err := os.ReadFile(methodsPath)
	if err != nil {
		return err
	}
	var add []byte
	if additionsPath != "" {
		if add, err = os.ReadFile(additionsPath); err != nil {
			return err
		}
	}
	b, err := Subset(src, ParseMethods(list), add, source, goPackage)
	if err != nil {
		return err
	}
	return os.WriteFile(out, b, 0o644)
}

// ParseMethods reads one method name per line; blank lines and # comments are skipped.
func ParseMethods(list []byte) []string {
	var out []string
	s := bufio.NewScanner(bytes.NewReader(list))
	for s.Scan() {
		l := strings.TrimSpace(s.Text())
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

type block struct {
	kind, name string
	lines      []string // leading comments included
}

var (
	declRE  = regexp.MustCompile(`^(service|message|enum|extend)\s+([A-Za-z_][A-Za-z0-9_.]*)\s*\{`)
	rpcRE   = regexp.MustCompile(`^\s*rpc\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	identRE = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.]*`)
	strRE   = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
)

// code strips string literals and the trailing // comment of a line.
func code(l string) string {
	l = strRE.ReplaceAllString(l, `""`)
	if i := strings.Index(l, "//"); i >= 0 {
		l = l[:i]
	}
	return l
}

func depthDelta(l string) int {
	c := code(l)
	return strings.Count(c, "{") - strings.Count(c, "}")
}

// parse splits a proto file into its preamble (everything before the first declaration's leading comments) and its
// top-level declarations.
func parse(src []byte) (preamble []string, blocks []block, err error) {
	lines := strings.Split(strings.TrimRight(string(src), "\n"), "\n")
	var pending []string
	first := true
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		m := declRE.FindStringSubmatch(l)
		if m == nil {
			switch {
			case strings.HasPrefix(strings.TrimSpace(l), "//"):
				pending = append(pending, l)
			case strings.TrimSpace(l) == "":
				if first {
					preamble = append(preamble, pending...)
					preamble = append(preamble, l)
				}
				pending = nil
			case first:
				preamble = append(preamble, pending...)
				preamble = append(preamble, l)
				pending = nil
			default:
				return nil, nil, fmt.Errorf("line %d: unexpected top-level line %q", i+1, l)
			}
			continue
		}
		first = false
		b := block{kind: m[1], name: m[2], lines: append(pending, l)}
		pending = nil
		depth := depthDelta(l)
		for depth > 0 {
			i++
			if i >= len(lines) {
				return nil, nil, fmt.Errorf("%s %s: unterminated", b.kind, b.name)
			}
			b.lines = append(b.lines, lines[i])
			depth += depthDelta(lines[i])
		}
		blocks = append(blocks, b)
	}
	return preamble, blocks, nil
}

// filterService keeps the service's own lines and the rpc entries named in keep, with their leading comments.
func filterService(b block, keep map[string]bool) (block, error) {
	head, body := []string{}, b.lines
	for len(body) > 0 && !declRE.MatchString(body[0]) {
		head, body = append(head, body[0]), body[1:]
	}
	if len(body) < 2 {
		return b, fmt.Errorf("service %s: empty", b.name)
	}
	head = append(head, body[0])
	inner, closing := body[1:len(body)-1], body[len(body)-1]
	out := block{kind: b.kind, name: b.name, lines: head}
	seen := map[string]bool{}
	var entry []string
	for i := 0; i < len(inner); i++ {
		l := inner[i]
		m := rpcRE.FindStringSubmatch(l)
		if m == nil {
			if strings.TrimSpace(l) == "" {
				entry = nil
			} else {
				entry = append(entry, l)
			}
			continue
		}
		entry = append(entry, l)
		depth := depthDelta(l)
		for depth > 0 {
			i++
			if i >= len(inner) {
				return b, fmt.Errorf("rpc %s: unterminated", m[1])
			}
			entry = append(entry, inner[i])
			depth += depthDelta(inner[i])
		}
		if keep[m[1]] {
			if len(out.lines) > len(head) {
				out.lines = append(out.lines, "")
			}
			out.lines = append(out.lines, entry...)
			seen[m[1]] = true
		}
		entry = nil
	}
	var missing []string
	for name := range keep {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return b, fmt.Errorf("service %s has no method %s", b.name, strings.Join(missing, ", "))
	}
	out.lines = append(out.lines, closing)
	return out, nil
}

// Subset returns the subset proto. additions, if not empty, holds message blocks (comments outside them are skipped):
// the lines inside each are appended to the subset's message of the same name, which the subset must reach.
func Subset(src []byte, methods []string, additions []byte, source, goPackage string) ([]byte, error) {
	if len(methods) == 0 {
		return nil, errors.New("no methods to keep")
	}
	preamble, blocks, err := parse(src)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, m := range methods {
		keep[m] = true
	}
	byName := map[string]int{}
	for i, b := range blocks {
		if b.kind != "extend" {
			byName[b.name] = i
		}
	}
	ci, ok := byName["Control"]
	if !ok || blocks[ci].kind != "service" {
		return nil, errors.New("no service Control")
	}
	if blocks[ci], err = filterService(blocks[ci], keep); err != nil {
		return nil, err
	}
	reached := map[int]bool{}
	var queue []int
	visit := func(i int) {
		if !reached[i] {
			reached[i] = true
			queue = append(queue, i)
		}
	}
	visit(ci)
	for i, b := range blocks {
		if b.kind == "extend" {
			visit(i)
		}
	}
	for len(queue) > 0 {
		b := blocks[queue[0]]
		queue = queue[1:]
		for _, l := range b.lines {
			for _, id := range identRE.FindAllString(code(l), -1) {
				id = strings.TrimPrefix(id, ".")
				id = id[strings.LastIndex(id, ".")+1:]
				if j, ok := byName[id]; ok {
					visit(j)
				}
			}
		}
	}
	herd := len(bytes.TrimSpace(additions)) > 0
	if herd {
		if err := addFields(blocks, byName, reached, additions); err != nil {
			return nil, err
		}
	}

	var out bytes.Buffer
	notice := []string{
		"// herd: vendored subset of Substrate's Control API (spec herd.md R-D2).",
		"// Source: " + source + ".",
		"// Modified by herd: cut down by internal/cmd/protosubset to the Control methods in subset.txt and the",
		"// messages and enums they reach; go_package rewritten. Nothing else is changed. Do not edit; run",
		"// `go generate ./api/...`.",
	}
	if herd {
		notice[3] = "// messages and enums they reach; go_package rewritten; the fields of herd-additions.txt appended to their"
		notice[4] = "// messages (each says \"herd addition\"). Nothing else is changed. Do not edit; run `go generate ./api/...`."
	}
	licenseDone := false
	for _, l := range preamble {
		if !licenseDone && strings.TrimSpace(l) == "" {
			out.WriteString("\n" + strings.Join(notice, "\n") + "\n")
			licenseDone = true
		}
		if strings.HasPrefix(strings.TrimSpace(l), "option go_package") {
			l = fmt.Sprintf("option go_package = %q;", goPackage)
		}
		out.WriteString(l + "\n")
	}
	if !licenseDone {
		return nil, errors.New("upstream has no license header")
	}
	for i, b := range blocks {
		if reached[i] {
			out.WriteString(strings.Join(b.lines, "\n") + "\n\n")
		}
	}
	return append(bytes.TrimRight(out.Bytes(), "\n"), '\n'), nil
}

// addFields appends the lines inside each message block of additions to the reached message of the same name.
func addFields(blocks []block, byName map[string]int, reached map[int]bool, additions []byte) error {
	_, extra, err := parse(additions)
	if err != nil {
		return fmt.Errorf("additions: %w", err)
	}
	for _, e := range extra {
		i, ok := byName[e.name]
		if e.kind != "message" || !ok || blocks[i].kind != "message" || !reached[i] {
			return fmt.Errorf("additions: %s %s is not a message of the subset", e.kind, e.name)
		}
		body := e.lines
		for len(body) > 0 && !declRE.MatchString(body[0]) {
			body = body[1:]
		}
		if len(body) < 3 {
			return fmt.Errorf("additions: message %s adds nothing", e.name)
		}
		b := blocks[i].lines
		lines := append(append([]string{}, b[:len(b)-1]...), "")
		lines = append(lines, body[1:len(body)-1]...)
		blocks[i].lines = append(lines, b[len(b)-1])
	}
	return nil
}
