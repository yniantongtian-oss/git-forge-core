package main

// GitHub GraphQL v4 at POST /api/graphql: a small spec-shaped engine (lexer, parser, schema, introspection,
// validation, executor) with the schema gh and github-mcp-server rely on. Resolvers read the same store as REST
// and MCP. Queries run under storeMu.RLock; mutations (and documents that ask for `mergeable`, which stores
// merged trees) under storeMu.Lock. Results are ordered objects, so field order follows the query.

import (
	"encoding/base64"
	"errors"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// lazyField is an introspection value computed on access (a function-valued property in the TS engine). It is
// omitted from serialized objects, as JSON.stringify omits functions.
type lazyField func(args map[string]any) any

// ---------------------------------------------------------------- lexer

type gTok struct {
	t    byte // 'p' punct, 'n' name, 'i' int, 'f' float, 's' string, 'e' eof
	v    string
	line int
	col  int
}

type gSyntaxError struct {
	msg  string
	line int
	col  int
}

func gAt(line, col int) string {
	return "[" + strconv.Itoa(line) + ", " + strconv.Itoa(col) + "]"
}

func isASCIILetter(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }
func isDigitRune(r rune) bool   { return r >= '0' && r <= '9' }

func gLex(text string) ([]gTok, *gSyntaxError) {
	src := []rune(text)
	n := len(src)
	toks := make([]gTok, 0, len(src)/3+1)
	i, line, lineStart := 0, 1, 0
	for i < n {
		ch := src[i]
		if ch == '\n' {
			line++
			i++
			lineStart = i
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\r' || ch == ',' || ch == 0xfeff {
			i++
			continue
		}
		if ch == '#' {
			for i < n && src[i] != '\n' && src[i] != '\r' {
				i++
			}
			continue
		}
		col := i - lineStart + 1
		switch {
		case ch == '.' && i+2 < n && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gTok{'p', "...", line, col})
			i += 3
		case strings.ContainsRune("!$&():=@[]{}|", ch):
			toks = append(toks, gTok{'p', string(ch), line, col})
			i++
		case ch == '_' || isASCIILetter(ch):
			j := i + 1
			for j < n && (src[j] == '_' || isASCIILetter(src[j]) || isDigitRune(src[j])) {
				j++
			}
			toks = append(toks, gTok{'n', string(src[i:j]), line, col})
			i = j
		case ch == '-' || isDigitRune(ch):
			end, float := gNumber(src, i)
			if end < 0 {
				return nil, &gSyntaxError{`Parse error on "` + string(ch) + `" at ` + gAt(line, col), line, col}
			}
			kind := byte('i')
			if float {
				kind = 'f'
			}
			toks = append(toks, gTok{kind, string(src[i:end]), line, col})
			i = end
		case ch == '"':
			if i+2 < n && src[i+1] == '"' && src[i+2] == '"' {
				closeAt := gIndexRunes(src, i+3, `"""`)
				if closeAt < 0 {
					return nil, &gSyntaxError{"Unterminated block string at " + gAt(line, col), line, col}
				}
				raw := strings.ReplaceAll(string(src[i+3:closeAt]), `\"""`, `"""`)
				raw = strings.TrimPrefix(raw, "\n")
				toks = append(toks, gTok{'s', gTrimBlockTail(raw), line, col})
				for _, r := range src[i : closeAt+3] {
					if r == '\n' {
						line++
					}
				}
				i = closeAt + 3
			} else {
				value, end, ok := gString(src, i)
				if !ok {
					return nil, &gSyntaxError{"Unterminated string at " + gAt(line, col), line, col}
				}
				toks = append(toks, gTok{'s', value, line, col})
				i = end
			}
		default:
			return nil, &gSyntaxError{`Parse error on "` + string(ch) + `" at ` + gAt(line, col), line, col}
		}
	}
	toks = append(toks, gTok{'e', "<EOF>", line, i - lineStart + 1})
	return toks, nil
}

func gIndexRunes(src []rune, from int, needle string) int {
	pat := []rune(needle)
	for i := from; i+len(pat) <= len(src); i++ {
		if slices.Equal(src[i:i+len(pat)], pat) {
			return i
		}
	}
	return -1
}

// gTrimBlockTail removes /\n\s*$/: it cuts at the first newline after which only whitespace remains.
func gTrimBlockTail(raw string) string {
	last := -1
	for i, r := range raw {
		if !isJSSpace(r) {
			last = i
		}
	}
	if cut := strings.IndexByte(raw[last+1:], '\n'); cut >= 0 {
		return raw[:last+1+cut]
	}
	return raw
}

// gNumber matches /^-?(?:0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/ against at most 40 characters.
func gNumber(src []rune, i int) (int, bool) {
	lim := min(len(src), i+40)
	j := i
	if src[j] == '-' {
		j++
	}
	if j >= lim || !isDigitRune(src[j]) {
		return -1, false
	}
	if src[j] == '0' {
		j++
	} else {
		for j < lim && isDigitRune(src[j]) {
			j++
		}
	}
	float := false
	if j+1 < lim && src[j] == '.' && isDigitRune(src[j+1]) {
		j += 2
		for j < lim && isDigitRune(src[j]) {
			j++
		}
		float = true
	}
	if j < lim && (src[j] == 'e' || src[j] == 'E') {
		k := j + 1
		if k < lim && (src[k] == '+' || src[k] == '-') {
			k++
		}
		if k < lim && isDigitRune(src[k]) {
			for k < lim && isDigitRune(src[k]) {
				k++
			}
			j = k
			float = true
		}
	}
	return j, float
}

// gHexUnit is String.fromCharCode(parseInt(text, 16)): leading whitespace, a sign, hex digits; NaN gives 0.
func gHexUnit(text []rune) uint16 {
	s := strings.TrimLeftFunc(string(text), isJSSpace)
	negative := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		negative = s[0] == '-'
		s = s[1:]
	}
	var v int64
	digits := 0
	for _, r := range s {
		d, ok := hexNibble(byte(r))
		if r >= utf8.RuneSelf || !ok {
			break
		}
		v = v*16 + int64(d)
		digits++
	}
	if digits == 0 {
		return 0
	}
	if negative {
		v = -v
	}
	return uint16(v)
}

func gString(src []rune, i int) (string, int, bool) {
	n := len(src)
	j := i + 1
	var units []uint16
	for {
		if j >= n || src[j] == '\n' {
			return "", 0, false
		}
		d := src[j]
		if d == '"' {
			break
		}
		if d == '\\' {
			if j+1 >= n {
				return "", 0, false
			}
			e := src[j+1]
			if e == 'u' {
				units = append(units, gHexUnit(src[j+2:min(n, j+6)]))
				j += 6
				continue
			}
			switch e {
			case 'n':
				units = append(units, '\n')
			case 't':
				units = append(units, '\t')
			case 'r':
				units = append(units, '\r')
			case 'b':
				units = append(units, '\b')
			case 'f':
				units = append(units, '\f')
			default:
				units = utf16.AppendRune(units, e)
			}
			j += 2
			continue
		}
		units = utf16.AppendRune(units, d)
		j++
	}
	return string(utf16.Decode(units)), j + 1, true
}

// ---------------------------------------------------------------- parser

type gValue struct {
	k      byte // 'v' variable, 'i' int, 'f' float, 's' string, 'b' bool, 'n' null, 'e' enum, 'l' list, 'o' object
	name   string
	num    float64
	str    string
	b      bool
	items  []*gValue
	fields []gArgNode
}

type gArgNode struct {
	name  string
	value *gValue
}

type gDirective struct {
	name string
	args []gArgNode
}

type gSel struct {
	kind       byte // 'f' field, 's' fragment spread, 'i' inline fragment
	alias      string
	hasAlias   bool
	name       string
	args       []gArgNode
	directives []gDirective
	sel        []*gSel
	hasSel     bool
	typeCond   string
	hasCond    bool
	line       int
	col        int
}

type gVarDef struct {
	name string
	typ  string
	def  *gValue
}

type gOp struct {
	typ     string
	name    string
	hasName bool
	vars    []gVarDef
	sel     []*gSel
}

type gFragment struct {
	name     string
	typeCond string
	sel      []*gSel
}

type gDoc struct {
	ops   []*gOp
	frags map[string]*gFragment
}

type gParser struct {
	toks []gTok
	pos  int
}

func (p *gParser) peek() gTok { return p.toks[p.pos] }

func (p *gParser) next() gTok {
	tok := p.toks[p.pos]
	if tok.t != 'e' {
		p.pos++
	}
	return tok
}

func (p *gParser) fail(tok gTok) {
	label := `"` + tok.v + `"`
	if tok.t == 'e' {
		label = "end of file"
	}
	kind := map[byte]string{'n': "IDENTIFIER", 'e': "EOF", 'p': "PUNCT", 'i': "INT", 'f': "FLOAT", 's': "STRING"}[tok.t]
	panic(&gSyntaxError{"Parse error on " + label + " (" + kind + ") at " + gAt(tok.line, tok.col), tok.line, tok.col})
}

func (p *gParser) isPunct(v string) bool {
	tok := p.peek()
	return tok.t == 'p' && tok.v == v
}

func (p *gParser) expectPunct(v string) gTok {
	if !p.isPunct(v) {
		p.fail(p.peek())
	}
	return p.next()
}

func (p *gParser) expectName() gTok {
	if p.peek().t != 'n' {
		p.fail(p.peek())
	}
	return p.next()
}

func setArg(list []gArgNode, name string, value *gValue) []gArgNode {
	for i := range list {
		if list[i].name == name {
			list[i].value = value
			return list
		}
	}
	return append(list, gArgNode{name, value})
}

func (p *gParser) document() *gDoc {
	doc := &gDoc{frags: map[string]*gFragment{}}
	for p.peek().t != 'e' {
		if p.isPunct("{") {
			doc.ops = append(doc.ops, &gOp{typ: "query", sel: p.selectionSet()})
			continue
		}
		tok := p.expectName()
		switch tok.v {
		case "query", "mutation", "subscription":
			op := &gOp{typ: tok.v}
			if p.peek().t == 'n' {
				op.name, op.hasName = p.next().v, true
			}
			if p.isPunct("(") {
				p.next()
				for !p.isPunct(")") {
					p.expectPunct("$")
					name := p.expectName().v
					p.expectPunct(":")
					typ := p.typeRef()
					var def *gValue
					if p.isPunct("=") {
						p.next()
						def = p.value(true)
					}
					p.directives()
					op.vars = append(op.vars, gVarDef{name, typ, def})
				}
				p.next()
			}
			p.directives()
			op.sel = p.selectionSet()
			doc.ops = append(doc.ops, op)
		case "fragment":
			name := p.expectName().v
			on := p.expectName()
			if on.v != "on" {
				p.fail(on)
			}
			cond := p.expectName().v
			p.directives()
			doc.frags[name] = &gFragment{name, cond, p.selectionSet()}
		default:
			p.fail(tok)
		}
	}
	return doc
}

func (p *gParser) typeRef() string {
	var out string
	if p.isPunct("[") {
		p.next()
		out = "[" + p.typeRef() + "]"
		p.expectPunct("]")
	} else {
		out = p.expectName().v
	}
	if p.isPunct("!") {
		p.next()
		out += "!"
	}
	return out
}

func (p *gParser) directives() []gDirective {
	var out []gDirective
	for p.isPunct("@") {
		p.next()
		d := gDirective{name: p.expectName().v}
		if p.isPunct("(") {
			d.args = p.args()
		}
		out = append(out, d)
	}
	return out
}

func (p *gParser) args() []gArgNode {
	var out []gArgNode
	p.expectPunct("(")
	for !p.isPunct(")") {
		name := p.expectName().v
		p.expectPunct(":")
		out = setArg(out, name, p.value(false))
	}
	p.next()
	return out
}

func (p *gParser) selectionSet() []*gSel {
	p.expectPunct("{")
	var out []*gSel
	for !p.isPunct("}") {
		if p.peek().t == 'e' {
			p.fail(p.peek())
		}
		out = append(out, p.selection())
	}
	p.next()
	if len(out) == 0 {
		p.fail(p.peek())
	}
	return out
}

func (p *gParser) selection() *gSel {
	if p.isPunct("...") {
		spread := p.next()
		tok := p.peek()
		if tok.t == 'n' && tok.v != "on" {
			p.next()
			return &gSel{kind: 's', name: tok.v, directives: p.directives(), line: spread.line, col: spread.col}
		}
		sel := &gSel{kind: 'i'}
		if tok.t == 'n' && tok.v == "on" {
			p.next()
			sel.typeCond, sel.hasCond = p.expectName().v, true
		}
		sel.directives = p.directives()
		sel.sel, sel.hasSel = p.selectionSet(), true
		return sel
	}
	first := p.expectName()
	sel := &gSel{kind: 'f', name: first.v, line: first.line, col: first.col}
	if p.isPunct(":") {
		p.next()
		sel.alias, sel.hasAlias = sel.name, true
		sel.name = p.expectName().v
	}
	if p.isPunct("(") {
		sel.args = p.args()
	}
	sel.directives = p.directives()
	if p.isPunct("{") {
		sel.sel, sel.hasSel = p.selectionSet(), true
	}
	return sel
}

func (p *gParser) value(isConst bool) *gValue {
	tok := p.next()
	switch tok.t {
	case 'p':
		if tok.v == "$" && !isConst {
			return &gValue{k: 'v', name: p.expectName().v}
		}
		if tok.v == "[" {
			list := &gValue{k: 'l', items: []*gValue{}}
			for !p.isPunct("]") {
				list.items = append(list.items, p.value(isConst))
			}
			p.next()
			return list
		}
		if tok.v == "{" {
			obj := &gValue{k: 'o'}
			for !p.isPunct("}") {
				name := p.expectName().v
				p.expectPunct(":")
				obj.fields = setArg(obj.fields, name, p.value(isConst))
			}
			p.next()
			return obj
		}
	case 'i', 'f':
		num, _ := strconv.ParseFloat(tok.v, 64)
		return &gValue{k: tok.t, num: num}
	case 's':
		return &gValue{k: 's', str: tok.v}
	case 'n':
		switch tok.v {
		case "true":
			return &gValue{k: 'b', b: true}
		case "false":
			return &gValue{k: 'b', b: false}
		case "null":
			return &gValue{k: 'n'}
		}
		return &gValue{k: 'e', str: tok.v}
	}
	p.fail(tok)
	return nil
}

func gParse(text string) (doc *gDoc, synErr *gSyntaxError) {
	toks, lexErr := gLex(text)
	if lexErr != nil {
		return nil, lexErr
	}
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(*gSyntaxError)
			if !ok {
				panic(r)
			}
			doc, synErr = nil, e
		}
	}()
	return (&gParser{toks: toks}).document(), nil
}

// ---------------------------------------------------------------- schema

type gResolver func(src any, a map[string]any) any

type gArgDef struct {
	name string
	typ  string
}

type gField struct {
	name    string
	typ     string
	resolve gResolver
	args    []gArgDef
}

func (f *gField) hasArg(name string) bool {
	for _, a := range f.args {
		if a.name == name {
			return true
		}
	}
	return false
}

type gType struct {
	name       string
	kind       string // OBJECT, INTERFACE, UNION, ENUM, INPUT_OBJECT, SCALAR
	fields     []*gField
	byName     map[string]*gField
	values     []string
	inputs     []gArgDef
	interfaces []string
	possible   []string
	dynamic    bool
}

var (
	gSchema      = map[string]*gType{}
	gTypeOrder   []string
	gTypeObjects = map[string]*Obj{}
)

func gDefine(t *gType) {
	if t.fields != nil {
		t.byName = make(map[string]*gField, len(t.fields))
		for _, f := range t.fields {
			t.byName[f.name] = f
		}
	}
	if _, exists := gSchema[t.name]; !exists {
		gTypeOrder = append(gTypeOrder, t.name)
	}
	gSchema[t.name] = t
}

func gf(name, typ string, resolve gResolver, args ...gArgDef) *gField {
	return &gField{name: name, typ: typ, resolve: resolve, args: args}
}

func ga(name, typ string) gArgDef { return gArgDef{name, typ} }

func gPage(extra ...gArgDef) []gArgDef {
	return append([]gArgDef{{"first", "Int"}, {"after", "String"}, {"last", "Int"}, {"before", "String"}}, extra...)
}

func gObject(name string, interfaces []string, fields ...*gField) {
	if interfaces == nil {
		interfaces = []string{}
	}
	if fields == nil {
		fields = []*gField{}
	}
	gDefine(&gType{name: name, kind: "OBJECT", fields: fields, interfaces: interfaces})
}

func gEnum(name string, values ...string) { gDefine(&gType{name: name, kind: "ENUM", values: values}) }

func gInput(name string, inputs ...gArgDef) {
	gDefine(&gType{name: name, kind: "INPUT_OBJECT", inputs: inputs})
}

func gConnectionTypes(name, node string) {
	gObject(name+"Connection", nil, gf("nodes", "["+node+"]", nil), gf("edges", "["+name+"Edge]", nil), gf("pageInfo", "PageInfo!", nil), gf("totalCount", "Int!", nil))
	gObject(name+"Edge", nil, gf("cursor", "String!", nil), gf("node", node, nil))
}

func gConst(v any) gResolver { return func(any, map[string]any) any { return v } }

// -- sources

type gUser struct {
	login string
	id    int64
}

type gCommit struct {
	repo *Repo
	oid  string
}

type gRefSrc struct {
	repo *Repo
	name string
	oid  string
}

type gPRCommit struct {
	oid    string
	repo   *Repo
	number int64
}

func gUserOf(login string) *gUser {
	id := int64(1_000_000)
	if login == viewerLogin {
		id = viewerID
	}
	return &gUser{login: login, id: id}
}

func gRepoURL(repo *Repo) string { return base() + "/" + repo.Owner + "/" + repo.Name }

func gRefOf(repo *Repo, branch string) any {
	oid, ok := repo.GetRef("refs/heads/" + branch)
	if !ok {
		return nil
	}
	return &gRefSrc{repo: repo, name: branch, oid: oid}
}

func gCommitOf(repo *Repo, oid string) any {
	if repo.Commit(oid) == nil {
		return nil
	}
	return &gCommit{repo: repo, oid: oid}
}

func onUser(fn func(*gUser, map[string]any) any) gResolver {
	return func(s any, a map[string]any) any {
		u, _ := s.(*gUser)
		if u == nil {
			u = &gUser{}
		}
		return fn(u, a)
	}
}

func onIssue(fn func(*Issue, map[string]any) any) gResolver {
	return func(s any, a map[string]any) any { return fn(s.(*Issue), a) }
}

func onPull(fn func(*Pull, map[string]any) any) gResolver {
	return func(s any, a map[string]any) any { return fn(s.(*Pull), a) }
}

func onRepo(fn func(*Repo, map[string]any) any) gResolver {
	return func(s any, a map[string]any) any { return fn(s.(*Repo), a) }
}

func onComment(fn func(*Comment) any) gResolver {
	return func(s any, _ map[string]any) any { return fn(s.(*Comment)) }
}

func onLabel(fn func(*Label) any) gResolver {
	return func(s any, _ map[string]any) any { return fn(s.(*Label)) }
}

func onCommit(fn func(*gCommit) any) gResolver {
	return func(s any, _ map[string]any) any { return fn(s.(*gCommit)) }
}

// -- cursors and connections

type gFailure struct{ msg string }

func (e *gFailure) Error() string { return e.msg }

func gFail(msg string) { panic(&gFailure{msg}) }

func gEncodeCursor(index int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(index)))
}

func gDecodeCursor(cursor any) int {
	s, ok := cursor.(string)
	if !ok {
		return -1
	}
	text := string(nodeBase64Decode(s))
	digits, found := strings.CutPrefix(text, "cursor:")
	if !found || digits == "" || strings.TrimLeft(digits, "0123456789") != "" {
		gFail("`" + s + "` does not appear to be a valid cursor.")
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n > 1<<30 {
		n = 1 << 30
	}
	return n
}

func gClampCount(f float64, limit int) int {
	if f >= float64(limit) {
		return limit
	}
	return int(f)
}

func gConnection(items []any, a map[string]any) map[string]any {
	total := len(items)
	start, end := 0, total
	if v, ok := a["after"]; ok && v != nil {
		start = min(total, gDecodeCursor(v)+1)
	}
	if v, ok := a["before"]; ok && v != nil {
		end = max(start, min(total, gDecodeCursor(v)))
	}
	if f, ok := a["first"].(float64); ok {
		if f < 0 {
			gFail("Argument 'first' must be a non-negative integer.")
		}
		end = min(end, start+gClampCount(f, total))
	}
	if l, ok := a["last"].(float64); ok {
		if l < 0 {
			gFail("Argument 'last' must be a non-negative integer.")
		}
		start = max(start, end-gClampCount(l, total))
	}
	nodes := make([]any, end-start)
	copy(nodes, items[start:end])
	edges := make([]any, len(nodes))
	for i, node := range nodes {
		edges[i] = map[string]any{"cursor": gEncodeCursor(start + i), "node": node}
	}
	var startCursor, endCursor any
	if len(nodes) > 0 {
		startCursor, endCursor = gEncodeCursor(start), gEncodeCursor(end-1)
	}
	return map[string]any{
		"nodes":      nodes,
		"edges":      edges,
		"pageInfo":   map[string]any{"hasNextPage": end < total, "hasPreviousPage": start > 0, "startCursor": startCursor, "endCursor": endCursor},
		"totalCount": total,
	}
}

func gAnyList[T any](items []T) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

func gOrderBy(orderBy any) (string, bool) {
	field, desc := "CREATED_AT", false
	if m, ok := orderBy.(map[string]any); ok {
		if f, ok := m["field"]; ok && f != nil {
			field = jsString(f)
		}
		desc = m["direction"] == "DESC"
	}
	return field, desc
}

func gOrdered[T any](items []T, orderBy any, seq, comments, number func(T) int64) []any {
	field, desc := gOrderBy(orderBy)
	key := number
	switch field {
	case "UPDATED_AT":
		key = seq
	case "COMMENTS":
		key = comments
	}
	sorted := slices.Clone(items)
	sort.SliceStable(sorted, func(x, y int) bool {
		a, b := sorted[x], sorted[y]
		if ka, kb := key(a), key(b); ka != kb {
			return ka < kb
		}
		return number(a) < number(b)
	})
	out := make([]any, len(sorted))
	for i, item := range sorted {
		if desc {
			out[len(sorted)-1-i] = item
		} else {
			out[i] = item
		}
	}
	return out
}

func gOrderIssues(items []*Issue, orderBy any) []any {
	return gOrdered(items, orderBy, func(i *Issue) int64 { return i.Seq }, func(i *Issue) int64 { return int64(len(i.Comments)) }, func(i *Issue) int64 { return i.Number })
}

func gOrderPulls(items []*Pull, orderBy any) []any {
	return gOrdered(items, orderBy, func(p *Pull) int64 { return p.Seq }, func(p *Pull) int64 { return int64(len(p.Comments)) }, func(p *Pull) int64 { return p.Number })
}

// gNumKey is a JS number usable as a Map key for issue/pull numbers.
func gNumKey(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1e15 {
		return 0, false
	}
	return int64(f), true
}

func gStringOf(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return "undefined"
	}
	return jsString(v)
}

func gNodeByID(id any) any {
	s, ok := id.(string)
	if !ok {
		return nil
	}
	typ, n, ok := parseGlobalID(s)
	if !ok {
		return nil
	}
	switch typ {
	case "Repository":
		if repo := reposByID[n]; repo != nil {
			return repo
		}
	case "Issue":
		if issue := issuesByID[n]; issue != nil {
			return issue
		}
	case "PullRequest":
		if pull := pullsByID[n]; pull != nil {
			return pull
		}
	case "IssueComment":
		if comment := commentsByID[n]; comment != nil {
			return comment
		}
	case "User":
		if n == viewerID {
			return gUserOf(viewerLogin)
		}
	case "Label":
		for _, repo := range repoList {
			for _, label := range repo.labelList {
				if label.ID == n {
					return label
				}
			}
		}
	}
	return nil
}

func gTypeNameOf(src any) string {
	switch src.(type) {
	case *Repo:
		return "Repository"
	case *Issue:
		return "Issue"
	case *Pull:
		return "PullRequest"
	case *gCommit:
		return "Commit"
	case *Comment:
		return "IssueComment"
	case *Label:
		return "Label"
	}
	return "User"
}

// -- search

func gIsQualifier(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

// gSearchTokens is text.match(/(?:[^\s"]+:"[^"]*")|(?:"[^"]*")|\S+/g).
func gSearchTokens(s string) []string {
	var out []string
	runEnd := func(i int, stopAtQuote bool) int {
		for i < len(s) {
			r, size := utf8.DecodeRuneInString(s[i:])
			if isJSSpace(r) || (stopAtQuote && r == '"') {
				break
			}
			i += size
		}
		return i
	}
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if isJSSpace(r) {
			i += size
			continue
		}
		if j := runEnd(i, true); j < len(s) && s[j] == '"' && j-1 > i && s[j-1] == ':' {
			if q := strings.IndexByte(s[j+1:], '"'); q >= 0 {
				end := j + 2 + q
				out = append(out, s[i:end])
				i = end
				continue
			}
		}
		if s[i] == '"' {
			if q := strings.IndexByte(s[i+1:], '"'); q >= 0 {
				end := i + 2 + q
				out = append(out, s[i:end])
				i = end
				continue
			}
		}
		j := runEnd(i, false)
		out = append(out, s[i:j])
		i = j
	}
	return out
}

func gSearch(text, typ string) []any {
	var words []string
	var quals [][2]string
	for _, raw := range gSearchTokens(text) {
		token := strings.ReplaceAll(raw, `"`, "")
		if colon := strings.IndexByte(token, ':'); colon > 0 && gIsQualifier(token[:colon]) {
			quals = append(quals, [2]string{strings.ToLower(token[:colon]), token[colon+1:]})
		} else {
			words = append(words, strings.ToLower(token))
		}
	}
	qual := func(name string, lower bool) []string {
		var out []string
		for _, q := range quals {
			if q[0] == name {
				if lower {
					out = append(out, strings.ToLower(q[1]))
				} else {
					out = append(out, q[1])
				}
			}
		}
		return out
	}
	textMatches := func(fields ...string) bool {
		hay := strings.ToLower(strings.Join(fields, "\n"))
		for _, word := range words {
			if !strings.Contains(hay, word) {
				return false
			}
		}
		return true
	}
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	switch typ {
	case "REPOSITORY":
		owners := append(append(qual("user", false), qual("owner", false)...), qual("org", false)...)
		var repos []*Repo
		for _, repo := range repoList {
			if !textMatches(repo.Name, deref(repo.Description)) {
				continue
			}
			ok := true
			for _, owner := range owners {
				if strings.ToLower(owner) != strings.ToLower(repo.Owner) {
					ok = false
				}
			}
			if ok {
				repos = append(repos, repo)
			}
		}
		sort.SliceStable(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
		return gAnyList(repos)
	case "USER":
		if textMatches(viewerLogin) {
			return []any{gUserOf(viewerLogin)}
		}
		return []any{}
	case "ISSUE", "ISSUE_ADVANCED":
	default:
		return []any{}
	}
	repoFilters := qual("repo", true)
	kinds := qual("is", true)
	types := qual("type", false)
	wantIssue := slices.Contains(kinds, "issue") || slices.Contains(types, "issue")
	wantPr := slices.Contains(kinds, "pr") || slices.Contains(kinds, "pull-request") || slices.Contains(types, "pr")
	var states []string
	for _, v := range append(slices.Clone(kinds), qual("state", true)...) {
		if v == "open" || v == "closed" || v == "merged" {
			states = append(states, v)
		}
	}
	labels := qual("label", true)
	authors := qual("author", true)
	type hit struct {
		item any
		seq  int64
	}
	var hits []hit
	consider := func(item any, title string, body *string, state string, itemLabels []*Label, user string, seq int64) {
		if !textMatches(title, deref(body)) {
			return
		}
		if len(states) > 0 && !slices.Contains(states, state) {
			return
		}
		for _, name := range labels {
			if !slices.ContainsFunc(itemLabels, func(l *Label) bool { return strings.ToLower(l.Name) == name }) {
				return
			}
		}
		if len(authors) > 0 && !slices.Contains(authors, strings.ToLower(user)) {
			return
		}
		hits = append(hits, hit{item, seq})
	}
	for _, repo := range repoList {
		if len(repoFilters) > 0 && !slices.Contains(repoFilters, strings.ToLower(repo.Owner+"/"+repo.Name)) {
			continue
		}
		if !(wantPr && !wantIssue) {
			for _, issue := range repo.issueList {
				consider(issue, issue.Title, issue.Body, issue.State, issue.Labels, issue.User, issue.Seq)
			}
		}
		if !(wantIssue && !wantPr) {
			for _, pull := range repo.pullList {
				state := pull.State
				if pull.Merged {
					state = "merged"
				}
				consider(pull, pull.Title, pull.Body, state, pull.Labels, pull.User, pull.Seq)
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].seq > hits[j].seq })
	out := make([]any, len(hits))
	for i, h := range hits {
		out[i] = h.item
	}
	return out
}

// -- mutation helpers

func gWrapAPI(run func()) {
	defer func() {
		if r := recover(); r != nil {
			apiErr, ok := r.(*ApiError)
			if !ok {
				panic(r)
			}
			var detail *Obj
			if len(apiErr.Errors) > 0 {
				detail, _ = apiErr.Errors[0].(*Obj)
			}
			if detail != nil {
				code, _ := detail.Get("code")
				field, _ := detail.Get("field")
				if f, isString := field.(string); code == "missing_field" && isString && f != "" {
					gFail(strings.ToUpper(f[:1]) + f[1:] + " can't be blank")
				}
				if message, ok := detail.Get("message"); ok {
					if s, isString := message.(string); isString {
						gFail(s)
					}
				}
			}
			gFail(apiErr.Message)
		}
	}()
	run()
}

func gInputOf(a map[string]any, firstKey string) map[string]any {
	v, present := a["input"]
	if !present {
		panic(errors.New("Cannot read properties of undefined (reading '" + firstKey + "')"))
	}
	if v == nil {
		panic(errors.New("Cannot read properties of null (reading '" + firstKey + "')"))
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func gClientID(a map[string]any) any {
	if m, ok := a["input"].(map[string]any); ok {
		if v, ok := m["clientMutationId"]; ok {
			return v
		}
	}
	return nil
}

func gRepoFromID(in map[string]any, key string) *Repo {
	if s, ok := in[key].(string); ok {
		if typ, n, ok := parseGlobalID(s); ok && typ == "Repository" {
			if repo := reposByID[n]; repo != nil {
				return repo
			}
		}
	}
	gFail("Could not resolve to a Repository with the id '" + gStringOf(in, key) + "'.")
	return nil
}

func gIssueFromID(in map[string]any, key string) *Issue {
	if issue, ok := gNodeByID(in[key]).(*Issue); ok {
		return issue
	}
	gFail("Could not resolve to an Issue with the id '" + gStringOf(in, key) + "'.")
	return nil
}

func gPullFromID(in map[string]any, key string) *Pull {
	if pull, ok := gNodeByID(in[key]).(*Pull); ok {
		return pull
	}
	gFail("Could not resolve to a PullRequest with the id '" + gStringOf(in, key) + "'.")
	return nil
}

func gISOOrNil(sec int64, set bool) any {
	if !set {
		return nil
	}
	return ISOMillis(sec)
}

func gActor(id Ident) any {
	var user any
	if id.Name == viewerLogin {
		user = gUserOf(viewerLogin)
	}
	return map[string]any{"name": id.Name, "email": id.Email, "date": ISOMillis(id.When), "user": user}
}

func gIssueOrNil(issue *Issue) any {
	if issue == nil {
		return nil
	}
	return issue
}

func gPullOrNil(pull *Pull) any {
	if pull == nil {
		return nil
	}
	return pull
}

func gBodyOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func gLowerStrings(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(list))
	for i, item := range list {
		s, isString := item.(string)
		if !isString {
			panic(errors.New("name.toLowerCase is not a function"))
		}
		out[i] = strings.ToLower(s)
	}
	return out, true
}

func gContainsAny(list []any, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func init() {
	gBuildSchema()
	for _, name := range gTypeOrder {
		gTypeObjects[name] = gNewTypeObject(gSchema[name])
	}
}

func gBuildSchema() {
	for _, name := range []string{"String", "Int", "Float", "Boolean", "ID", "DateTime", "URI", "HTML", "GitObjectID"} {
		gDefine(&gType{name: name, kind: "SCALAR"})
	}
	gEnum("IssueState", "OPEN", "CLOSED")
	gEnum("IssueStateReason", "COMPLETED", "NOT_PLANNED", "REOPENED")
	gEnum("IssueClosedStateReason", "COMPLETED", "NOT_PLANNED", "DUPLICATE")
	gEnum("PullRequestState", "OPEN", "CLOSED", "MERGED")
	gEnum("MergeableState", "MERGEABLE", "CONFLICTING", "UNKNOWN")
	gEnum("PullRequestMergeMethod", "MERGE", "SQUASH", "REBASE")
	gEnum("RepositoryPermission", "ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ")
	gEnum("RepositoryVisibility", "PRIVATE", "PUBLIC", "INTERNAL")
	gEnum("SearchType", "ISSUE", "ISSUE_ADVANCED", "REPOSITORY", "USER", "DISCUSSION")
	gEnum("OrderDirection", "ASC", "DESC")
	gEnum("IssueOrderField", "CREATED_AT", "UPDATED_AT", "COMMENTS")
	gEnum("PullRequestOrderField", "CREATED_AT", "UPDATED_AT")
	gEnum("PatchStatus", "ADDED", "DELETED", "RENAMED", "COPIED", "MODIFIED", "CHANGED")

	gInput("IssueOrder", ga("field", "IssueOrderField!"), ga("direction", "OrderDirection!"))
	gInput("IssueFilters", ga("assignee", "String"), ga("createdBy", "String"), ga("labels", "[String!]"), ga("mentioned", "String"), ga("since", "DateTime"), ga("states", "[IssueState!]"))
	gInput("IssueOrPullRequestOrder", ga("field", "IssueOrderField!"), ga("direction", "OrderDirection!"))
	gInput("PullRequestOrder", ga("field", "PullRequestOrderField!"), ga("direction", "OrderDirection!"))
	gInput("CreateIssueInput", ga("repositoryId", "ID!"), ga("title", "String!"), ga("body", "String"), ga("assigneeIds", "[ID!]"), ga("labelIds", "[ID!]"), ga("milestoneId", "ID"), ga("clientMutationId", "String"))
	gInput("UpdateIssueInput", ga("id", "ID!"), ga("title", "String"), ga("body", "String"), ga("state", "IssueState"), ga("labelIds", "[ID!]"), ga("clientMutationId", "String"))
	gInput("CloseIssueInput", ga("issueId", "ID!"), ga("stateReason", "IssueClosedStateReason"), ga("clientMutationId", "String"))
	gInput("ReopenIssueInput", ga("issueId", "ID!"), ga("clientMutationId", "String"))
	gInput("CreatePullRequestInput", ga("repositoryId", "ID!"), ga("baseRefName", "String!"), ga("headRefName", "String!"), ga("title", "String!"), ga("body", "String"), ga("draft", "Boolean"), ga("maintainerCanModify", "Boolean"), ga("clientMutationId", "String"))
	gInput("MergePullRequestInput", ga("pullRequestId", "ID!"), ga("commitHeadline", "String"), ga("commitBody", "String"), ga("expectedHeadOid", "GitObjectID"), ga("mergeMethod", "PullRequestMergeMethod"), ga("authorEmail", "String"), ga("clientMutationId", "String"))
	gInput("ClosePullRequestInput", ga("pullRequestId", "ID!"), ga("clientMutationId", "String"))
	gInput("ReopenPullRequestInput", ga("pullRequestId", "ID!"), ga("clientMutationId", "String"))
	gInput("AddCommentInput", ga("subjectId", "ID!"), ga("body", "String!"), ga("clientMutationId", "String"))
	gInput("CreateRepositoryInput", ga("name", "String!"), ga("visibility", "RepositoryVisibility!"), ga("description", "String"), ga("clientMutationId", "String"))

	gDefine(&gType{name: "Node", kind: "INTERFACE", fields: []*gField{gf("id", "ID!", nil)}, possible: []string{"Repository", "Issue", "PullRequest", "IssueComment", "User", "Label"}})
	gDefine(&gType{name: "Actor", kind: "INTERFACE", fields: []*gField{gf("login", "String!", nil), gf("url", "URI!", nil), gf("avatarUrl", "URI!", nil)}, possible: []string{"User"}})
	gDefine(&gType{name: "RepositoryOwner", kind: "INTERFACE", fields: []*gField{gf("id", "ID!", nil), gf("login", "String!", nil), gf("url", "URI!", nil)}, possible: []string{"User"}})
	gDefine(&gType{name: "GitObject", kind: "INTERFACE", fields: []*gField{gf("oid", "GitObjectID!", nil)}, possible: []string{"Commit"}})
	gDefine(&gType{name: "SearchResultItem", kind: "UNION", possible: []string{"Issue", "PullRequest", "Repository", "User"}})

	gObject("PageInfo", nil, gf("hasNextPage", "Boolean!", nil), gf("hasPreviousPage", "Boolean!", nil), gf("startCursor", "String", nil), gf("endCursor", "String", nil))
	gObject("RateLimit", nil, gf("limit", "Int!", gConst(5000)), gf("cost", "Int!", gConst(1)), gf("remaining", "Int!", gConst(5000)),
		gf("used", "Int!", gConst(0)), gf("nodeCount", "Int!", gConst(0)), gf("resetAt", "DateTime!", gConst("2025-01-01T01:00:00Z")))

	gObject("User", []string{"Node", "Actor", "RepositoryOwner"},
		gf("id", "ID!", onUser(func(u *gUser, _ map[string]any) any { return globalID("User", u.id) })),
		gf("databaseId", "Int", onUser(func(u *gUser, _ map[string]any) any { return u.id })),
		gf("login", "String!", onUser(func(u *gUser, _ map[string]any) any { return u.login })),
		gf("name", "String", onUser(func(u *gUser, _ map[string]any) any { return u.login })),
		gf("url", "URI!", onUser(func(u *gUser, _ map[string]any) any { return base() + "/" + u.login })),
		gf("avatarUrl", "URI!", onUser(func(u *gUser, _ map[string]any) any { return base() + "/avatars/u/" + strconv.FormatInt(u.id, 10) }), ga("size", "Int")),
		gf("repositories", "RepositoryConnection!", onUser(func(_ *gUser, a map[string]any) any {
			repos := slices.Clone(repoList)
			sort.SliceStable(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
			return gConnection(gAnyList(repos), a)
		}), gPage(ga("orderBy", "String"), ga("privacy", "RepositoryVisibility"))...),
	)
	gObject("Label", []string{"Node"},
		gf("id", "ID!", onLabel(func(l *Label) any { return globalID("Label", l.ID) })),
		gf("name", "String!", onLabel(func(l *Label) any { return l.Name })),
		gf("color", "String!", onLabel(func(l *Label) any { return l.Color })),
		gf("description", "String", onLabel(func(l *Label) any { return strOrNil(l.Description) })),
	)
	gObject("GitActor", nil, gf("name", "String", nil), gf("email", "String", nil), gf("date", "DateTime", nil), gf("user", "User", nil))
	commitInfo := func(c *gCommit) *CommitInfo { return c.repo.Commit(c.oid) }
	gObject("Commit", []string{"GitObject"},
		gf("id", "ID!", onCommit(func(c *gCommit) any { return globalID("Commit", shortHexID(c.oid)) })),
		gf("oid", "GitObjectID!", onCommit(func(c *gCommit) any { return c.oid })),
		gf("abbreviatedOid", "String!", onCommit(func(c *gCommit) any { return c.oid[:min(7, len(c.oid))] })),
		gf("message", "String!", onCommit(func(c *gCommit) any {
			if info := commitInfo(c); info != nil {
				return strings.TrimSuffix(info.Message, "\n")
			}
			return ""
		})),
		gf("messageHeadline", "String!", onCommit(func(c *gCommit) any {
			message := ""
			if info := commitInfo(c); info != nil {
				message = info.Message
			}
			headline, _, _ := strings.Cut(message, "\n")
			return headline
		})),
		gf("messageBody", "String!", onCommit(func(c *gCommit) any {
			message := ""
			if info := commitInfo(c); info != nil {
				message = info.Message
			}
			_, body, _ := strings.Cut(message, "\n")
			return jsTrim(body)
		})),
		gf("committedDate", "DateTime!", onCommit(func(c *gCommit) any {
			var when int64
			if info := commitInfo(c); info != nil {
				when = info.Committer.When
			}
			return ISOMillis(when)
		})),
		gf("authoredDate", "DateTime!", onCommit(func(c *gCommit) any {
			var when int64
			if info := commitInfo(c); info != nil {
				when = info.Author.When
			}
			return ISOMillis(when)
		})),
		gf("url", "URI!", onCommit(func(c *gCommit) any { return gRepoURL(c.repo) + "/commit/" + c.oid })),
		gf("author", "GitActor", onCommit(func(c *gCommit) any {
			if info := commitInfo(c); info != nil {
				return gActor(info.Author)
			}
			return nil
		})),
		gf("committer", "GitActor", onCommit(func(c *gCommit) any {
			if info := commitInfo(c); info != nil {
				return gActor(info.Committer)
			}
			return nil
		})),
	)
	gObject("Ref", nil,
		gf("id", "ID!", func(s any, _ map[string]any) any {
			r := s.(*gRefSrc)
			return globalID("Ref", r.repo.ID*1000+int64(utf16Len(r.name)))
		}),
		gf("name", "String!", func(s any, _ map[string]any) any { return s.(*gRefSrc).name }),
		gf("prefix", "String!", gConst("refs/heads/")),
		gf("target", "GitObject", func(s any, _ map[string]any) any { r := s.(*gRefSrc); return gCommitOf(r.repo, r.oid) }),
	)
	gObject("IssueComment", []string{"Node"},
		gf("id", "ID!", onComment(func(c *Comment) any { return globalID("IssueComment", c.ID) })),
		gf("databaseId", "Int", onComment(func(c *Comment) any { return c.ID })),
		gf("body", "String!", onComment(func(c *Comment) any { return c.Body })),
		gf("bodyText", "String!", onComment(func(c *Comment) any { return c.Body })),
		gf("author", "Actor", onComment(func(c *Comment) any { return gUserOf(c.User) })),
		gf("createdAt", "DateTime!", onComment(func(c *Comment) any { return c.CreatedAt })),
		gf("updatedAt", "DateTime!", onComment(func(c *Comment) any { return c.UpdatedAt })),
		gf("url", "URI!", onComment(func(c *Comment) any {
			return gRepoURL(c.repo()) + "/issues/" + strconv.FormatInt(c.number(), 10) + "#issuecomment-" + strconv.FormatInt(c.ID, 10)
		})),
		gf("issue", "Issue", onComment(func(c *Comment) any { return gIssueOrNil(c.Issue) })),
	)
	gObject("Issue", []string{"Node"},
		gf("id", "ID!", onIssue(func(i *Issue, _ map[string]any) any { return globalID("Issue", i.ID) })),
		gf("databaseId", "Int", onIssue(func(i *Issue, _ map[string]any) any { return i.ID })),
		gf("number", "Int!", onIssue(func(i *Issue, _ map[string]any) any { return i.Number })),
		gf("title", "String!", onIssue(func(i *Issue, _ map[string]any) any { return i.Title })),
		gf("body", "String!", onIssue(func(i *Issue, _ map[string]any) any { return gBodyOf(i.Body) })),
		gf("bodyText", "String!", onIssue(func(i *Issue, _ map[string]any) any { return gBodyOf(i.Body) })),
		gf("state", "IssueState!", onIssue(func(i *Issue, _ map[string]any) any { return strings.ToUpper(i.State) })),
		gf("stateReason", "IssueStateReason", onIssue(func(i *Issue, _ map[string]any) any {
			if i.StateReason == nil {
				return nil
			}
			return strings.ToUpper(*i.StateReason)
		})),
		gf("closed", "Boolean!", onIssue(func(i *Issue, _ map[string]any) any { return i.State == "closed" })),
		gf("locked", "Boolean!", gConst(false)),
		gf("url", "URI!", onIssue(func(i *Issue, _ map[string]any) any {
			return gRepoURL(i.Repo) + "/issues/" + strconv.FormatInt(i.Number, 10)
		})),
		gf("createdAt", "DateTime!", onIssue(func(i *Issue, _ map[string]any) any { return i.CreatedAt })),
		gf("updatedAt", "DateTime!", onIssue(func(i *Issue, _ map[string]any) any { return i.UpdatedAt })),
		gf("closedAt", "DateTime", onIssue(func(i *Issue, _ map[string]any) any { return strOrNil(i.ClosedAt) })),
		gf("author", "Actor", onIssue(func(i *Issue, _ map[string]any) any { return gUserOf(i.User) })),
		gf("repository", "Repository!", onIssue(func(i *Issue, _ map[string]any) any { return i.Repo })),
		gf("labels", "LabelConnection", onIssue(func(i *Issue, a map[string]any) any { return gConnection(gAnyList(i.Labels), a) }), gPage()...),
		gf("comments", "IssueCommentConnection!", onIssue(func(i *Issue, a map[string]any) any { return gConnection(gAnyList(i.Comments), a) }), gPage()...),
		gf("assignees", "UserConnection!", func(_ any, a map[string]any) any { return gConnection([]any{}, a) }, gPage()...),
	)
	gObject("PullRequestChangedFile", nil,
		gf("path", "String!", func(s any, _ map[string]any) any { return s.(*FileDiff).Filename }),
		gf("additions", "Int!", func(s any, _ map[string]any) any { return s.(*FileDiff).Additions }),
		gf("deletions", "Int!", func(s any, _ map[string]any) any { return s.(*FileDiff).Deletions }),
		gf("changeType", "PatchStatus!", func(s any, _ map[string]any) any {
			switch s.(*FileDiff).Status {
			case "added":
				return "ADDED"
			case "removed":
				return "DELETED"
			}
			return "MODIFIED"
		}),
	)
	gObject("PullRequestCommit", nil,
		gf("id", "ID!", func(s any, _ map[string]any) any {
			return globalID("PullRequestCommit", shortHexID(s.(*gPRCommit).oid))
		}),
		gf("commit", "Commit!", func(s any, _ map[string]any) any { c := s.(*gPRCommit); return gCommitOf(c.repo, c.oid) }),
		gf("url", "URI!", func(s any, _ map[string]any) any {
			c := s.(*gPRCommit)
			return gRepoURL(c.repo) + "/pull/" + strconv.FormatInt(c.number, 10) + "/commits/" + c.oid
		}),
	)
	pullFileList := func(p *Pull) []any {
		files := pullFiles(p)
		out := make([]any, len(files))
		for i := range files {
			out[i] = &files[i]
		}
		return out
	}
	gObject("PullRequest", []string{"Node"},
		gf("id", "ID!", onPull(func(p *Pull, _ map[string]any) any { return globalID("PullRequest", p.ID) })),
		gf("databaseId", "Int", onPull(func(p *Pull, _ map[string]any) any { return p.ID })),
		gf("number", "Int!", onPull(func(p *Pull, _ map[string]any) any { return p.Number })),
		gf("title", "String!", onPull(func(p *Pull, _ map[string]any) any { return p.Title })),
		gf("body", "String!", onPull(func(p *Pull, _ map[string]any) any { return gBodyOf(p.Body) })),
		gf("bodyText", "String!", onPull(func(p *Pull, _ map[string]any) any { return gBodyOf(p.Body) })),
		gf("state", "PullRequestState!", onPull(func(p *Pull, _ map[string]any) any {
			if p.Merged {
				return "MERGED"
			}
			return strings.ToUpper(p.State)
		})),
		gf("closed", "Boolean!", onPull(func(p *Pull, _ map[string]any) any { return p.State == "closed" })),
		gf("merged", "Boolean!", onPull(func(p *Pull, _ map[string]any) any { return p.Merged })),
		gf("mergedAt", "DateTime", onPull(func(p *Pull, _ map[string]any) any { return strOrNil(p.MergedAt) })),
		gf("isDraft", "Boolean!", onPull(func(p *Pull, _ map[string]any) any { return p.Draft })),
		gf("mergeable", "MergeableState!", onPull(func(p *Pull, _ map[string]any) any {
			switch pullMergeable(p) {
			case 1:
				return "MERGEABLE"
			case 0:
				return "CONFLICTING"
			}
			return "UNKNOWN"
		})),
		gf("headRefName", "String!", onPull(func(p *Pull, _ map[string]any) any { return p.HeadRef })),
		gf("baseRefName", "String!", onPull(func(p *Pull, _ map[string]any) any { return p.BaseRef })),
		gf("headRefOid", "GitObjectID!", onPull(func(p *Pull, _ map[string]any) any { return pullHeadSha(p) })),
		gf("baseRefOid", "GitObjectID!", onPull(func(p *Pull, _ map[string]any) any { return pullBaseSha(p) })),
		gf("url", "URI!", onPull(func(p *Pull, _ map[string]any) any {
			return gRepoURL(p.Repo) + "/pull/" + strconv.FormatInt(p.Number, 10)
		})),
		gf("createdAt", "DateTime!", onPull(func(p *Pull, _ map[string]any) any { return p.CreatedAt })),
		gf("updatedAt", "DateTime!", onPull(func(p *Pull, _ map[string]any) any { return p.UpdatedAt })),
		gf("closedAt", "DateTime", onPull(func(p *Pull, _ map[string]any) any { return strOrNil(p.ClosedAt) })),
		gf("author", "Actor", onPull(func(p *Pull, _ map[string]any) any { return gUserOf(p.User) })),
		gf("repository", "Repository!", onPull(func(p *Pull, _ map[string]any) any { return p.Repo })),
		gf("baseRepository", "Repository", onPull(func(p *Pull, _ map[string]any) any { return p.Repo })),
		gf("headRepository", "Repository", onPull(func(p *Pull, _ map[string]any) any { return p.Repo })),
		gf("mergeCommit", "Commit", onPull(func(p *Pull, _ map[string]any) any {
			if p.MergeCommitSha == nil {
				return nil
			}
			return gCommitOf(p.Repo, *p.MergeCommitSha)
		})),
		gf("labels", "LabelConnection", onPull(func(p *Pull, a map[string]any) any { return gConnection(gAnyList(p.Labels), a) }), gPage()...),
		gf("comments", "IssueCommentConnection!", onPull(func(p *Pull, a map[string]any) any { return gConnection(gAnyList(p.Comments), a) }), gPage()...),
		gf("files", "PullRequestChangedFileConnection", onPull(func(p *Pull, a map[string]any) any { return gConnection(pullFileList(p), a) }), gPage()...),
		gf("commits", "PullRequestCommitConnection!", onPull(func(p *Pull, a map[string]any) any {
			oids := pullCommits(p)
			items := make([]any, len(oids))
			for i, oid := range oids {
				items[len(oids)-1-i] = &gPRCommit{oid: oid, repo: p.Repo, number: p.Number}
			}
			return gConnection(items, a)
		}), gPage()...),
		gf("additions", "Int!", onPull(func(p *Pull, _ map[string]any) any {
			n := 0
			for _, f := range pullFiles(p) {
				n += f.Additions
			}
			return n
		})),
		gf("deletions", "Int!", onPull(func(p *Pull, _ map[string]any) any {
			n := 0
			for _, f := range pullFiles(p) {
				n += f.Deletions
			}
			return n
		})),
		gf("changedFiles", "Int!", onPull(func(p *Pull, _ map[string]any) any { return len(pullFiles(p)) })),
	)
	gObject("Repository", []string{"Node"},
		gf("id", "ID!", onRepo(func(r *Repo, _ map[string]any) any { return globalID("Repository", r.ID) })),
		gf("databaseId", "Int", onRepo(func(r *Repo, _ map[string]any) any { return r.ID })),
		gf("name", "String!", onRepo(func(r *Repo, _ map[string]any) any { return r.Name })),
		gf("nameWithOwner", "String!", onRepo(func(r *Repo, _ map[string]any) any { return r.Owner + "/" + r.Name })),
		gf("owner", "RepositoryOwner!", onRepo(func(r *Repo, _ map[string]any) any { return gUserOf(r.Owner) })),
		gf("description", "String", onRepo(func(r *Repo, _ map[string]any) any { return strOrNil(r.Description) })),
		gf("isPrivate", "Boolean!", onRepo(func(r *Repo, _ map[string]any) any { return r.Private })),
		gf("isArchived", "Boolean!", onRepo(func(r *Repo, _ map[string]any) any { return r.Archived })),
		gf("isFork", "Boolean!", gConst(false)),
		gf("url", "URI!", onRepo(func(r *Repo, _ map[string]any) any { return gRepoURL(r) })),
		gf("createdAt", "DateTime!", onRepo(func(r *Repo, _ map[string]any) any { return r.CreatedAt })),
		gf("updatedAt", "DateTime!", onRepo(func(r *Repo, _ map[string]any) any { return r.UpdatedAt })),
		gf("pushedAt", "DateTime", onRepo(func(r *Repo, _ map[string]any) any { return gISOOrNil(r.PushedAt, r.PushedAt != 0) })),
		gf("viewerPermission", "RepositoryPermission", gConst("ADMIN")),
		gf("hasIssuesEnabled", "Boolean!", onRepo(func(r *Repo, _ map[string]any) any { return r.HasIssues })),
		gf("hasPullRequestsEnabled", "Boolean!", gConst(true)),
		gf("stargazerCount", "Int!", gConst(0)),
		gf("defaultBranchRef", "Ref", onRepo(func(r *Repo, _ map[string]any) any { return gRefOf(r, r.DefaultBranch) })),
		gf("ref", "Ref", onRepo(func(r *Repo, a map[string]any) any {
			name := ""
			if v, ok := a["qualifiedName"]; ok && v != nil {
				name = jsString(v)
			}
			return gRefOf(r, strings.TrimPrefix(name, "refs/heads/"))
		}), ga("qualifiedName", "String!")),
		gf("issue", "Issue", onRepo(func(r *Repo, a map[string]any) any {
			if n, ok := gNumKey(a["number"]); ok {
				return gIssueOrNil(r.issues[n])
			}
			return nil
		}), ga("number", "Int!")),
		gf("issues", "IssueConnection!", onRepo(func(r *Repo, a map[string]any) any {
			states, hasStates := a["states"].([]any)
			if !hasStates {
				if filter, ok := a["filterBy"].(map[string]any); ok {
					states, hasStates = filter["states"].([]any)
				}
			}
			labels, _ := gLowerStrings(a["labels"])
			var items []*Issue
			for _, issue := range r.issueList {
				if hasStates && !gContainsAny(states, strings.ToUpper(issue.State)) {
					continue
				}
				ok := true
				for _, name := range labels {
					if !slices.ContainsFunc(issue.Labels, func(l *Label) bool { return strings.ToLower(l.Name) == name }) {
						ok = false
						break
					}
				}
				if ok {
					items = append(items, issue)
				}
			}
			return gConnection(gOrderIssues(items, a["orderBy"]), a)
		}), gPage(ga("states", "[IssueState!]"), ga("orderBy", "IssueOrder"), ga("labels", "[String!]"), ga("filterBy", "IssueFilters"))...),
		gf("pullRequest", "PullRequest", onRepo(func(r *Repo, a map[string]any) any {
			if n, ok := gNumKey(a["number"]); ok {
				return gPullOrNil(r.pulls[n])
			}
			return nil
		}), ga("number", "Int!")),
		gf("pullRequests", "PullRequestConnection!", onRepo(func(r *Repo, a map[string]any) any {
			states, hasStates := a["states"].([]any)
			var items []*Pull
			for _, pull := range r.pullList {
				state := strings.ToUpper(pull.State)
				if pull.Merged {
					state = "MERGED"
				}
				if hasStates && !gContainsAny(states, state) {
					continue
				}
				if v := a["baseRefName"]; v != nil && v != pull.BaseRef {
					continue
				}
				if v := a["headRefName"]; v != nil && v != pull.HeadRef {
					continue
				}
				items = append(items, pull)
			}
			return gConnection(gOrderPulls(items, a["orderBy"]), a)
		}), gPage(ga("states", "[PullRequestState!]"), ga("baseRefName", "String"), ga("headRefName", "String"), ga("orderBy", "IssueOrder"))...),
		gf("labels", "LabelConnection", onRepo(func(r *Repo, a map[string]any) any { return gConnection(gAnyList(r.labelList), a) }), gPage(ga("query", "String"))...),
	)
	gObject("SearchResultItemConnection", nil, gf("issueCount", "Int!", nil), gf("repositoryCount", "Int!", nil), gf("userCount", "Int!", nil),
		gf("discussionCount", "Int!", gConst(0)), gf("codeCount", "Int!", gConst(0)), gf("nodes", "[SearchResultItem]", nil),
		gf("edges", "[SearchResultItemEdge]", nil), gf("pageInfo", "PageInfo!", nil))
	gObject("SearchResultItemEdge", nil, gf("cursor", "String!", nil), gf("node", "SearchResultItem", nil))
	for _, name := range []string{"Issue", "PullRequest", "Label", "IssueComment", "PullRequestChangedFile", "PullRequestCommit", "User", "Repository"} {
		gConnectionTypes(name, name)
	}
	payload := func(name, field, typ string) {
		gObject(name, nil, gf(field, typ, nil), gf("clientMutationId", "String", nil))
	}
	payload("CreateIssuePayload", "issue", "Issue")
	payload("UpdateIssuePayload", "issue", "Issue")
	payload("CloseIssuePayload", "issue", "Issue")
	payload("ReopenIssuePayload", "issue", "Issue")
	payload("CreatePullRequestPayload", "pullRequest", "PullRequest")
	payload("MergePullRequestPayload", "pullRequest", "PullRequest")
	payload("ClosePullRequestPayload", "pullRequest", "PullRequest")
	payload("ReopenPullRequestPayload", "pullRequest", "PullRequest")
	payload("CreateRepositoryPayload", "repository", "Repository")
	gObject("AddCommentPayload", nil, gf("commentEdge", "IssueCommentEdge", nil), gf("subject", "Node", nil), gf("timelineEdge", "IssueCommentEdge", nil), gf("clientMutationId", "String", nil))

	viewerMatches := func(a map[string]any, key string) bool { return strings.ToLower(gStringOf(a, key)) == viewerLower }
	gObject("Query", nil,
		gf("viewer", "User!", func(any, map[string]any) any { return gUserOf(viewerLogin) }),
		gf("user", "User", func(_ any, a map[string]any) any {
			if viewerMatches(a, "login") {
				return gUserOf(viewerLogin)
			}
			return nil
		}, ga("login", "String!")),
		gf("repository", "Repository", func(_ any, a map[string]any) any {
			if !viewerMatches(a, "owner") {
				return nil
			}
			if repo := getRepo(viewerLogin, gStringOf(a, "name")); repo != nil {
				return repo
			}
			return nil
		}, ga("owner", "String"), ga("name", "String"), ga("followRenames", "Boolean")),
		gf("repositoryOwner", "RepositoryOwner", func(_ any, a map[string]any) any {
			if viewerMatches(a, "login") {
				return gUserOf(viewerLogin)
			}
			return nil
		}, ga("login", "String!")),
		gf("node", "Node", func(_ any, a map[string]any) any { return gNodeByID(a["id"]) }, ga("id", "ID!")),
		gf("nodes", "[Node]!", func(_ any, a map[string]any) any {
			ids, ok := a["ids"].([]any)
			if !ok {
				return []any{}
			}
			out := make([]any, len(ids))
			for i, id := range ids {
				out[i] = gNodeByID(id)
			}
			return out
		}, ga("ids", "[ID!]!")),
		gf("search", "SearchResultItemConnection!", func(_ any, a map[string]any) any {
			text := ""
			if v, ok := a["query"]; ok && v != nil {
				text = jsString(v)
			}
			typ := gStringOf(a, "type")
			items := gSearch(text, typ)
			connection := gConnection(items, a)
			count := func(match bool) int {
				if match {
					return len(items)
				}
				return 0
			}
			connection["issueCount"] = count(typ == "ISSUE" || typ == "ISSUE_ADVANCED")
			connection["repositoryCount"] = count(typ == "REPOSITORY")
			connection["userCount"] = count(typ == "USER")
			return connection
		}, gPage(ga("query", "String!"), ga("type", "SearchType!"))...),
		gf("rateLimit", "RateLimit", func(any, map[string]any) any { return map[string]any{} }, ga("dryRun", "Boolean")),
		gf("__schema", "__Schema!", nil),
		gf("__type", "__Type", nil, ga("name", "String!")),
	)
	gObject("Mutation", nil,
		gf("createIssue", "CreateIssuePayload", func(_ any, a map[string]any) any {
			in := gInputOf(a, "repositoryId")
			repo := gRepoFromID(in, "repositoryId")
			var labels any
			if ids, ok := in["labelIds"].([]any); ok {
				names := []any{}
				for _, id := range ids {
					switch node := gNodeByID(id).(type) {
					case *Label:
						names = append(names, node.Name)
					case *Repo:
						names = append(names, node.Name)
					}
				}
				labels = names
			}
			var issue *Issue
			gWrapAPI(func() { issue = createIssue(repo, in["title"], in["body"], labels, viewerLogin) })
			return map[string]any{"issue": issue, "clientMutationId": gClientID(a)}
		}, ga("input", "CreateIssueInput!")),
		gf("updateIssue", "UpdateIssuePayload", func(_ any, a map[string]any) any {
			in := gInputOf(a, "id")
			issue := gIssueFromID(in, "id")
			patch := map[string]any{}
			if v, ok := in["title"]; ok {
				patch["title"] = v
			}
			if v, ok := in["body"]; ok {
				patch["body"] = v
			}
			if v, ok := in["state"]; ok {
				patch["state"] = strings.ToLower(jsString(v))
			}
			gWrapAPI(func() { updateIssue(issue, patch) })
			return map[string]any{"issue": issue, "clientMutationId": gClientID(a)}
		}, ga("input", "UpdateIssueInput!")),
		gf("closeIssue", "CloseIssuePayload", func(_ any, a map[string]any) any {
			in := gInputOf(a, "issueId")
			issue := gIssueFromID(in, "issueId")
			var reason *string
			if in["stateReason"] == "NOT_PLANNED" {
				r := "not_planned"
				reason = &r
			}
			setIssueState(issue, "closed", reason)
			return map[string]any{"issue": issue, "clientMutationId": gClientID(a)}
		}, ga("input", "CloseIssueInput!")),
		gf("reopenIssue", "ReopenIssuePayload", func(_ any, a map[string]any) any {
			in := gInputOf(a, "issueId")
			issue := gIssueFromID(in, "issueId")
			setIssueState(issue, "open", nil)
			return map[string]any{"issue": issue, "clientMutationId": gClientID(a)}
		}, ga("input", "ReopenIssueInput!")),
		gf("createPullRequest", "CreatePullRequestPayload", func(_ any, a map[string]any) any {
			in := gInputOf(a, "repositoryId")
			repo := gRepoFromID(in, "repositoryId")
			var pull *Pull
			gWrapAPI(func() {
				pull = createPull(repo, in["title"], in["body"], in["headRefName"], in["baseRefName"], in["draft"], viewerLogin)
			})
			return map[string]any{"pullRequest": pull, "clientMutationId": gClientID(a)}
		}, ga("input", "CreatePullRequestInput!")),
		gf("mergePullRequest", "MergePullRequestPayload", func(_ any, a map[string]any) any {
			in := gInputOf(a, "pullRequestId")
			pull := gPullFromID(in, "pullRequestId")
			method := "squash"
			switch in["mergeMethod"] {
			case "MERGE":
				method = "merge"
			case "REBASE":
				method = "rebase"
			}
			gWrapAPI(func() { mergePull(pull, method, in["commitHeadline"], in["commitBody"], in["expectedHeadOid"]) })
			return map[string]any{"pullRequest": pull, "clientMutationId": gClientID(a)}
		}, ga("input", "MergePullRequestInput!")),
		gf("closePullRequest", "ClosePullRequestPayload", func(_ any, a map[string]any) any {
			in := gInputOf(a, "pullRequestId")
			pull := gPullFromID(in, "pullRequestId")
			closePull(pull)
			return map[string]any{"pullRequest": pull, "clientMutationId": gClientID(a)}
		}, ga("input", "ClosePullRequestInput!")),
		gf("reopenPullRequest", "ReopenPullRequestPayload", func(_ any, a map[string]any) any {
			in := gInputOf(a, "pullRequestId")
			pull := gPullFromID(in, "pullRequestId")
			reopenPull(pull)
			return map[string]any{"pullRequest": pull, "clientMutationId": gClientID(a)}
		}, ga("input", "ReopenPullRequestInput!")),
		gf("createRepository", "CreateRepositoryPayload", func(_ any, a map[string]any) any {
			in := gInputOf(a, "name")
			var repo *Repo
			gWrapAPI(func() { repo = createRepo(in["name"], in["description"], in["visibility"] != "PUBLIC", nil, nil) })
			return map[string]any{"repository": repo, "clientMutationId": gClientID(a)}
		}, ga("input", "CreateRepositoryInput!")),
		gf("addComment", "AddCommentPayload", func(_ any, a map[string]any) any {
			in := gInputOf(a, "subjectId")
			subject := gNodeByID(in["subjectId"])
			issue, isIssue := subject.(*Issue)
			pull, isPull := subject.(*Pull)
			if !isIssue && !isPull {
				gFail("Could not resolve to a node with the global id of '" + gStringOf(in, "subjectId") + "'.")
			}
			var comment *Comment
			gWrapAPI(func() { comment = addComment(issue, pull, in["body"], viewerLogin) })
			count := 0
			if isIssue {
				count = len(issue.Comments)
			} else {
				count = len(pull.Comments)
			}
			edge := map[string]any{"cursor": gEncodeCursor(count - 1), "node": comment}
			return map[string]any{"commentEdge": edge, "subject": subject, "timelineEdge": edge, "clientMutationId": gClientID(a)}
		}, ga("input", "AddCommentInput!")),
	)
	for _, name := range []string{"__Schema", "__Type", "__Field", "__InputValue", "__EnumValue", "__Directive"} {
		gDefine(&gType{name: name, kind: "OBJECT", dynamic: true})
	}
}

// ---------------------------------------------------------------- introspection

func gTypeRef(text string) any {
	if strings.HasSuffix(text, "!") {
		return O("kind", "NON_NULL", "name", nil, "ofType", gTypeRef(text[:len(text)-1]))
	}
	if strings.HasPrefix(text, "[") {
		return O("kind", "LIST", "name", nil, "ofType", gTypeRef(text[1:len(text)-1]))
	}
	if o := gTypeObjects[text]; o != nil {
		return o
	}
	return nil
}

func gInputValue(name, typ string) *Obj {
	return O("name", name, "description", nil, "type", gTypeRef(typ), "defaultValue", nil, "isDeprecated", false, "deprecationReason", nil)
}

func gNamedTypes(names []string) []any {
	out := make([]any, 0, len(names))
	for _, name := range names {
		out = append(out, gTypeRef(name))
	}
	return out
}

func gNewTypeObject(t *gType) *Obj {
	return O("__typename", "__Type", "kind", t.kind, "name", t.name, "description", nil, "specifiedByURL", nil, "isOneOf", false, "ofType", nil,
		"fields", lazyField(func(map[string]any) any {
			if t.kind != "OBJECT" && t.kind != "INTERFACE" {
				return nil
			}
			out := make([]any, 0, len(t.fields))
			for _, f := range t.fields {
				args := make([]any, 0, len(f.args))
				for _, a := range f.args {
					args = append(args, gInputValue(a.name, a.typ))
				}
				out = append(out, O("__typename", "__Field", "name", f.name, "description", nil, "args", args, "type", gTypeRef(f.typ), "isDeprecated", false, "deprecationReason", nil))
			}
			return out
		}),
		"interfaces", lazyField(func(map[string]any) any {
			if t.kind != "OBJECT" {
				return nil
			}
			return gNamedTypes(t.interfaces)
		}),
		"possibleTypes", lazyField(func(map[string]any) any {
			if t.kind != "INTERFACE" && t.kind != "UNION" {
				return nil
			}
			return gNamedTypes(t.possible)
		}),
		"enumValues", lazyField(func(map[string]any) any {
			if t.kind != "ENUM" {
				return nil
			}
			out := make([]any, len(t.values))
			for i, v := range t.values {
				out[i] = O("__typename", "__EnumValue", "name", v, "description", nil, "isDeprecated", false, "deprecationReason", nil)
			}
			return out
		}),
		"inputFields", lazyField(func(map[string]any) any {
			if t.kind != "INPUT_OBJECT" {
				return nil
			}
			out := make([]any, len(t.inputs))
			for i, in := range t.inputs {
				out[i] = gInputValue(in.name, in.typ)
			}
			return out
		}),
	)
}

func gSchemaObject() *Obj {
	directive := func(name string) *Obj {
		return O("__typename", "__Directive", "name", name, "description", nil, "locations", []any{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"},
			"args", []any{gInputValue("if", "Boolean!")}, "isRepeatable", false)
	}
	types := make([]any, 0, len(gTypeOrder))
	for _, name := range gTypeOrder {
		if !strings.HasPrefix(name, "__") {
			types = append(types, gTypeObjects[name])
		}
	}
	return O("__typename", "__Schema", "description", nil, "queryType", gTypeObjects["Query"], "mutationType", gTypeObjects["Mutation"],
		"subscriptionType", nil, "types", types, "directives", []any{directive("include"), directive("skip")})
}

// ---------------------------------------------------------------- validation

func gLoc(line, col int) []any { return []any{O("line", line, "column", col)} }

func gPathWith(path []any, more ...any) []any {
	out := make([]any, 0, len(path)+len(more))
	return append(append(out, path...), more...)
}

func gNamedType(ref string) string {
	return strings.Map(func(r rune) rune {
		if r == '[' || r == ']' || r == '!' {
			return -1
		}
		return r
	}, ref)
}

func gTypeApplies(cond, concrete string) bool {
	if cond == concrete {
		return true
	}
	def := gSchema[cond]
	if def == nil {
		return false
	}
	if def.kind == "INTERFACE" || def.kind == "UNION" {
		return slices.Contains(def.possible, concrete)
	}
	return false
}

func gValidate(doc *gDoc, op *gOp) []any {
	var errs []any
	opLabel := op.typ
	opName := "anonymous operation"
	if op.hasName {
		opLabel += " " + op.name
		opName = op.name
	}
	defined := map[string]bool{}
	for _, v := range op.vars {
		defined[v.name] = true
	}
	used := map[string]bool{}
	var checkValue func(v *gValue, line, col int)
	checkValue = func(v *gValue, line, col int) {
		switch v.k {
		case 'v':
			if !defined[v.name] {
				errs = append(errs, O("message", "Variable $"+v.name+" is used by "+opName+" but not declared", "locations", gLoc(line, col),
					"extensions", O("code", "variableNotDefined", "variableName", v.name)))
			}
		case 'l':
			for _, item := range v.items {
				checkValue(item, line, col)
			}
		case 'o':
			for _, f := range v.fields {
				checkValue(f.value, line, col)
			}
		}
	}
	noSuchType := func(name string) *Obj {
		return O("message", "No such type "+name+", so it can't be a fragment condition", "extensions", O("code", "undefinedType", "typeName", name))
	}
	var walk func(typeName string, sels []*gSel, path []any)
	walk = func(typeName string, sels []*gSel, path []any) {
		def := gSchema[typeName]
		if def == nil || def.dynamic {
			return
		}
		for _, sel := range sels {
			if sel.kind == 'i' {
				cond := typeName
				if sel.hasCond {
					cond = sel.typeCond
				}
				if gSchema[cond] == nil {
					errs = append(errs, noSuchType(cond))
					continue
				}
				walk(cond, sel.sel, path)
				continue
			}
			if sel.kind == 's' {
				frag := doc.frags[sel.name]
				if frag == nil {
					errs = append(errs, O("message", "Fragment "+sel.name+" was used, but not defined", "locations", gLoc(sel.line, sel.col), "path", gPathWith(path),
						"extensions", O("code", "undefinedFragment", "fragmentName", sel.name)))
					continue
				}
				if !used[sel.name] {
					used[sel.name] = true
					if gSchema[frag.typeCond] == nil {
						errs = append(errs, noSuchType(frag.typeCond))
					} else {
						walk(frag.typeCond, frag.sel, path)
					}
				}
				continue
			}
			for _, arg := range sel.args {
				checkValue(arg.value, sel.line, sel.col)
			}
			if sel.name == "__typename" {
				continue
			}
			field := def.byName[sel.name]
			if field == nil {
				errs = append(errs, O("message", "Field '"+sel.name+"' doesn't exist on type '"+typeName+"'", "locations", gLoc(sel.line, sel.col),
					"path", gPathWith(path, sel.name), "extensions", O("code", "undefinedField", "typeName", typeName, "fieldName", sel.name)))
				continue
			}
			for _, arg := range sel.args {
				if !field.hasArg(arg.name) {
					errs = append(errs, O("message", "Field '"+sel.name+"' doesn't accept argument '"+arg.name+"'", "locations", gLoc(sel.line, sel.col),
						"path", gPathWith(path, sel.name, arg.name), "extensions", O("code", "argumentNotAccepted", "name", sel.name, "typeName", typeName, "argumentName", arg.name)))
				}
			}
			inner := gNamedType(field.typ)
			innerDef := gSchema[inner]
			if innerDef == nil {
				continue
			}
			composite := innerDef.kind == "OBJECT" || innerDef.kind == "INTERFACE" || innerDef.kind == "UNION"
			switch {
			case composite && !sel.hasSel:
				errs = append(errs, O("message", "Field must have selections (field '"+sel.name+"' returns "+field.typ+" but has no selections. Did you mean '"+sel.name+" { ... }'?)",
					"locations", gLoc(sel.line, sel.col), "path", gPathWith(path, sel.name), "extensions", O("code", "selectionMismatch", "nodeName", sel.name, "typeName", field.typ)))
			case !composite && sel.hasSel:
				names := make([]string, len(sel.sel))
				for i, s := range sel.sel {
					if s.kind == 'f' {
						names[i] = `"` + s.name + `"`
					} else {
						names[i] = "..."
					}
				}
				errs = append(errs, O("message", "Selections can't be made on scalars (field '"+sel.name+"' returns "+inner+" but has selections ["+strings.Join(names, ", ")+"])",
					"locations", gLoc(sel.line, sel.col), "path", gPathWith(path, sel.name), "extensions", O("code", "selectionMismatch", "nodeName", sel.name, "typeName", inner)))
			case sel.hasSel:
				walk(inner, sel.sel, gPathWith(path, sel.name))
			}
		}
	}
	root := "Query"
	if op.typ == "mutation" {
		root = "Mutation"
	}
	walk(root, op.sel, []any{opLabel})
	return errs
}

// ---------------------------------------------------------------- execution

var errPropagate = errors.New("propagate")

type gExec struct {
	vars   map[string]any
	frags  map[string]*gFragment
	errors []any
	depth  int
}

type gGroup struct {
	key   string
	nodes []*gSel
}

func isUndef(v any) bool {
	_, ok := v.(jsUndefined)
	return ok
}

func gEval(v *gValue, vars map[string]any) any {
	switch v.k {
	case 'v':
		if val, ok := vars[v.name]; ok {
			return val
		}
		return undefined
	case 'n':
		return nil
	case 'l':
		items := make([]any, len(v.items))
		for i, item := range v.items {
			items[i] = gEval(item, vars)
		}
		return items
	case 'o':
		out := map[string]any{}
		for _, f := range v.fields {
			if val := gEval(f.value, vars); !isUndef(val) {
				out[f.name] = val
			}
		}
		return out
	case 'i', 'f':
		return v.num
	case 'b':
		return v.b
	}
	return v.str
}

func (ex *gExec) evalArgs(args []gArgNode) map[string]any {
	out := make(map[string]any, len(args))
	for _, arg := range args {
		if val := gEval(arg.value, ex.vars); !isUndef(val) {
			out[arg.name] = val
		}
	}
	return out
}

func (ex *gExec) allowed(directives []gDirective) bool {
	for _, d := range directives {
		var arg any = undefined
		for _, a := range d.args {
			if a.name == "if" {
				arg = gEval(a.value, ex.vars)
			}
		}
		if d.name == "skip" && arg == true {
			return false
		}
		if d.name == "include" && arg == false {
			return false
		}
	}
	return true
}

func addToGroup(groups *[]*gGroup, index map[string]*gGroup, sel *gSel) {
	key := sel.name
	if sel.hasAlias {
		key = sel.alias
	}
	if g := index[key]; g != nil {
		g.nodes = append(g.nodes, sel)
		return
	}
	g := &gGroup{key: key, nodes: []*gSel{sel}}
	index[key] = g
	*groups = append(*groups, g)
}

func (ex *gExec) collect(typeName string, sels []*gSel, groups *[]*gGroup, index map[string]*gGroup, seen map[string]bool) {
	for _, sel := range sels {
		if !ex.allowed(sel.directives) {
			continue
		}
		switch sel.kind {
		case 'f':
			addToGroup(groups, index, sel)
		case 'i':
			if !sel.hasCond || gTypeApplies(sel.typeCond, typeName) {
				ex.collect(typeName, sel.sel, groups, index, seen)
			}
		case 's':
			if !seen[sel.name] {
				seen[sel.name] = true
				if frag := ex.frags[sel.name]; frag != nil && gTypeApplies(frag.typeCond, typeName) {
					ex.collect(typeName, frag.sel, groups, index, seen)
				}
			}
		}
	}
}

func mergedSelections(nodes []*gSel) []*gSel {
	if len(nodes) == 1 {
		return nodes[0].sel
	}
	var out []*gSel
	for _, n := range nodes {
		out = append(out, n.sel...)
	}
	return out
}

func gPanicError(r any) error {
	switch e := r.(type) {
	case *gFailure:
		return e
	case error:
		return e
	}
	return errors.New(jsString(r))
}

func (ex *gExec) record(err error, node *gSel, path []any) {
	message := "Internal error: " + err.Error()
	if f, ok := err.(*gFailure); ok {
		message = f.msg
	}
	ex.errors = append(ex.errors, O("message", message, "locations", gLoc(node.line, node.col), "path", gPathWith(path)))
}

func (ex *gExec) fieldValue(typeName string, field *gField, g *gGroup, src any, path []any) (value any, err error) {
	defer func() {
		if r := recover(); r != nil {
			value, err = nil, gPanicError(r)
		}
	}()
	node := g.nodes[0]
	args := ex.evalArgs(node.args)
	var resolved any
	switch {
	case typeName == "Query" && node.name == "__schema":
		resolved = gSchemaObject()
	case typeName == "Query" && node.name == "__type":
		if name, ok := args["name"].(string); ok && gSchema[name] != nil && !strings.HasPrefix(name, "__") {
			resolved = gTypeObjects[name]
		}
	case field.resolve != nil:
		resolved = field.resolve(src, args)
	default:
		if m, ok := src.(map[string]any); ok {
			resolved = m[node.name]
		}
	}
	return ex.complete(field.typ, resolved, mergedSelections(g.nodes), path, node)
}

func (ex *gExec) execObject(typeName string, src any, sels []*gSel, path []any) (*Obj, error) {
	def := gSchema[typeName]
	var groups []*gGroup
	ex.collect(typeName, sels, &groups, map[string]*gGroup{}, map[string]bool{})
	out := &Obj{keys: make([]string, 0, len(groups)), vals: make([]any, 0, len(groups))}
	for _, g := range groups {
		node := g.nodes[0]
		out.keys = append(out.keys, g.key)
		if node.name == "__typename" {
			out.vals = append(out.vals, typeName)
			continue
		}
		var field *gField
		if def != nil && def.byName != nil {
			field = def.byName[node.name]
		}
		if field == nil {
			out.vals = append(out.vals, nil)
			continue
		}
		fieldPath := gPathWith(path, g.key)
		value, err := ex.fieldValue(typeName, field, g, src, fieldPath)
		if err != nil {
			if err != errPropagate {
				ex.record(err, node, fieldPath)
			}
			if strings.HasSuffix(field.typ, "!") {
				return nil, errPropagate
			}
			value = nil
		}
		out.vals = append(out.vals, value)
	}
	return out, nil
}

type gRef struct {
	nonNull bool
	list    bool
	inner   *gRef
	name    string
}

func gParseRef(text string) *gRef {
	if strings.HasSuffix(text, "!") {
		r := *gParseRef(text[:len(text)-1])
		r.nonNull = true
		return &r
	}
	if strings.HasPrefix(text, "[") {
		return &gRef{list: true, inner: gParseRef(text[1 : len(text)-1])}
	}
	return &gRef{name: text}
}

func gTypeTextOf(r *gRef) string {
	if r.list {
		inner := gTypeTextOf(r.inner)
		if r.inner.nonNull {
			inner += "!"
		}
		return "[" + inner + "]"
	}
	return r.name
}

func gJoinPath(path []any) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = jsString(p)
	}
	return strings.Join(parts, ".")
}

func (ex *gExec) complete(typeText string, value any, sels []*gSel, path []any, node *gSel) (any, error) {
	ref := gParseRef(typeText)
	if value == nil || isUndef(value) {
		if ref.nonNull {
			ex.errors = append(ex.errors, O("message", "Cannot return null for non-nullable field "+gJoinPath(path), "locations", gLoc(node.line, node.col), "path", gPathWith(path)))
			return nil, errPropagate
		}
		return nil, nil
	}
	if ref.list {
		items, ok := value.([]any)
		if !ok {
			return nil, &gFailure{"Expected an array for " + gJoinPath(path)}
		}
		itemType := gTypeTextOf(ref.inner)
		if ref.inner.nonNull {
			itemType += "!"
		}
		out := make([]any, len(items))
		for i, item := range items {
			v, err := ex.complete(itemType, item, sels, gPathWith(path, i), node)
			if err != nil {
				if err != errPropagate || ref.inner.nonNull {
					return nil, err
				}
				v = nil
			}
			out[i] = v
		}
		return out, nil
	}
	def := gSchema[ref.name]
	if def == nil || def.kind == "SCALAR" || def.kind == "ENUM" {
		return value, nil
	}
	if def.dynamic {
		return ex.completeDynamic(value, sels, path), nil
	}
	concrete := ref.name
	if def.kind == "INTERFACE" || def.kind == "UNION" {
		concrete = gTypeNameOf(value)
	}
	obj, err := ex.execObject(concrete, value, sels, path)
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func (ex *gExec) collectDynamic(sels []*gSel, groups *[]*gGroup, index map[string]*gGroup) {
	ex.depth++
	defer func() { ex.depth-- }()
	if ex.depth > 200 {
		panic(errors.New("Maximum call stack size exceeded"))
	}
	for _, sel := range sels {
		if !ex.allowed(sel.directives) {
			continue
		}
		switch sel.kind {
		case 'f':
			addToGroup(groups, index, sel)
		case 'i':
			ex.collectDynamic(sel.sel, groups, index)
		case 's':
			if frag := ex.frags[sel.name]; frag != nil {
				ex.collectDynamic(frag.sel, groups, index)
			}
		}
	}
}

func (ex *gExec) completeDynamic(value any, sels []*gSel, path []any) any {
	switch v := value.(type) {
	case nil, jsUndefined:
		return nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = ex.completeDynamic(item, sels, gPathWith(path, i))
		}
		return out
	case *Obj:
		var groups []*gGroup
		ex.collectDynamic(sels, &groups, map[string]*gGroup{})
		out := &Obj{keys: make([]string, 0, len(groups)), vals: make([]any, 0, len(groups))}
		for _, g := range groups {
			node := g.nodes[0]
			out.keys = append(out.keys, g.key)
			if node.name == "__typename" {
				name, ok := v.Get("__typename")
				if !ok || name == nil {
					name = "__Type"
				}
				out.vals = append(out.vals, name)
				continue
			}
			item, ok := v.Get(node.name)
			if !ok {
				item = undefined
			}
			if lazy, isLazy := item.(lazyField); isLazy {
				item = lazy(ex.evalArgs(node.args))
			}
			if !node.hasSel {
				if isUndef(item) {
					item = nil
				}
				out.vals = append(out.vals, item)
			} else {
				out.vals = append(out.vals, ex.completeDynamic(item, mergedSelections(g.nodes), gPathWith(path, g.key)))
			}
		}
		return out
	}
	return value
}

func gCoerce(op *gOp, provided map[string]any, providedNull bool) (map[string]any, []any) {
	vars := map[string]any{}
	var errs []any
	invalid := func(def gVarDef) *Obj {
		return O("message", "Variable $"+def.name+" of type "+def.typ+" was provided invalid value",
			"extensions", O("value", nil, "problems", []any{O("path", []any{}, "explanation", "Expected value to not be null")}))
	}
	for _, def := range op.vars {
		if providedNull {
			panic(errors.New("Cannot convert undefined or null to object"))
		}
		nonNull := strings.HasSuffix(def.typ, "!")
		if v, ok := provided[def.name]; ok {
			vars[def.name] = v
		} else if def.def != nil {
			vars[def.name] = gEval(def.def, nil)
		} else if nonNull {
			errs = append(errs, invalid(def))
			continue
		}
		if v, ok := vars[def.name]; ok && v == nil && nonNull {
			errs = append(errs, invalid(def))
		}
	}
	return vars, errs
}

func gErrorsOnly(message string) *Obj {
	return O("errors", []any{O("message", message)})
}

// gRun runs one GraphQL request ({query, variables, operationName}) and returns its response body.
func gRun(request map[string]any) *Obj {
	text, ok := request["query"].(string)
	if !ok || jsTrim(text) == "" {
		return gErrorsOnly("A query attribute must be specified and must be a string.")
	}
	variables := map[string]any{}
	providedNull := false
	if raw, present := request["variables"]; present && raw != nil && raw != "" {
		provided := raw
		if s, isString := provided.(string); isString {
			parsed, err := parseJSON([]byte(s))
			if err != nil {
				return gErrorsOnly("Variables must be provided as a JSON object.")
			}
			provided = parsed
		}
		switch p := provided.(type) {
		case map[string]any:
			variables = p
		case nil:
			providedNull = true
		default:
			return gErrorsOnly("Variables must be provided as a JSON object.")
		}
	}
	doc, synErr := gParse(text)
	if synErr != nil {
		return O("errors", []any{O("message", synErr.msg, "locations", gLoc(synErr.line, synErr.col))})
	}
	if len(doc.ops) == 0 {
		return gErrorsOnly("No operations in query document.")
	}
	var op *gOp
	if wanted, _ := request["operationName"].(string); wanted != "" {
		for _, candidate := range doc.ops {
			if candidate.hasName && candidate.name == wanted {
				op = candidate
				break
			}
		}
		if op == nil {
			return gErrorsOnly(`Unknown operation named "` + wanted + `".`)
		}
	} else if len(doc.ops) == 1 {
		op = doc.ops[0]
	} else {
		return gErrorsOnly("An operation name is required when the document has more than one operation.")
	}
	if errs := gValidate(doc, op); len(errs) > 0 {
		return O("errors", errs)
	}
	vars, errs := gCoerce(op, variables, providedNull)
	if len(errs) > 0 {
		return O("errors", errs)
	}
	if op.typ == "subscription" {
		return gErrorsOnly("Subscriptions are not supported.")
	}
	root := "Query"
	if op.typ == "mutation" {
		root = "Mutation"
	}
	if op.typ == "mutation" || strings.Contains(text, "mergeable") {
		storeMu.Lock()
		defer storeMu.Unlock()
	} else {
		storeMu.RLock()
		defer storeMu.RUnlock()
	}
	ex := &gExec{vars: vars, frags: doc.frags}
	data, err := ex.execObject(root, map[string]any{}, op.sel, []any{})
	var dataValue any = data
	if err != nil {
		dataValue = nil
	}
	if len(ex.errors) > 0 {
		return O("data", dataValue, "errors", ex.errors)
	}
	return O("data", dataValue)
}

// handleGraphQL answers POST /api/graphql: a single request object, or an array of them answered in order.
func handleGraphQL(raw []byte) (frame []byte) {
	defer func() {
		if r := recover(); r != nil {
			frame = errorFrame(500, "Server Error: "+jsString(panicValue(r)), nil, "")
		}
	}()
	body, err := parseJSON(raw)
	if err != nil {
		return errorFrame(400, "Problems parsing JSON", nil, "")
	}
	switch b := body.(type) {
	case []any:
		out := make([]any, len(b))
		for i, entry := range b {
			switch e := entry.(type) {
			case map[string]any:
				out[i] = gRun(e)
			case []any:
				out[i] = gRun(map[string]any{})
			default:
				out[i] = gErrorsOnly("Each batched operation must be a JSON object.")
			}
		}
		return jsonValueFrame(200, out, "")
	case map[string]any:
		return jsonValueFrame(200, gRun(b), "")
	}
	return errorFrame(400, "Problems parsing JSON", nil, "")
}
