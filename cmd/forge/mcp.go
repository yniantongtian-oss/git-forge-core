package main

// Model Context Protocol over streamable HTTP at POST /mcp: JSON-RPC 2.0, the 2026-07-28 revision
// (server/discover, no handshake) and the handshake revisions (initialize + notifications/initialized). The tools
// act on the same store as REST (reads under storeMu.RLock, writes under storeMu.Lock), so what an agent creates
// here reads back there unchanged. tools/list is serialized once, so every answer is byte-identical.

import (
	"encoding/base64"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
)

var mcpServerInfo = O("name", "forge-core", "title", "forge-core git forge", "version", "1.0.0")

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = "A GitHub-compatible forge: create repositories, issues and pull requests, then merge them. Everything is shared with the REST and GraphQL APIs."

type mcpProp struct {
	name   string
	typ    string // string, integer, boolean, array
	enum   []string
	schema *Obj
}

type mcpTool struct {
	name        string
	description string
	props       []mcpProp
	required    []string
	write       bool
	run         func(args map[string]any) string
}

func (t *mcpTool) inputSchema() *Obj {
	props := &Obj{}
	for _, p := range t.props {
		props.Set(p.name, p.schema)
	}
	return O("type", "object", "properties", props, "required", slices.Clone(t.required), "additionalProperties", false)
}

func mcpStr(name, description string) mcpProp {
	return mcpProp{name: name, typ: "string", schema: O("type", "string", "description", description)}
}

func mcpTyped(name, typ, description string) mcpProp {
	return mcpProp{name: name, typ: typ, schema: O("type", typ, "description", description)}
}

func mcpEnum(name, description string, values ...string) mcpProp {
	return mcpProp{name: name, typ: "string", enum: values, schema: O("type", "string", "enum", slices.Clone(values), "description", description)}
}

var (
	mcpOwner = mcpStr("owner", "Repository owner (user login)")
	mcpRepo  = mcpStr("repo", "Repository name")
)

// mcpToolFailure is a tool-level failure reported as an isError result.
type mcpToolFailure struct{ msg string }

// mcpRPCError is a JSON-RPC error answer.
type mcpRPCError struct {
	code int
	msg  string
}

func toolFail(msg string) { panic(&mcpToolFailure{msg}) }

func mcpJSON(v any) string { return string(appendJSON(nil, v)) }

func mcpRepoOf(args map[string]any) *Repo {
	owner := gStringOf(args, "owner")
	var repo *Repo
	if strings.ToLower(owner) == viewerLower {
		repo = getRepo(owner, gStringOf(args, "repo"))
	}
	if repo == nil {
		toolFail("Not Found: repository " + owner + "/" + gStringOf(args, "repo") + " does not exist")
	}
	return repo
}

// jsNumber is Number(value) for the values tool arguments can hold.
func jsNumber(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	case nil:
		return 0
	case string:
		s := jsTrim(x)
		if s == "" {
			return 0
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && !strings.ContainsAny(s, "xXpP_") && !strings.EqualFold(s, "inf") && !strings.EqualFold(s, "infinity") && !strings.EqualFold(s, "nan") {
			return f
		}
		return math.NaN()
	}
	return math.NaN()
}

func mcpNumberOf(args map[string]any, key string) (*Repo, int64) {
	n := math.NaN()
	if v, ok := args[key]; ok {
		n = jsNumber(v)
	}
	if n != math.Trunc(n) || math.IsInf(n, 0) || n < 1 {
		toolFail(key + " must be a positive integer")
	}
	return mcpRepoOf(args), int64(n)
}

func mcpIssueText(issue *Issue) string { return string(issueFrameOf(issue).json) }

var mcpTools = []*mcpTool{
	{
		name: "create_repo", description: "Create a new repository owned by the authenticated user.", write: true,
		props: []mcpProp{mcpStr("name", "Repository name"), mcpStr("description", "Short description"), mcpTyped("private", "boolean", "Whether the repository is private"),
			mcpTyped("autoInit", "boolean", "Create an initial commit with a README")},
		required: []string{"name"},
		run: func(a map[string]any) string {
			return mcpJSON(repoJSON(createRepo(a["name"], a["description"], a["private"], a["autoInit"], nil)))
		},
	},
	{
		name: "get_repo", description: "Get the details of a repository.",
		props: []mcpProp{mcpOwner, mcpRepo}, required: []string{"owner", "repo"},
		run: func(a map[string]any) string { return mcpJSON(repoJSON(mcpRepoOf(a))) },
	},
	{
		name: "create_issue", description: "Create a new issue in a repository.", write: true,
		props: []mcpProp{mcpOwner, mcpRepo, mcpStr("title", "Issue title"), mcpStr("body", "Issue body"),
			{name: "labels", typ: "array", schema: O("type", "array", "items", O("type", "string"), "description", "Names of existing labels")}},
		required: []string{"owner", "repo", "title"},
		run: func(a map[string]any) string {
			return mcpIssueText(createIssue(mcpRepoOf(a), a["title"], a["body"], a["labels"], viewerLogin))
		},
	},
	{
		name: "list_issues", description: "List the issues of a repository, optionally filtered by state.",
		props: []mcpProp{mcpOwner, mcpRepo, mcpEnum("state", "Issue state filter (default open)", "open", "closed", "all"), mcpStr("labels", "Comma separated label names"),
			mcpTyped("perPage", "integer", "Results per page (max 100)")},
		required: []string{"owner", "repo"},
		run: func(a map[string]any) string {
			raw, ok := a["perPage"]
			if !ok || raw == nil {
				raw, ok = a["per_page"]
			}
			n := math.NaN()
			if ok {
				n = jsNumber(raw)
			}
			if n != n || n == 0 {
				n = 30
			}
			per := int(math.Min(100, math.Max(1, n)))
			state, _ := a["state"].(string)
			labels, _ := a["labels"].(string)
			issues := listIssues(mcpRepoOf(a), state, labels, "", "")
			if len(issues) > per {
				issues = issues[:per]
			}
			out := []byte{'['}
			for i, issue := range issues {
				if i > 0 {
					out = append(out, ',')
				}
				out = append(out, issueFrameOf(issue).json...)
			}
			return string(append(out, ']'))
		},
	},
	{
		name: "create_pull_request", description: "Open a pull request from a head branch into a base branch.", write: true,
		props: []mcpProp{mcpOwner, mcpRepo, mcpStr("title", "Pull request title"), mcpStr("head", "Head branch"), mcpStr("base", "Base branch"),
			mcpStr("body", "Pull request body"), mcpTyped("draft", "boolean", "Open as a draft")},
		required: []string{"owner", "repo", "title", "head", "base"},
		run: func(a map[string]any) string {
			return mcpJSON(pullJSON(createPull(mcpRepoOf(a), a["title"], a["body"], a["head"], a["base"], a["draft"], viewerLogin), true))
		},
	},
	{
		name: "merge_pull_request", description: "Merge a pull request (squash by default).", write: true,
		props: []mcpProp{mcpOwner, mcpRepo, mcpTyped("pull_number", "integer", "Pull request number"),
			mcpEnum("merge_method", "Merge method (default squash)", "merge", "squash", "rebase"), mcpStr("commit_title", "Merge commit title"),
			mcpStr("commit_message", "Merge commit message")},
		required: []string{"owner", "repo", "pull_number"},
		run: func(a map[string]any) string {
			repo, number := mcpNumberOf(a, "pull_number")
			pull := repo.pulls[number]
			if pull == nil {
				toolFail("Not Found: pull request #" + strconv.FormatInt(number, 10))
			}
			sha, message := mergePull(pull, a["merge_method"], a["commit_title"], a["commit_message"], nil)
			return mcpJSON(O("sha", sha, "merged", true, "message", message))
		},
	},
	{
		name: "get_issue", description: "Get one issue by number.",
		props: []mcpProp{mcpOwner, mcpRepo, mcpTyped("issue_number", "integer", "Issue number")}, required: []string{"owner", "repo", "issue_number"},
		run: func(a map[string]any) string {
			repo, number := mcpNumberOf(a, "issue_number")
			issue := repo.issues[number]
			if issue == nil {
				toolFail("Not Found: issue #" + strconv.FormatInt(number, 10))
			}
			return mcpIssueText(issue)
		},
	},
	{
		name: "update_issue", description: "Update the title, body or state of an issue.", write: true,
		props: []mcpProp{mcpOwner, mcpRepo, mcpTyped("issue_number", "integer", "Issue number"), mcpStr("title", "New title"), mcpStr("body", "New body"),
			mcpEnum("state", "New state", "open", "closed")},
		required: []string{"owner", "repo", "issue_number"},
		run: func(a map[string]any) string {
			repo, number := mcpNumberOf(a, "issue_number")
			issue := repo.issues[number]
			if issue == nil {
				toolFail("Not Found: issue #" + strconv.FormatInt(number, 10))
			}
			patch := map[string]any{}
			for _, key := range []string{"title", "body", "state"} {
				if v, ok := a[key]; ok {
					patch[key] = v
				}
			}
			updateIssue(issue, patch)
			return mcpIssueText(issue)
		},
	},
	{
		name: "add_issue_comment", description: "Add a comment to an issue or pull request.", write: true,
		props:    []mcpProp{mcpOwner, mcpRepo, mcpTyped("issue_number", "integer", "Issue or pull request number"), mcpStr("body", "Comment text")},
		required: []string{"owner", "repo", "issue_number", "body"},
		run: func(a map[string]any) string {
			repo, number := mcpNumberOf(a, "issue_number")
			issue, pull := findThread(repo, number)
			if issue == nil && pull == nil {
				toolFail("Not Found: #" + strconv.FormatInt(number, 10))
			}
			comment := addComment(issue, pull, a["body"], viewerLogin)
			return mcpJSON(O("id", comment.ID, "body", comment.Body, "user", comment.User, "created_at", comment.CreatedAt))
		},
	},
	{
		name: "list_pull_requests", description: "List the pull requests of a repository.",
		props: []mcpProp{mcpOwner, mcpRepo, mcpEnum("state", "State filter (default open)", "open", "closed", "all")}, required: []string{"owner", "repo"},
		run: func(a map[string]any) string {
			state := "open"
			if s, _ := a["state"].(string); s == "closed" || s == "all" {
				state = s
			}
			out := []any{}
			for _, pull := range mcpRepoOf(a).pullList {
				if state == "all" || pull.State == state {
					out = append(out, pullJSON(pull, false))
				}
			}
			return mcpJSON(out)
		},
	},
	{
		name: "get_pull_request", description: "Get one pull request by number.", write: true,
		props: []mcpProp{mcpOwner, mcpRepo, mcpTyped("pull_number", "integer", "Pull request number")}, required: []string{"owner", "repo", "pull_number"},
		run: func(a map[string]any) string {
			repo, number := mcpNumberOf(a, "pull_number")
			pull := repo.pulls[number]
			if pull == nil {
				toolFail("Not Found: pull request #" + strconv.FormatInt(number, 10))
			}
			return mcpJSON(pullJSON(pull, true))
		},
	},
	{
		name: "get_pull_request_files", description: "List the files changed by a pull request.",
		props: []mcpProp{mcpOwner, mcpRepo, mcpTyped("pull_number", "integer", "Pull request number")}, required: []string{"owner", "repo", "pull_number"},
		run: func(a map[string]any) string {
			repo, number := mcpNumberOf(a, "pull_number")
			pull := repo.pulls[number]
			if pull == nil {
				toolFail("Not Found: pull request #" + strconv.FormatInt(number, 10))
			}
			out := []any{}
			for _, f := range pullFiles(pull) {
				out = append(out, O("filename", f.Filename, "status", f.Status, "additions", f.Additions, "deletions", f.Deletions, "changes", f.Changes, "sha", f.SHA))
			}
			return mcpJSON(out)
		},
	},
	{
		name: "list_branches", description: "List the branches of a repository.",
		props: []mcpProp{mcpOwner, mcpRepo}, required: []string{"owner", "repo"},
		run: func(a map[string]any) string {
			out := []any{}
			for _, ref := range mcpRepoOf(a).RefsWithPrefix("refs/heads/") {
				out = append(out, O("name", ref.Name[11:], "commit", O("sha", ref.OID)))
			}
			return mcpJSON(out)
		},
	},
	{
		name: "create_branch", description: "Create a branch from an existing branch (the default branch unless from_branch is given).", write: true,
		props: []mcpProp{mcpOwner, mcpRepo, mcpStr("branch", "New branch name"), mcpStr("from_branch", "Source branch")}, required: []string{"owner", "repo", "branch"},
		run: func(a map[string]any) string {
			repo := mcpRepoOf(a)
			from := repo.DefaultBranch
			if s, ok := a["from_branch"].(string); ok {
				from = s
			}
			source := repo.ResolveCommit(from)
			if source == "" {
				toolFail("Not Found: the source branch does not exist")
			}
			ref := "refs/heads/" + gStringOf(a, "branch")
			createRef(repo, ref, source)
			return mcpJSON(O("ref", ref, "object", O("sha", source)))
		},
	},
	{
		name: "get_file_contents", description: "Get the contents of a file or directory.",
		props: []mcpProp{mcpOwner, mcpRepo, mcpStr("path", "Path within the repository"), mcpStr("ref", "Branch, tag or commit")}, required: []string{"owner", "repo", "path"},
		run: func(a map[string]any) string {
			repo := mcpRepoOf(a)
			ref, hasRef := a["ref"].(string)
			file, entries, dir := getContents(repo, gStringOf(a, "path"), ref, hasRef)
			if dir {
				out := make([]any, len(entries))
				for i, e := range entries {
					out[i] = O("type", e.Type, "name", e.Name, "path", e.Path, "sha", e.OID, "size", e.Size)
				}
				return mcpJSON(out)
			}
			return mcpJSON(O("type", "file", "path", file.Path, "sha", file.OID, "size", len(file.Data), "encoding", "base64",
				"content", base64.StdEncoding.EncodeToString(file.Data), "text", string(file.Data)))
		},
	},
	{
		name: "create_or_update_file", description: "Create or update a file with a commit.", write: true,
		props: []mcpProp{mcpOwner, mcpRepo, mcpStr("path", "File path"), mcpStr("content", "File content (plain text)"), mcpStr("message", "Commit message"),
			mcpStr("branch", "Target branch"), mcpStr("sha", "Blob sha of the file being replaced")},
		required: []string{"owner", "repo", "path", "content", "message"},
		run: func(a map[string]any) string {
			repo := mcpRepoOf(a)
			done := putContents(repo, gStringOf(a, "path"), contentsInput{message: a["message"], content: []byte(gStringOf(a, "content")), sha: a["sha"], branch: a["branch"]})
			return mcpJSON(O("created", done.Created, "commit", O("sha", done.Commit), "content", O("path", done.Path, "sha", done.Blob)))
		},
	},
	{
		name: "delete_file", description: "Delete a file with a commit.", write: true,
		props: []mcpProp{mcpOwner, mcpRepo, mcpStr("path", "File path"), mcpStr("message", "Commit message"), mcpStr("sha", "Blob sha of the file"),
			mcpStr("branch", "Target branch")},
		required: []string{"owner", "repo", "path", "message", "sha"},
		run: func(a map[string]any) string {
			done := deleteContents(mcpRepoOf(a), gStringOf(a, "path"), a["message"], a["sha"], a["branch"])
			return mcpJSON(O("commit", O("sha", done.Commit)))
		},
	},
	{
		name: "list_commits", description: "List commits reachable from a branch, tag or commit.",
		props: []mcpProp{mcpOwner, mcpRepo, mcpStr("sha", "Branch, tag or commit (default branch when omitted)")}, required: []string{"owner", "repo"},
		run: func(a map[string]any) string {
			repo := mcpRepoOf(a)
			name := "HEAD"
			if s, ok := a["sha"].(string); ok && s != "" {
				name = s
			}
			tip := repo.ResolveCommit(name)
			if tip == "" {
				toolFail("Not Found: no such commit")
			}
			out := []any{}
			for _, oid := range repo.ListCommits(tip, 30, nil) {
				out = append(out, commitJSON(repo, oid, false))
			}
			return mcpJSON(out)
		},
	},
	{
		name: "get_me", description: "Get the authenticated user.", props: []mcpProp{}, required: []string{},
		run: func(map[string]any) string { return mcpJSON(userJSON(viewerLogin)) },
	},
}

var (
	mcpToolByName = map[string]*mcpTool{}
	mcpToolsList  RawJSON // {"tools":[...]} serialized once
)

func init() {
	list := make([]any, len(mcpTools))
	for i, tool := range mcpTools {
		mcpToolByName[tool.name] = tool
		list[i] = O("name", tool.name, "title", strings.ReplaceAll(tool.name, "_", " "), "description", tool.description, "inputSchema", tool.inputSchema())
	}
	mcpToolsList = RawJSON(appendJSON(nil, O("tools", list)))
}

// mcpTypeProblem checks the arguments against the tool's input schema; "" when they fit.
func mcpTypeProblem(tool *mcpTool, args map[string]any) string {
	for _, key := range tool.required {
		if v, ok := args[key]; !ok || v == nil {
			return "Missing required argument: " + key
		}
	}
	for _, p := range tool.props {
		value, ok := args[p.name]
		if !ok || value == nil {
			continue
		}
		switch p.typ {
		case "string":
			if _, isString := value.(string); !isString {
				return "Argument " + p.name + " must be a string"
			}
		case "integer":
			integral := false
			switch v := value.(type) {
			case float64:
				integral = v == math.Trunc(v) && !math.IsInf(v, 0)
			case string:
				integral = v != "" && strings.Trim(v, "0123456789") == ""
			}
			if !integral {
				return "Argument " + p.name + " must be an integer"
			}
		case "boolean":
			if _, isBool := value.(bool); !isBool {
				return "Argument " + p.name + " must be a boolean"
			}
		case "array":
			if _, isList := value.([]any); !isList {
				return "Argument " + p.name + " must be an array"
			}
		}
		if p.enum != nil {
			s, isString := value.(string)
			if !isString || !slices.Contains(p.enum, s) {
				return "Argument " + p.name + " must be one of " + strings.Join(p.enum, ", ")
			}
		}
	}
	return ""
}

func mcpToolResult(text string, isError bool) *Obj {
	result := O("content", []any{O("type", "text", "text", text)})
	if isError {
		result.Set("isError", true)
	}
	return result
}

func mcpRunTool(tool *mcpTool, args map[string]any) (result *Obj) {
	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case *mcpToolFailure:
				result = mcpToolResult(e.msg, true)
			case *ApiError:
				detail := ""
				if e.Errors != nil {
					detail = " " + string(appendJSON(nil, e.Errors))
				}
				result = mcpToolResult(strconv.Itoa(e.Status)+" "+e.Message+detail, true)
			default:
				result = mcpToolResult("Internal error: "+jsString(panicValue(r)), true)
			}
		}
	}()
	if tool.write {
		storeMu.Lock()
		defer storeMu.Unlock()
	} else {
		storeMu.RLock()
		defer storeMu.RUnlock()
	}
	return mcpToolResult(tool.run(args), false)
}

func mcpCallTool(params any) *Obj {
	p, _ := params.(map[string]any)
	name, _ := p["name"].(string)
	tool := mcpToolByName[name]
	if tool == nil {
		panic(&mcpRPCError{-32602, "Unknown tool: " + name})
	}
	args := map[string]any{}
	if raw, present := p["arguments"]; present && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			panic(&mcpRPCError{-32602, "arguments must be an object"})
		}
		args = m
	}
	if problem := mcpTypeProblem(tool, args); problem != "" {
		return mcpToolResult(problem, true)
	}
	return mcpRunTool(tool, args)
}

var mcpSessionCounter atomic.Int64

func mcpDispatch(method string, params any) (any, string) {
	switch method {
	case "server/discover":
		return O("resultType", "complete", "supportedVersions", slices.Clone(mcpSupportedVersions), "capabilities", O("tools", O("listChanged", false)),
			"serverInfo", mcpServerInfo, "instructions", mcpInstructions, "_meta", O("io.modelcontextprotocol/serverInfo", mcpServerInfo)), ""
	case "initialize":
		asked := ""
		if p, ok := params.(map[string]any); ok {
			asked, _ = p["protocolVersion"].(string)
		}
		chosen := "2025-11-25"
		if slices.Contains(mcpSupportedVersions, asked) && asked != "2026-07-28" {
			chosen = asked
		}
		session := mcpSessionCounter.Add(1)
		return O("protocolVersion", chosen, "capabilities", O("tools", O("listChanged", false)), "serverInfo", mcpServerInfo, "instructions", mcpInstructions),
			"Mcp-Session-Id: forge-session-" + strconv.FormatInt(session, 10) + "\r\n"
	case "ping", "logging/setLevel":
		return O(), ""
	case "tools/list":
		return mcpToolsList, ""
	case "tools/call":
		return mcpCallTool(params), ""
	case "resources/list":
		return O("resources", []any{}), ""
	case "resources/templates/list":
		return O("resourceTemplates", []any{}), ""
	case "prompts/list":
		return O("prompts", []any{}), ""
	}
	panic(&mcpRPCError{-32601, "Method not found: " + method})
}

func mcpErrorBody(id any, code int, message string) *Obj {
	return O("jsonrpc", "2.0", "id", id, "error", O("code", code, "message", message))
}

// mcpAnswer answers one JSON-RPC message; a nil body means nothing to send (a notification).
func mcpAnswer(message any) (body *Obj, headers string) {
	m, isObject := message.(map[string]any)
	method, hasMethod := m["method"].(string)
	version, hasVersion := m["jsonrpc"]
	if !isObject || !hasMethod || (hasVersion && version != "2.0") {
		var id any
		if isObject {
			id = m["id"]
		}
		return mcpErrorBody(id, -32600, "Invalid Request"), ""
	}
	id, hasID := m["id"]
	defer func() {
		if r := recover(); r != nil {
			body, headers = nil, ""
			if !hasID {
				return
			}
			if rpcErr, ok := r.(*mcpRPCError); ok {
				body = mcpErrorBody(id, rpcErr.code, rpcErr.msg)
			} else {
				body = mcpErrorBody(id, -32603, "Internal error: "+jsString(panicValue(r)))
			}
		}
	}()
	result, extra := mcpDispatch(method, m["params"])
	if !hasID {
		return nil, ""
	}
	return O("jsonrpc", "2.0", "id", id, "result", result), extra
}

func mcpSend(status int, body any, extra, accept string) []byte {
	payload := appendJSON(make([]byte, 0, 512), body)
	if strings.Contains(accept, "text/event-stream") && !strings.Contains(accept, "application/json") {
		framed := make([]byte, 0, len(payload)+24)
		framed = append(framed, "event: message\ndata: "...)
		framed = append(framed, payload...)
		framed = append(framed, "\n\n"...)
		return HTTPFrame(status, "text/event-stream", "Cache-Control: no-cache\r\n"+extra, framed)
	}
	return HTTPFrame(status, "application/json; charset=utf-8", "Cache-Control: no-cache\r\n"+extra, payload)
}

// handleMCP answers POST /mcp (authentication has already been checked).
func handleMCP(raw []byte, accept string) []byte {
	message, err := parseJSON(raw)
	if err != nil {
		return mcpSend(400, mcpErrorBody(nil, -32700, "Parse error"), "", accept)
	}
	switch m := message.(type) {
	case []any:
		if len(m) == 0 {
			return mcpSend(400, mcpErrorBody(nil, -32600, "Invalid Request"), "", accept)
		}
		bodies := []any{}
		for _, entry := range m {
			if body, _ := mcpAnswer(entry); body != nil {
				bodies = append(bodies, body)
			}
		}
		if len(bodies) == 0 {
			return HTTPFrame(202, "", "", nil)
		}
		return mcpSend(200, bodies, "", accept)
	case map[string]any:
		body, extra := mcpAnswer(m)
		if body == nil {
			return HTTPFrame(202, "", "", nil)
		}
		return mcpSend(200, body, extra, accept)
	}
	return mcpSend(400, mcpErrorBody(nil, -32600, "Invalid Request"), "", accept)
}
