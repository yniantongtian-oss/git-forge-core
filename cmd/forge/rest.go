package main

// GitHub REST v3 under /api/v3: repositories, branches, commits, git refs and objects, contents, labels, issues,
// comments and pull requests. Issue JSON is serialized once (by direct byte appending) into a complete HTTP
// reply and cached on the issue, so the hot read path is a lookup, a copy and one write.

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
	"sync/atomic"
)

// ---------------------------------------------------------------- JSON shapes

func userJSON(login string) *Obj {
	id := int64(1_000_000 + utf16Len(login))
	if login == viewerLogin {
		id = viewerID
	}
	b := base()
	return O("login", login, "id", id, "node_id", globalID("User", id), "avatar_url", b+"/avatars/u/"+strconv.FormatInt(id, 10),
		"gravatar_id", "", "url", b+"/api/v3/users/"+login, "html_url", b+"/"+login, "type", "User", "site_admin", false)
}

func repoJSON(repo *Repo) *Obj {
	b := base()
	full := repo.Owner + "/" + repo.Name
	url := b + "/api/v3/repos/" + full
	var pushed any
	if repo.PushedAt != 0 {
		pushed = ISO(repo.PushedAt)
	}
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	return O(
		"id", repo.ID, "node_id", globalID("Repository", repo.ID), "name", repo.Name, "full_name", full,
		"private", repo.Private, "owner", userJSON(repo.Owner), "html_url", b+"/"+full, "description", strOrNil(repo.Description),
		"fork", false, "url", url, "branches_url", url+"/branches{/branch}", "commits_url", url+"/commits{/sha}",
		"contents_url", url+"/contents/{+path}", "issues_url", url+"/issues{/number}", "labels_url", url+"/labels{/name}",
		"pulls_url", url+"/pulls{/number}", "git_url", "git://"+strings.TrimPrefix(b, "http://")+"/"+full+".git",
		"clone_url", b+"/"+full+".git", "created_at", repo.CreatedAt, "updated_at", repo.UpdatedAt, "pushed_at", pushed,
		"homepage", nil, "size", 0, "stargazers_count", 0, "watchers_count", 0, "language", nil,
		"has_issues", repo.HasIssues, "has_projects", true, "has_downloads", true, "has_wiki", true, "has_pages", false,
		"forks_count", 0, "archived", repo.Archived, "disabled", false,
		"open_issues_count", repo.openIssueCount()+repo.openPullCount(), "license", nil, "allow_forking", true,
		"is_template", false, "topics", []any{}, "visibility", visibility, "forks", 0, "open_issues", repo.openIssueCount(),
		"watchers", 0, "default_branch", repo.DefaultBranch,
		"permissions", O("admin", true, "maintain", true, "push", true, "triage", true, "pull", true),
	)
}

func labelJSON(repo *Repo, label *Label) *Obj {
	return O("id", label.ID, "node_id", globalID("Label", label.ID),
		"url", base()+"/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/labels/"+jsEncodeURIComponent(label.Name),
		"name", label.Name, "color", label.Color, "default", false, "description", strOrNil(label.Description))
}

func labelList(repo *Repo, labels []*Label) []any {
	out := make([]any, len(labels))
	for i, label := range labels {
		out[i] = labelJSON(repo, label)
	}
	return out
}

type userJSONCache struct {
	base string
	json []byte
}

// viewerJSON caches userJSON(viewerLogin) serialized for the base URL it was built with.
var viewerJSON atomic.Pointer[userJSONCache]

func appendUserJSON(dst []byte, login string) []byte {
	if login != viewerLogin {
		id := int64(1_000_000 + utf16Len(login))
		return appendJSON(dst, O("login", login, "id", id, "node_id", globalID("User", id), "type", "User", "site_admin", false))
	}
	b := base()
	if cached := viewerJSON.Load(); cached != nil && cached.base == b {
		return append(dst, cached.json...)
	}
	serialized := appendJSON(nil, O("login", login, "id", viewerID, "node_id", globalID("User", viewerID), "type", "User", "site_admin", false))
	viewerJSON.Store(&userJSONCache{base: b, json: serialized})
	return append(dst, serialized...)
}

func appendKey(dst []byte, key string) []byte {
	dst = append(dst, ',', '"')
	dst = append(dst, key...)
	return append(dst, '"', ':')
}

func appendStrPtr(dst []byte, s *string) []byte {
	if s == nil {
		return append(dst, "null"...)
	}
	return appendJSONString(dst, *s)
}

func appendIssueNodeID(dst []byte, id int64) []byte {
	var raw [32]byte
	b := append(raw[:0], "005:Issue"...)
	b = strconv.AppendInt(b, id, 10)
	var enc [48]byte
	n := base64.RawStdEncoding.EncodedLen(len(b))
	base64.RawStdEncoding.Encode(enc[:n], b)
	dst = append(dst, '"')
	dst = append(dst, enc[:n]...)
	return append(dst, '"')
}

// appendIssueJSON serializes an issue's REST v3 representation directly into dst with zero intermediate URL strings.
func appendIssueJSON(dst []byte, issue *Issue) []byte {
	repo := issue.Repo
	b := base()
	dst = append(dst, `{"url":"`...)
	dst = append(dst, b...)
	dst = append(dst, "/api/v3/repos/"...)
	dst = append(dst, repo.Owner...)
	dst = append(dst, '/')
	dst = append(dst, repo.Name...)
	dst = append(dst, "/issues/"...)
	dst = strconv.AppendInt(dst, issue.Number, 10)
	dst = append(dst, `","html_url":"`...)
	dst = append(dst, b...)
	dst = append(dst, '/')
	dst = append(dst, repo.Owner...)
	dst = append(dst, '/')
	dst = append(dst, repo.Name...)
	dst = append(dst, "/issues/"...)
	dst = strconv.AppendInt(dst, issue.Number, 10)
	dst = append(dst, `","id":`...)
	dst = strconv.AppendInt(dst, issue.ID, 10)
	dst = append(dst, `,"node_id":`...)
	dst = appendIssueNodeID(dst, issue.ID)
	dst = append(dst, `,"number":`...)
	dst = strconv.AppendInt(dst, issue.Number, 10)
	dst = appendKey(dst, "title")
	dst = appendJSONString(dst, issue.Title)
	dst = appendKey(dst, "user")
	dst = appendUserJSON(dst, issue.User)
	dst = appendKey(dst, "labels")
	if len(issue.Labels) == 0 {
		dst = append(dst, "[]"...)
	} else {
		dst = appendJSON(dst, labelList(repo, issue.Labels))
	}
	dst = appendKey(dst, "state")
	dst = appendJSONString(dst, issue.State)
	dst = append(dst, `,"locked":false,"comments":`...)
	dst = strconv.AppendInt(dst, int64(len(issue.Comments)), 10)
	dst = append(dst, `,"created_at":"`...)
	dst = append(dst, issue.CreatedAt...)
	dst = append(dst, `","updated_at":"`...)
	dst = append(dst, issue.UpdatedAt...)
	dst = append(dst, `","closed_at":`...)
	dst = appendStrPtr(dst, issue.ClosedAt)
	dst = appendKey(dst, "body")
	if issue.Body != nil {
		dst = appendJSONString(dst, *issue.Body)
	} else {
		dst = append(dst, `""`...)
	}
	dst = appendKey(dst, "state_reason")
	dst = appendStrPtr(dst, issue.StateReason)
	return append(dst, '}')
}

const ridPlaceholder = "X-GitHub-Request-Id: FEED:0000:0000:0000A1:00000001\r\n"

// issueFrameOf returns the issue's cached reply, building it on first use after a change. Callers hold storeMu.
func issueFrameOf(issue *Issue) *issueFrame {
	if f := issue.frame.Load(); f != nil {
		return f
	}
	body := appendIssueJSON(make([]byte, 0, 2048), issue)
	full := AppendHTTPHead(make([]byte, 0, 256+len(body)), 200, jsonContentType, len(body), ridPlaceholder)
	ridAt := bytes.Index(full, []byte("FEED:")) + 5
	headLen := len(full)
	full = append(full, body...)
	f := &issueFrame{http: full, json: full[headLen:], ridAt: ridAt}
	if issue.frame.CompareAndSwap(nil, f) {
		return f
	}
	if cur := issue.frame.Load(); cur != nil {
		return cur
	}
	return f
}

// appendIssueReply copies the cached reply into dst with the next request id patched in (no allocation when dst
// has room).
func appendIssueReply(dst []byte, f *issueFrame) []byte {
	n := requestCounter.Add(1)
	if n > 0xFFFFFFFF {
		return append(dst, jsonFrame(200, f.json, "")...)
	}
	start := len(dst)
	dst = append(dst, f.http...)
	at := start + f.ridAt
	for i := 7; i >= 0; i-- {
		pos := at + i
		if i >= 4 {
			pos++ // skip the ':' between the two halves
		}
		dst[pos] = upperHex[n&15]
		n >>= 4
	}
	return dst
}

func issueReply(issue *Issue) []byte {
	f := issueFrameOf(issue)
	return appendIssueReply(make([]byte, 0, len(f.http)), f)
}

func commentJSON(comment *Comment) *Obj {
	repo := comment.repo()
	b := base()
	repoAPI := b + "/api/v3/repos/" + repo.Owner + "/" + repo.Name
	id := strconv.FormatInt(comment.ID, 10)
	num := strconv.FormatInt(comment.number(), 10)
	return O("url", repoAPI+"/issues/comments/"+id, "html_url", b+"/"+repo.Owner+"/"+repo.Name+"/issues/"+num+"#issuecomment-"+id,
		"issue_url", repoAPI+"/issues/"+num, "id", comment.ID, "node_id", globalID("IssueComment", comment.ID),
		"user", userJSON(comment.User), "created_at", comment.CreatedAt, "updated_at", comment.UpdatedAt,
		"author_association", "OWNER", "body", comment.Body)
}

func identJSON(id Ident) *Obj {
	return O("name", id.Name, "email", id.Email, "date", ISO(id.When))
}

func trimMessage(message string) string {
	return strings.TrimSuffix(message, "\n")
}

func shortHexID(oid string) int64 {
	if len(oid) < 8 {
		return 0
	}
	v, _ := strconv.ParseInt(oid[:8], 16, 64)
	return v
}

func commitJSON(repo *Repo, oid string, withFiles bool) any {
	info := repo.Commit(oid)
	if info == nil {
		return nil
	}
	b := base()
	repoAPI := b + "/api/v3/repos/" + repo.Owner + "/" + repo.Name
	parents := make([]any, len(info.Parents))
	for i, parent := range info.Parents {
		parents[i] = O("sha", parent, "url", repoAPI+"/commits/"+parent, "html_url", b+"/"+repo.Owner+"/"+repo.Name+"/commit/"+parent)
	}
	var author, committer any
	if info.Author.Name == viewerLogin {
		author = userJSON(viewerLogin)
	}
	if info.Committer.Name == viewerLogin {
		committer = userJSON(viewerLogin)
	}
	out := O("sha", oid, "node_id", globalID("Commit", shortHexID(oid)),
		"commit", O("author", identJSON(info.Author), "committer", identJSON(info.Committer), "message", trimMessage(info.Message),
			"tree", O("sha", info.Tree, "url", repoAPI+"/git/trees/"+info.Tree), "url", repoAPI+"/git/commits/"+oid,
			"comment_count", 0, "verification", O("verified", false, "reason", "unsigned", "signature", nil, "payload", nil)),
		"url", repoAPI+"/commits/"+oid, "html_url", b+"/"+repo.Owner+"/"+repo.Name+"/commit/"+oid,
		"comments_url", repoAPI+"/commits/"+oid+"/comments", "author", author, "committer", committer, "parents", parents)
	if withFiles {
		baseTree := ""
		if len(info.Parents) > 0 {
			if parent := repo.Commit(info.Parents[0]); parent != nil {
				baseTree = parent.Tree
			}
		}
		files := repo.DiffTrees(baseTree, info.Tree)
		total, adds, dels := 0, 0, 0
		list := make([]any, len(files))
		for i := range files {
			total += files[i].Changes
			adds += files[i].Additions
			dels += files[i].Deletions
			list[i] = fileJSON(repo, &files[i], oid)
		}
		out.Set("stats", O("total", total, "additions", adds, "deletions", dels))
		out.Set("files", list)
	}
	return out
}

func fileJSON(repo *Repo, file *FileDiff, ref string) *Obj {
	b := base()
	out := O("sha", file.SHA, "filename", file.Filename, "status", file.Status, "additions", file.Additions,
		"deletions", file.Deletions, "changes", file.Changes,
		"blob_url", b+"/"+repo.Owner+"/"+repo.Name+"/blob/"+ref+"/"+file.Filename,
		"raw_url", b+"/"+repo.Owner+"/"+repo.Name+"/raw/"+ref+"/"+file.Filename,
		"contents_url", b+"/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/contents/"+file.Filename+"?ref="+ref)
	if file.HasPatch {
		out.Set("patch", file.Patch)
	}
	return out
}

func pullJSON(pull *Pull, full bool) *Obj {
	repo := pull.Repo
	b := base()
	repoAPI := b + "/api/v3/repos/" + repo.Owner + "/" + repo.Name
	num := strconv.FormatInt(pull.Number, 10)
	side := func(ref, sha string) *Obj {
		return O("label", repo.Owner+":"+ref, "ref", ref, "sha", sha, "user", userJSON(repo.Owner), "repo", repoJSON(repo))
	}
	body := ""
	if pull.Body != nil {
		body = *pull.Body
	}
	out := O(
		"url", repoAPI+"/pulls/"+num, "id", pull.ID, "node_id", globalID("PullRequest", pull.ID),
		"html_url", b+"/"+repo.Owner+"/"+repo.Name+"/pull/"+num, "diff_url", b+"/"+repo.Owner+"/"+repo.Name+"/pull/"+num+".diff",
		"patch_url", b+"/"+repo.Owner+"/"+repo.Name+"/pull/"+num+".patch", "issue_url", repoAPI+"/issues/"+num,
		"number", pull.Number, "state", pull.State, "locked", false, "title", pull.Title, "user", userJSON(pull.User), "body", body,
		"created_at", pull.CreatedAt, "updated_at", pull.UpdatedAt, "closed_at", strOrNil(pull.ClosedAt),
		"merged_at", strOrNil(pull.MergedAt), "merge_commit_sha", strOrNil(pull.MergeCommitSha), "assignee", nil,
		"assignees", []any{}, "requested_reviewers", []any{}, "labels", labelList(repo, pull.Labels), "milestone", nil,
		"draft", pull.Draft, "commits_url", repoAPI+"/pulls/"+num+"/commits", "comments_url", repoAPI+"/issues/"+num+"/comments",
		"head", side(pull.HeadRef, pullHeadSha(pull)), "base", side(pull.BaseRef, pullBaseSha(pull)),
		"author_association", "OWNER", "auto_merge", nil, "active_lock_reason", nil, "merged", pull.Merged,
	)
	if full {
		mergeable := pullMergeable(pull)
		files := pullFiles(pull)
		var m any
		state := "unknown"
		switch mergeable {
		case 1:
			m = true
			state = "clean"
		case 0:
			m = false
			state = "dirty"
		}
		if pull.Merged {
			state = "unknown"
		}
		var mergedBy any
		if pull.Merged {
			mergedBy = userJSON(viewerLogin)
		}
		adds, dels := 0, 0
		for i := range files {
			adds += files[i].Additions
			dels += files[i].Deletions
		}
		out.Set("mergeable", m)
		out.Set("rebaseable", m)
		out.Set("mergeable_state", state)
		out.Set("merged_by", mergedBy)
		out.Set("comments", len(pull.Comments))
		out.Set("review_comments", 0)
		out.Set("maintainer_can_modify", false)
		out.Set("commits", len(pullCommits(pull)))
		out.Set("additions", adds)
		out.Set("deletions", dels)
		out.Set("changed_files", len(files))
	}
	return out
}

func pullAsIssue(pull *Pull) *Obj {
	repo := pull.Repo
	b := base()
	repoAPI := b + "/api/v3/repos/" + repo.Owner + "/" + repo.Name
	num := strconv.FormatInt(pull.Number, 10)
	body := ""
	if pull.Body != nil {
		body = *pull.Body
	}
	return O("url", repoAPI+"/issues/"+num, "repository_url", repoAPI, "html_url", b+"/"+repo.Owner+"/"+repo.Name+"/pull/"+num,
		"id", pull.ID, "node_id", globalID("PullRequest", pull.ID), "number", pull.Number, "title", pull.Title,
		"user", userJSON(pull.User), "labels", labelList(repo, pull.Labels), "state", pull.State, "locked", false,
		"comments", len(pull.Comments), "created_at", pull.CreatedAt, "updated_at", pull.UpdatedAt,
		"closed_at", strOrNil(pull.ClosedAt), "body", body)
}

func branchJSON(repo *Repo, name, oid string) *Obj {
	return O("name", name, "commit", O("sha", oid, "url", base()+"/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/commits/"+oid), "protected", false)
}

func refJSON(repo *Repo, name, oid string) *Obj {
	typ := "commit"
	if obj := repo.GetObject(oid); obj != nil {
		typ = obj.Type.String()
	}
	repoAPI := base() + "/api/v3/repos/" + repo.Owner + "/" + repo.Name
	return O("ref", name, "node_id", globalID("Ref", shortHexID(oid)), "url", repoAPI+"/git/"+name,
		"object", O("sha", oid, "type", typ, "url", repoAPI+"/git/"+typ+"s/"+oid))
}

func contentCommitJSON(repo *Repo, oid string) *Obj {
	info := repo.Commit(oid)
	b := base()
	repoAPI := b + "/api/v3/repos/" + repo.Owner + "/" + repo.Name
	out := O("sha", oid, "node_id", globalID("Commit", shortHexID(oid)), "url", repoAPI+"/git/commits/"+oid,
		"html_url", b+"/"+repo.Owner+"/"+repo.Name+"/commit/"+oid)
	parents := []any{}
	if info == nil {
		out.Set("author", nil).Set("committer", nil).Set("tree", nil).Set("message", "")
	} else {
		out.Set("author", identJSON(info.Author)).Set("committer", identJSON(info.Committer))
		out.Set("tree", O("sha", info.Tree, "url", repoAPI+"/git/trees/"+info.Tree)).Set("message", trimMessage(info.Message))
		for _, parent := range info.Parents {
			parents = append(parents, O("sha", parent, "url", repoAPI+"/git/commits/"+parent, "html_url", b+"/"+repo.Owner+"/"+repo.Name+"/commit/"+parent))
		}
	}
	return out.Set("parents", parents)
}

func fileEntryJSON(repo *Repo, path, oid string, size int, typ, ref string) *Obj {
	b := base()
	repoAPI := b + "/api/v3/repos/" + repo.Owner + "/" + repo.Name
	name := path[strings.LastIndexByte(path, '/')+1:]
	var download any
	if typ == "file" {
		download = b + "/" + repo.Owner + "/" + repo.Name + "/raw/" + ref + "/" + path
	}
	return O("type", typ, "name", name, "path", path, "sha", oid, "size", size, "url", repoAPI+"/contents/"+path+"?ref="+ref,
		"html_url", b+"/"+repo.Owner+"/"+repo.Name+"/blob/"+ref+"/"+path, "git_url", repoAPI+"/git/blobs/"+oid, "download_url", download)
}

// ---------------------------------------------------------------- helpers

type restReq struct {
	method  string
	path    string
	search  string
	host    string
	hasHost bool
	body    []byte
}

func notFoundFrame() []byte {
	return errorFrame(404, "Not Found", nil, "")
}

func jsonBody(rq *restReq) map[string]any {
	if len(rq.body) == 0 {
		return map[string]any{}
	}
	parsed, err := parseJSON(rq.body)
	if err != nil {
		panic(&HttpError{Status: 400, Message: "Problems parsing JSON"})
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		panic(&HttpError{Status: 400, Message: "Problems parsing JSON"})
	}
	return obj
}

// positive is parseInt(value, 10) when it is a finite number >= 1, capped at max; otherwise fallback.
func positive(q *query, key string, fallback, max int) int {
	value, ok := q.get(key)
	if !ok {
		return fallback
	}
	n := jsParseInt(value)
	if n != n || n > 1e300 || n < 1 {
		return fallback
	}
	if n > float64(max) {
		return max
	}
	return int(n)
}

func pageURL(rq *restReq, q *query, page int) string {
	var params []string
	seen := false
	for _, key := range q.objectKeys() {
		if key == "page" {
			params = append(params, "page="+strconv.Itoa(page))
			seen = true
		} else {
			value, _ := q.get(key)
			params = append(params, jsEncodeURIComponent(key)+"="+jsEncodeURIComponent(value))
		}
	}
	if !seen {
		params = append(params, "page="+strconv.Itoa(page))
	}
	host := strings.TrimPrefix(base(), "http://")
	if rq.hasHost {
		host = rq.host
	}
	return "http://" + host + rq.path + "?" + strings.Join(params, "&")
}

// paginate picks the page of total items the query asks for and the Link header (as an extra header line).
func paginate(rq *restReq, q *query, total int) (lo, hi int, extra string) {
	perPage := positive(q, "per_page", 30, 100)
	page := positive(q, "page", 1, 1_000_000)
	last := max(1, (total+perPage-1)/perPage)
	lo = min(total, (page-1)*perPage)
	hi = min(total, page*perPage)
	var rels []string
	if page < last {
		rels = append(rels, "<"+pageURL(rq, q, page+1)+">; rel=\"next\"", "<"+pageURL(rq, q, last)+">; rel=\"last\"")
	}
	if page > 1 {
		rels = append(rels, "<"+pageURL(rq, q, page-1)+">; rel=\"prev\"", "<"+pageURL(rq, q, 1)+">; rel=\"first\"")
	}
	if len(rels) > 0 {
		extra = "Link: " + strings.Join(rels, ", ") + "\r\n"
	}
	return lo, hi, extra
}

func resolveRepo(owner, name string) *Repo {
	if strings.Contains(owner, "%") {
		owner = jsDecode(owner)
	}
	if strings.Contains(name, "%") {
		name = jsDecode(name)
	}
	if strings.ToLower(owner) != viewerLower {
		return nil
	}
	return getRepo(owner, name)
}

func parseNumber(text string) int64 {
	if text == "" {
		return -1
	}
	for i := 0; i < len(text); i++ {
		if !isDigit(text[i]) {
			return -1
		}
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func decodeJoin(parts []string) string {
	decoded := make([]string, len(parts))
	for i, part := range parts {
		decoded[i] = jsDecode(part)
	}
	return strings.Join(decoded, "/")
}

func noContent(requestID string) []byte {
	return HTTPFrame(204, "", "X-GitHub-Request-Id: "+requestID+"\r\n", nil)
}

func restFail(r any) []byte {
	switch e := r.(type) {
	case *ApiError:
		return errorFrame(e.Status, e.Message, e.Errors, "")
	case *HttpError:
		return errorFrame(e.Status, e.Message, nil, "")
	case error:
		return errorFrame(500, "Server Error: "+e.Error(), nil, "")
	}
	return errorFrame(500, "Server Error: "+jsString(r), nil, "")
}

// ---------------------------------------------------------------- the router

// handleREST answers one authenticated request under /api/v3.
func handleREST(rq *restReq) (frame []byte) {
	defer func() {
		if r := recover(); r != nil {
			frame = restFail(r)
		}
	}()
	rest := ""
	if len(rq.path) > 8 {
		rest = rq.path[8:]
	}
	var parts []string
	if rest != "" {
		parts = strings.Split(rest, "/")
	}
	if len(parts) > 0 && parts[len(parts)-1] == "" && !(parts[0] == "repos" && len(parts) > 3 && parts[3] == "contents") {
		parts = parts[:len(parts)-1]
	}
	// Reads share the store; anything that writes (including computing a pull's mergeability, which stores
	// merged trees) holds it exclusively.
	readOnly := (rq.method == "GET" || rq.method == "HEAD") && !(len(parts) == 5 && parts[0] == "repos" && parts[3] == "pulls")
	if readOnly {
		storeMu.RLock()
		defer storeMu.RUnlock()
	} else {
		storeMu.Lock()
		defer storeMu.Unlock()
	}
	// The hot read: GET /repos/{owner}/{repo}/issues/{number}.
	if rq.method == "GET" && len(parts) == 6 && parts[0] == "repos" && parts[4] == "issues" {
		repo := resolveRepo(parts[1], parts[2])
		if repo == nil {
			return notFoundFrame()
		}
		if issue := repo.issues[parseNumber(parts[5])]; issue != nil {
			return issueReply(issue)
		}
	}
	return restDispatch(rq, parts, parseQuery(rq.search))
}

func restDispatch(rq *restReq, parts []string, q *query) []byte {
	method := rq.method
	if len(parts) == 0 {
		if method != "GET" {
			return notFoundFrame()
		}
		b := base()
		return jsonValueFrame(200, O("current_user_url", b+"/api/v3/user", "repository_url", b+"/api/v3/repos/{owner}/{repo}", "rate_limit_url", b+"/api/v3/rate_limit"),
			"X-OAuth-Scopes: repo, read:org\r\nX-Accepted-OAuth-Scopes: \r\n")
	}
	first := parts[0]
	if len(parts) == 1 && method == "GET" {
		switch first {
		case "meta":
			return jsonValueFrame(200, O("verifiable_password_authentication", false, "installed_version", "3.14.0", "packages", []any{}, "hooks", []any{},
				"web", []any{}, "api", []any{}, "git", []any{}, "pages", []any{}, "importer", []any{}), "")
		case "user":
			u := userJSON(viewerLogin)
			u.Set("name", viewerLogin).Set("email", nil).Set("public_repos", len(repoList)).Set("followers", 0).Set("following", 0)
			u.Set("created_at", "2025-01-01T00:00:01Z").Set("updated_at", "2025-01-01T00:00:01Z")
			return jsonValueFrame(200, u, "")
		case "rate_limit":
			bucket := func() *Obj { return O("limit", 5000, "used", 0, "remaining", 5000, "reset", 1_735_693_200) }
			search := bucket().Set("limit", 30).Set("remaining", 30)
			return jsonValueFrame(200, O("resources", O("core", bucket(), "graphql", bucket(), "search", search), "rate", bucket()), "")
		}
	}
	if first == "user" && len(parts) == 2 && parts[1] == "repos" {
		if method == "GET" {
			repos := listRepos()
			sortKey, _ := q.get("sort")
			direction, hasDirection := q.get("direction")
			if sortKey == "created" || sortKey == "updated" || sortKey == "pushed" {
				sortReposBy(repos, sortKey == "pushed")
				if !hasDirection || direction == "desc" {
					reverseRepos(repos)
				}
			} else if hasDirection && direction == "desc" {
				reverseRepos(repos)
			}
			lo, hi, link := paginate(rq, q, len(repos))
			items := make([]any, 0, hi-lo)
			for _, repo := range repos[lo:hi] {
				items = append(items, repoJSON(repo))
			}
			return jsonValueFrame(200, items, link)
		}
		if method == "POST" {
			body := jsonBody(rq)
			repo := createRepo(body["name"], body["description"], body["private"], body["auto_init"], body["default_branch"])
			return jsonValueFrame(201, repoJSON(repo), "Location: "+base()+"/api/v3/repos/"+repo.Owner+"/"+repo.Name+"\r\n")
		}
		return notFoundFrame()
	}
	if first == "users" && len(parts) == 2 && method == "GET" {
		if strings.ToLower(parts[1]) != viewerLower {
			return notFoundFrame()
		}
		return jsonValueFrame(200, userJSON(viewerLogin), "")
	}
	if first == "repos" && len(parts) >= 3 {
		repo := resolveRepo(parts[1], parts[2])
		if repo == nil {
			return notFoundFrame()
		}
		return repoRoutes(rq, repo, parts[3:], q)
	}
	return notFoundFrame()
}

func sortReposBy(repos []*Repo, pushed bool) {
	less := func(a, b *Repo) bool {
		if pushed {
			return a.PushedAt < b.PushedAt
		}
		return a.ID < b.ID
	}
	// insertion sort keeps it stable, as Array.prototype.sort is
	for i := 1; i < len(repos); i++ {
		for j := i; j > 0 && less(repos[j], repos[j-1]); j-- {
			repos[j], repos[j-1] = repos[j-1], repos[j]
		}
	}
}

func reverseRepos(repos []*Repo) {
	for i, j := 0, len(repos)-1; i < j; i, j = i+1, j-1 {
		repos[i], repos[j] = repos[j], repos[i]
	}
}

func repoRoutes(rq *restReq, repo *Repo, parts []string, q *query) []byte {
	method := rq.method
	if len(parts) == 0 {
		switch method {
		case "GET":
			return jsonValueFrame(200, repoJSON(repo), "")
		case "PATCH":
			updateRepo(repo, jsonBody(rq))
			return jsonValueFrame(200, repoJSON(repo), "")
		case "DELETE":
			deleteRepo(repo)
			return noContent("FEED:0000:00000000:00000000:00000000")
		}
		return notFoundFrame()
	}
	switch parts[0] {
	case "branches":
		return branchRoutes(rq, repo, parts[1:], q)
	case "commits":
		return commitRoutes(rq, repo, parts[1:], q)
	case "tags":
		if method != "GET" {
			return notFoundFrame()
		}
		tags := repo.RefsWithPrefix("refs/tags/")
		lo, hi, link := paginate(rq, q, len(tags))
		items := make([]any, 0, hi-lo)
		for _, ref := range tags[lo:hi] {
			sha := repo.ResolveCommit(ref.Name)
			if sha == "" {
				sha = ref.OID
			}
			items = append(items, O("name", ref.Name[10:], "commit", O("sha", sha, "url", base()+"/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/commits/"+sha),
				"zipball_url", "", "tarball_url", ""))
		}
		return jsonValueFrame(200, items, link)
	case "git":
		return gitDataRoutes(rq, repo, parts[1:], q)
	case "contents":
		return contentsRoutes(rq, repo, parts[1:], q)
	case "labels":
		return labelRoutes(rq, repo, parts[1:], q)
	case "issues":
		return issueRoutes(rq, repo, parts[1:], q)
	case "pulls":
		return pullRoutes(rq, repo, parts[1:], q)
	}
	return notFoundFrame()
}

// ---------------------------------------------------------------- branches and commits

func branchRoutes(rq *restReq, repo *Repo, parts []string, q *query) []byte {
	if rq.method != "GET" {
		return notFoundFrame()
	}
	if len(parts) == 0 {
		branches := repo.RefsWithPrefix("refs/heads/")
		lo, hi, link := paginate(rq, q, len(branches))
		items := make([]any, 0, hi-lo)
		for _, ref := range branches[lo:hi] {
			items = append(items, branchJSON(repo, ref.Name[11:], ref.OID))
		}
		return jsonValueFrame(200, items, link)
	}
	name := decodeJoin(parts)
	oid, ok := repo.GetRef("refs/heads/" + name)
	if !ok {
		return notFoundFrame()
	}
	return jsonValueFrame(200, branchJSON(repo, name, oid), "")
}

func commitRoutes(rq *restReq, repo *Repo, parts []string, q *query) []byte {
	if rq.method != "GET" {
		return notFoundFrame()
	}
	if len(parts) == 0 {
		if repo.RefCount() == 0 {
			return errorFrame(409, "Git Repository is empty.", nil, "")
		}
		sha, hasSha := q.get("sha")
		name := "HEAD"
		if hasSha && sha != "" {
			name = sha
		}
		tip := repo.ResolveCommit(name)
		if tip == "" {
			return errorFrame(404, "No commit found for SHA: "+sha, nil, "")
		}
		perPage := positive(q, "per_page", 30, 100)
		page := positive(q, "page", 1, 1_000_000)
		all := repo.ListCommits(tip, perPage*page+1, nil)
		lo, hi := min(len(all), (page-1)*perPage), min(len(all), page*perPage)
		items := make([]any, 0, hi-lo)
		for _, oid := range all[lo:hi] {
			items = append(items, commitJSON(repo, oid, false))
		}
		link := ""
		if len(all) > page*perPage {
			link = "Link: <" + pageURL(rq, q, page+1) + ">; rel=\"next\"\r\n"
		}
		return jsonValueFrame(200, items, link)
	}
	oid := repo.ResolveCommit(decodeJoin(parts))
	if oid == "" {
		return notFoundFrame()
	}
	return jsonValueFrame(200, commitJSON(repo, oid, true), "")
}

// ---------------------------------------------------------------- git data

func refList(repo *Repo, refs []Ref) []any {
	out := make([]any, len(refs))
	for i, ref := range refs {
		out[i] = refJSON(repo, ref.Name, ref.OID)
	}
	return out
}

func recursiveFlag(q *query) bool {
	v, ok := q.get("recursive")
	return ok && v != "" && v != "0" && v != "false"
}

func gitDataRoutes(rq *restReq, repo *Repo, parts []string, q *query) []byte {
	method := rq.method
	kind := ""
	if len(parts) > 0 {
		kind = parts[0]
	}
	repoAPI := base() + "/api/v3/repos/" + repo.Owner + "/" + repo.Name
	if kind == "refs" || kind == "ref" {
		if len(parts) == 1 && kind == "refs" {
			switch method {
			case "GET":
				return jsonValueFrame(200, refList(repo, repo.RefsWithPrefix("refs/")), "")
			case "POST":
				body := jsonBody(rq)
				createRef(repo, body["ref"], body["sha"])
				ref, _ := body["ref"].(string)
				sha, _ := body["sha"].(string)
				return jsonValueFrame(201, refJSON(repo, ref, sha), "")
			}
			return notFoundFrame()
		}
		name := "refs/" + decodeJoin(parts[1:])
		switch method {
		case "GET":
			if oid, ok := repo.GetRef(name); ok {
				return jsonValueFrame(200, refJSON(repo, name, oid), "")
			}
			if matching := repo.RefsWithPrefix(name + "/"); len(matching) > 0 && kind == "refs" {
				return jsonValueFrame(200, refList(repo, matching), "")
			}
			return notFoundFrame()
		case "PATCH":
			body := jsonBody(rq)
			updateRef(repo, name, body["sha"], body["force"] == true)
			sha, _ := body["sha"].(string)
			return jsonValueFrame(200, refJSON(repo, name, sha), "")
		case "DELETE":
			removeRef(repo, name)
			return noContent("FEED:0000:00000000:00000000:00000001")
		}
		return notFoundFrame()
	}
	if kind == "matching-refs" && method == "GET" {
		return jsonValueFrame(200, refList(repo, repo.RefsWithPrefix("refs/"+decodeJoin(parts[1:]))), "")
	}
	if method != "GET" || len(parts) != 2 {
		return notFoundFrame()
	}
	sha := parts[1]
	obj := repo.GetObject(sha)
	if obj == nil {
		return notFoundFrame()
	}
	switch {
	case kind == "commits" && obj.Type == ObjCommit:
		info := repo.Commit(sha)
		parents := make([]any, len(info.Parents))
		for i, parent := range info.Parents {
			parents[i] = O("sha", parent, "url", repoAPI+"/git/commits/"+parent)
		}
		return jsonValueFrame(200, O("sha", sha, "url", repoAPI+"/git/commits/"+sha, "author", identJSON(info.Author),
			"committer", identJSON(info.Committer), "tree", O("sha", info.Tree, "url", repoAPI+"/git/trees/"+info.Tree),
			"message", trimMessage(info.Message), "parents", parents), "")
	case kind == "trees" && obj.Type == ObjTree:
		entries := []any{}
		recursive := recursiveFlag(q)
		var walk func(treeSha, prefix string)
		walk = func(treeSha, prefix string) {
			tree := repo.GetObject(treeSha)
			if tree == nil {
				return
			}
			for _, entry := range tree.Tree() {
				typ := "blob"
				switch entry.Mode {
				case "40000":
					typ = "tree"
				case "160000":
					typ = "commit"
				}
				mode := entry.Mode
				for len(mode) < 6 {
					mode = "0" + mode
				}
				item := O("path", prefix+entry.Name, "mode", mode, "type", typ, "sha", entry.OID, "url", repoAPI+"/git/"+typ+"s/"+entry.OID)
				if typ == "blob" {
					size := 0
					if blob := repo.GetObject(entry.OID); blob != nil {
						size = len(blob.Data)
					}
					item.Set("size", size)
				}
				entries = append(entries, item)
				if typ == "tree" && recursive {
					walk(entry.OID, prefix+entry.Name+"/")
				}
			}
		}
		walk(sha, "")
		return jsonValueFrame(200, O("sha", sha, "url", repoAPI+"/git/trees/"+sha, "tree", entries, "truncated", false), "")
	case kind == "blobs" && obj.Type == ObjBlob:
		return jsonValueFrame(200, O("sha", sha, "node_id", globalID("Blob", shortHexID(sha)), "size", len(obj.Data),
			"url", repoAPI+"/git/blobs/"+sha, "content", base64.StdEncoding.EncodeToString(obj.Data), "encoding", "base64"), "")
	case kind == "tags" && obj.Type == ObjTag:
		tag := ParseTag(obj.Data)
		return jsonValueFrame(200, O("sha", sha, "tag", tag.Tag, "message", tag.Message, "object", O("sha", tag.Object, "type", tag.Type)), "")
	}
	return notFoundFrame()
}

// ---------------------------------------------------------------- contents

func contentsRoutes(rq *restReq, repo *Repo, parts []string, q *query) []byte {
	filePath := decodeJoin(parts)
	switch rq.method {
	case "GET":
		refArg, hasRef := q.get("ref")
		file, entries, dir := getContents(repo, filePath, refArg, hasRef)
		ref := repo.DefaultBranch
		if hasRef {
			ref = refArg
		}
		if dir {
			items := make([]any, len(entries))
			for i, entry := range entries {
				items[i] = fileEntryJSON(repo, entry.Path, entry.OID, entry.Size, entry.Type, ref)
			}
			return jsonValueFrame(200, items, "")
		}
		out := fileEntryJSON(repo, file.Path, file.OID, len(file.Data), "file", ref)
		out.Set("content", base64.StdEncoding.EncodeToString(file.Data)).Set("encoding", "base64")
		return jsonValueFrame(200, out, "")
	case "PUT":
		body := jsonBody(rq)
		content, ok := body["content"].(string)
		if !ok {
			apiFail(422, "Invalid request.\n\n\"content\" wasn't supplied.")
		}
		committer, hasCommitter := body["committer"]
		done := putContents(repo, filePath, contentsInput{message: body["message"], content: nodeBase64Decode(content), sha: body["sha"],
			branch: body["branch"], author: body["author"], committer: committer, hasCommitter: hasCommitter})
		size := 0
		if blob := repo.GetObject(done.Blob); blob != nil {
			size = len(blob.Data)
		}
		status := 200
		if done.Created {
			status = 201
		}
		entry := fileEntryJSON(repo, done.Path, done.Blob, size, "file", done.Branch).Set("encoding", "base64")
		return jsonValueFrame(status, O("content", entry, "commit", contentCommitJSON(repo, done.Commit)), "")
	case "DELETE":
		body := jsonBody(rq)
		done := deleteContents(repo, filePath, body["message"], body["sha"], body["branch"])
		return jsonValueFrame(200, O("content", nil, "commit", contentCommitJSON(repo, done.Commit)), "")
	}
	return notFoundFrame()
}

// ---------------------------------------------------------------- labels

func labelRoutes(rq *restReq, repo *Repo, parts []string, q *query) []byte {
	method := rq.method
	if len(parts) == 0 {
		switch method {
		case "GET":
			lo, hi, link := paginate(rq, q, len(repo.labelList))
			return jsonValueFrame(200, labelList(repo, repo.labelList[lo:hi]), link)
		case "POST":
			body := jsonBody(rq)
			return jsonValueFrame(201, labelJSON(repo, createLabel(repo, body["name"], body["color"], body["description"])), "")
		}
		return notFoundFrame()
	}
	label := repo.labelByName(decodeJoin(parts))
	if label == nil {
		return notFoundFrame()
	}
	switch method {
	case "GET":
		return jsonValueFrame(200, labelJSON(repo, label), "")
	case "DELETE":
		repo.removeLabelEntry(label)
		for _, issue := range repo.issueList {
			for i, candidate := range issue.Labels {
				if candidate == label {
					issue.Labels = append(issue.Labels[:i:i], issue.Labels[i+1:]...)
					issue.invalidate()
					break
				}
			}
		}
		return noContent("FEED:0000:00000000:00000000:00000002")
	case "PATCH":
		body := jsonBody(rq)
		if newName, ok := body["new_name"].(string); ok && newName != "" && strings.ToLower(newName) != strings.ToLower(label.Name) {
			if repo.labelByName(newName) != nil {
				apiFail(422, "Validation Failed", O("resource", "Label", "code", "already_exists", "field", "name"))
			}
			repo.removeLabelEntry(label)
			label.Name = newName
			repo.labels[strings.ToLower(newName)] = label
			repo.labelList = append(repo.labelList, label)
		}
		if color, ok := body["color"].(string); ok {
			label.Color = strings.TrimPrefix(color, "#")
		}
		if description, ok := body["description"].(string); ok {
			label.Description = &description
		}
		for _, issue := range repo.issueList {
			for _, candidate := range issue.Labels {
				if candidate == label {
					issue.invalidate()
					break
				}
			}
		}
		return jsonValueFrame(200, labelJSON(repo, label), "")
	}
	return notFoundFrame()
}

// ---------------------------------------------------------------- issues

func issueRoutes(rq *restReq, repo *Repo, parts []string, q *query) []byte {
	method := rq.method
	if len(parts) == 0 {
		switch method {
		case "GET":
			state, _ := q.get("state")
			labels, _ := q.get("labels")
			sortKey, _ := q.get("sort")
			direction, _ := q.get("direction")
			issues := listIssues(repo, state, labels, sortKey, direction)
			lo, hi, link := paginate(rq, q, len(issues))
			body := []byte{'['}
			for i, issue := range issues[lo:hi] {
				if i > 0 {
					body = append(body, ',')
				}
				body = append(body, issueFrameOf(issue).json...)
			}
			return jsonFrame(200, append(body, ']'), link)
		case "POST":
			body := jsonBody(rq)
			issue := createIssue(repo, body["title"], body["body"], body["labels"], viewerLogin)
			location := "Location: " + base() + "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.FormatInt(issue.Number, 10) + "\r\n"
			return jsonFrame(201, issueFrameOf(issue).json, location)
		}
		return notFoundFrame()
	}
	n := parseNumber(parts[0])
	if n < 0 {
		return notFoundFrame()
	}
	issue, pull := findThread(repo, n)
	if issue == nil && pull == nil {
		return notFoundFrame()
	}
	if len(parts) == 1 {
		switch method {
		case "GET":
			if issue != nil {
				return issueReply(issue)
			}
			out := pullAsIssue(pull).Set("pull_request", O("url", base()+"/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/pulls/"+strconv.FormatInt(pull.Number, 10)))
			return jsonValueFrame(200, out, "")
		case "PATCH":
			body := jsonBody(rq)
			if issue != nil {
				updateIssue(issue, body)
				return issueReply(issue)
			}
			updatePull(pull, body)
			return jsonValueFrame(200, pullAsIssue(pull), "")
		}
		return notFoundFrame()
	}
	if parts[1] == "comments" && len(parts) == 2 {
		comments := []*Comment(nil)
		if issue != nil {
			comments = issue.Comments
		} else {
			comments = pull.Comments
		}
		switch method {
		case "GET":
			lo, hi, link := paginate(rq, q, len(comments))
			items := make([]any, 0, hi-lo)
			for _, comment := range comments[lo:hi] {
				items = append(items, commentJSON(comment))
			}
			return jsonValueFrame(200, items, link)
		case "POST":
			body := jsonBody(rq)
			return jsonValueFrame(201, commentJSON(addComment(issue, pull, body["body"], viewerLogin)), "")
		}
	}
	return notFoundFrame()
}

// ---------------------------------------------------------------- pull requests

func pullRoutes(rq *restReq, repo *Repo, parts []string, q *query) []byte {
	method := rq.method
	if len(parts) == 0 {
		switch method {
		case "GET":
			stateArg, _ := q.get("state")
			state := "open"
			if stateArg == "closed" || stateArg == "all" {
				state = stateArg
			}
			headFilter, hasHead := q.get("head")
			if i := strings.IndexByte(headFilter, ':'); hasHead && i >= 0 {
				headFilter = headFilter[i+1:]
			}
			baseFilter, hasBase := q.get("base")
			var pulls []*Pull
			for _, pull := range repo.pullList {
				if (state == "all" || pull.State == state) && (!hasHead || pull.HeadRef == headFilter) && (!hasBase || pull.BaseRef == baseFilter) {
					pulls = append(pulls, pull)
				}
			}
			sortKey, _ := q.get("sort")
			bySeq := sortKey == "updated"
			for i := 1; i < len(pulls); i++ {
				for j := i; j > 0; j-- {
					a, b := pulls[j], pulls[j-1]
					less := a.Number < b.Number
					if bySeq {
						less = a.Seq < b.Seq
					}
					if !less {
						break
					}
					pulls[j], pulls[j-1] = b, a
				}
			}
			if direction, ok := q.get("direction"); !ok || direction == "desc" {
				for i, j := 0, len(pulls)-1; i < j; i, j = i+1, j-1 {
					pulls[i], pulls[j] = pulls[j], pulls[i]
				}
			}
			lo, hi, link := paginate(rq, q, len(pulls))
			items := make([]any, 0, hi-lo)
			for _, pull := range pulls[lo:hi] {
				items = append(items, pullJSON(pull, false))
			}
			return jsonValueFrame(200, items, link)
		case "POST":
			body := jsonBody(rq)
			pull := createPull(repo, body["title"], body["body"], body["head"], body["base"], body["draft"], viewerLogin)
			location := "Location: " + base() + "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + strconv.FormatInt(pull.Number, 10) + "\r\n"
			return jsonValueFrame(201, pullJSON(pull, true), location)
		}
		return notFoundFrame()
	}
	n := parseNumber(parts[0])
	pull := repo.pulls[n]
	if n < 0 || pull == nil {
		return notFoundFrame()
	}
	if len(parts) == 1 {
		switch method {
		case "GET":
			return jsonValueFrame(200, pullJSON(pull, true), "")
		case "PATCH":
			updatePull(pull, jsonBody(rq))
			return jsonValueFrame(200, pullJSON(pull, true), "")
		}
		return notFoundFrame()
	}
	switch sub := parts[1]; {
	case sub == "files" && method == "GET":
		head := pullHeadSha(pull)
		files := pullFiles(pull)
		lo, hi, link := paginate(rq, q, len(files))
		items := make([]any, 0, hi-lo)
		for i := lo; i < hi; i++ {
			items = append(items, fileJSON(repo, &files[i], head))
		}
		return jsonValueFrame(200, items, link)
	case sub == "commits" && method == "GET":
		commits := pullCommits(pull)
		for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
			commits[i], commits[j] = commits[j], commits[i]
		}
		lo, hi, link := paginate(rq, q, len(commits))
		items := make([]any, 0, hi-lo)
		for _, oid := range commits[lo:hi] {
			items = append(items, commitJSON(repo, oid, false))
		}
		return jsonValueFrame(200, items, link)
	case sub == "merge":
		if method == "GET" {
			if pull.Merged {
				return noContent("FEED:0000:00000000:00000000:00000003")
			}
			return notFoundFrame()
		}
		if method == "PUT" {
			body := jsonBody(rq)
			sha, message := mergePull(pull, body["merge_method"], body["commit_title"], body["commit_message"], body["sha"])
			return jsonValueFrame(200, O("sha", sha, "merged", true, "message", message), "")
		}
	case sub == "comments" || sub == "reviews":
		if method == "GET" {
			return jsonValueFrame(200, []any{}, "")
		}
	}
	return notFoundFrame()
}
