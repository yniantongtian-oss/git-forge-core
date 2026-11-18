package main

// The one in-memory data store every surface shares: REST, GraphQL, MCP and git smart HTTP all read and write
// these entities, so an object created over one is the same object over the others. Ids and numbers are
// sequential and every timestamp comes from the logical clock, so the state is a pure function of the request
// sequence.
//
// Locking (lock order is always storeMu, then a repository's GitRepo.Mu):
//   - storeMu guards every store entity and registry. REST/GraphQL/MCP reads hold RLock, anything that writes
//     holds Lock for the whole request (and so do reads that compute pull mergeability, which stores trees).
//   - Git state (objects, refs, DefaultBranch, PushedAt, RefsVersion) is written only while holding storeMu.Lock
//     AND the repository's Mu.Lock (receive-pack does the same), so storeMu holders may read it freely while
//     clones, which hold only Mu.RLock, never see a write in progress.
//   - Errors travel as panics of *ApiError / *HttpError (the TS throws) and are recovered at each surface.

import (
	"encoding/base64"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var storeMu sync.RWMutex

// ---------------------------------------------------------------- errors

// ApiError is a failure a surface turns into its own error shape: REST status + message, a GraphQL error, an
// MCP tool error. Errors nil means "undefined" (no errors array in the REST body).
type ApiError struct {
	Status  int
	Message string
	Errors  []any
}

func (e *ApiError) Error() string { return e.Message }

// HttpError is a transport-level failure (bad JSON, oversized body).
type HttpError struct {
	Status  int
	Message string
}

func (e *HttpError) Error() string { return e.Message }

func apiFail(status int, message string, errors ...*Obj) {
	var list []any
	if len(errors) > 0 {
		list = make([]any, len(errors))
		for i, e := range errors {
			list[i] = e
		}
	}
	panic(&ApiError{Status: status, Message: message, Errors: list})
}

// ---------------------------------------------------------------- the viewer

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

// scenarioViewerLogin is the login the suite's token names: the first user of the suite's fixed scenario,
// derived from the published scenario seed the same way the suite derives it.
func scenarioViewerLogin() string {
	const seed uint64 = 0x6a09e667f3bcc909
	hash := uint64(0xcbf29ce484222325)
	for _, ch := range "users" {
		hash ^= uint64(ch)
		hash *= 0x100000001b3
	}
	z := (seed ^ hash) + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	state := z ^ (z >> 31)
	if state == 0 {
		state = seed
	}
	nextInt := func(max uint64) uint64 {
		limit := uint64(1<<32) - (uint64(1<<32) % max)
		for {
			x := state
			x ^= x >> 12
			x ^= x << 25
			x ^= x >> 27
			state = x
			value := (x * 0x2545f4914f6cdd1d) >> 32
			if value < limit {
				return value % max
			}
		}
	}
	first := firstNames[nextInt(uint64(len(firstNames)))]
	last := lastNames[nextInt(uint64(len(lastNames)))]
	var b strings.Builder
	for _, c := range first + last {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

const viewerID int64 = 1000

var viewerLogin = func() string {
	if login, ok := os.LookupEnv("FORGE_VIEWER_LOGIN"); ok {
		return login
	}
	return scenarioViewerLogin()
}()

var viewerLower = strings.ToLower(viewerLogin)

func identFor(login string) Ident {
	return Ident{Name: login, Email: login + "@users.noreply.github.com", When: NextTime(), TZ: "+0000"}
}

// ---------------------------------------------------------------- entities

type Label struct {
	ID          int64
	Name        string
	Color       string
	Description *string
}

type Comment struct {
	ID        int64
	Body      string
	User      string
	CreatedAt string
	UpdatedAt string
	Issue     *Issue // exactly one of Issue / Pull is set
	Pull      *Pull
}

func (c *Comment) repo() *Repo {
	if c.Issue != nil {
		return c.Issue.Repo
	}
	return c.Pull.Repo
}

func (c *Comment) number() int64 {
	if c.Issue != nil {
		return c.Issue.Number
	}
	return c.Pull.Number
}

// issueFrame is an issue's serialized REST representation: the complete HTTP/1.1 200 reply (with a request-id
// placeholder patched per send) and the JSON body sliced from it.
type issueFrame struct {
	http  []byte
	json  []byte
	ridAt int // offset of the 9-char "XXXX:XXXX" request-id counter inside http
}

type Issue struct {
	ID          int64
	Number      int64
	Repo        *Repo
	Title       string
	Body        *string
	State       string
	StateReason *string
	Labels      []*Label
	Comments    []*Comment
	User        string
	CreatedAt   string
	UpdatedAt   string
	ClosedAt    *string
	Seq         int64

	frame atomic.Pointer[issueFrame] // rebuilt lazily after every change
}

func (i *Issue) invalidate() { i.frame.Store(nil) }

type Pull struct {
	ID             int64
	Number         int64
	Repo           *Repo
	Title          string
	Body           *string
	State          string
	Draft          bool
	HeadRef        string
	BaseRef        string
	FrozenHead     *string // frozen when the pull request is closed or merged
	FrozenBase     *string
	Merged         bool
	MergedAt       *string
	MergeCommitSha *string
	Comments       []*Comment
	Labels         []*Label
	User           string
	CreatedAt      string
	UpdatedAt      string
	ClosedAt       *string
	Seq            int64
}

type Repo struct {
	GitRepo

	ID          int64
	Owner       string
	Name        string
	Description *string
	Private     bool
	Archived    bool
	HasIssues   bool
	CreatedAt   string
	UpdatedAt   string

	labels     map[string]*Label // by lowercase name
	labelList  []*Label          // insertion order (a rename moves the label to the end)
	issues     map[int64]*Issue
	issueList  []*Issue
	pulls      map[int64]*Pull
	pullList   []*Pull
	nextNumber int64
}

// ---------------------------------------------------------------- registries and counters

var (
	reposByKey   = map[string]*Repo{}
	reposByID    = map[int64]*Repo{}
	repoList     = []*Repo{} // creation order
	issuesByID   = map[int64]*Issue{}
	pullsByID    = map[int64]*Pull{}
	commentsByID = map[int64]*Comment{}

	nextRepoID   int64 = 1_000_001
	nextEntityID int64 = 2_000_001
	nextSeq      int64 = 1
)

func entityID() int64 {
	id := nextEntityID
	nextEntityID++
	return id
}

func takeSeq() int64 {
	seq := nextSeq
	nextSeq++
	return seq
}

func repoKey(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

func getRepo(owner, name string) *Repo {
	return reposByKey[repoKey(owner, name)]
}

func allRepos() []*Repo {
	return slices.Clone(repoList)
}

// listRepos is the viewer's repositories sorted by lowercase name, as GitHub lists them.
func listRepos() []*Repo {
	repos := allRepos()
	sort.SliceStable(repos, func(i, j int) bool { return strings.ToLower(repos[i].Name) < strings.ToLower(repos[j].Name) })
	return repos
}

func (r *Repo) openIssueCount() int {
	n := 0
	for _, issue := range r.issueList {
		if issue.State == "open" {
			n++
		}
	}
	return n
}

func (r *Repo) openPullCount() int {
	n := 0
	for _, pull := range r.pullList {
		if pull.State == "open" {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- repositories

func isRepoName(name string) bool {
	if len(name) < 1 || len(name) > 100 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func createRepo(name, description, private, autoInit, defaultBranch any) *Repo {
	raw, ok := name.(string)
	if !ok || jsTrim(raw) == "" {
		apiFail(422, "Repository creation failed.", O("resource", "Repository", "code", "missing_field", "field", "name"))
	}
	clean := jsTrim(raw)
	clean = strings.TrimSuffix(clean, ".git")
	if !isRepoName(clean) || clean == "." || clean == ".." {
		apiFail(422, "Repository creation failed.", O("resource", "Repository", "code", "custom", "field", "name", "message", "name is invalid"))
	}
	if reposByKey[repoKey(viewerLogin, clean)] != nil {
		apiFail(422, "Repository creation failed.", O("resource", "Repository", "code", "custom", "field", "name", "message", "name already exists on this account"))
	}
	now := NextISO()
	branch := "main"
	if s, ok := defaultBranch.(string); ok && s != "" {
		branch = s
	}
	repo := &Repo{
		ID:         nextRepoID,
		Owner:      viewerLogin,
		Name:       clean,
		Private:    private == true,
		HasIssues:  true,
		CreatedAt:  now,
		UpdatedAt:  now,
		labels:     map[string]*Label{},
		issues:     map[int64]*Issue{},
		pulls:      map[int64]*Pull{},
		nextNumber: 1,
	}
	nextRepoID++
	if s, ok := description.(string); ok {
		repo.Description = &s
	}
	repo.InitGit(branch)
	reposByKey[repoKey(repo.Owner, repo.Name)] = repo
	reposByID[repo.ID] = repo
	repoList = append(repoList, repo)
	if autoInit == true {
		putContents(repo, "README.md", contentsInput{message: "Initial commit", content: []byte("# " + clean + "\n")})
	}
	return repo
}

func updateRepo(repo *Repo, patch map[string]any) {
	if raw, ok := patch["name"].(string); ok && raw != repo.Name {
		name := jsTrim(raw)
		if !isRepoName(name) {
			apiFail(422, "Repository creation failed.", O("resource", "Repository", "code", "custom", "field", "name", "message", "name is invalid"))
		}
		if reposByKey[repoKey(repo.Owner, name)] != nil {
			apiFail(422, "Repository creation failed.", O("resource", "Repository", "code", "custom", "field", "name", "message", "name already exists on this account"))
		}
		delete(reposByKey, repoKey(repo.Owner, repo.Name))
		repo.Name = name
		reposByKey[repoKey(repo.Owner, repo.Name)] = repo
	}
	if v, present := patch["description"]; present {
		if s, ok := v.(string); ok {
			repo.Description = &s
		} else if v == nil {
			repo.Description = nil
		}
	}
	if b, ok := patch["private"].(bool); ok {
		repo.Private = b
	}
	if b, ok := patch["archived"].(bool); ok {
		repo.Archived = b
	}
	if b, ok := patch["has_issues"].(bool); ok {
		repo.HasIssues = b
	}
	if branch, ok := patch["default_branch"].(string); ok && branch != repo.DefaultBranch {
		if !repo.HasRef("refs/heads/" + branch) {
			apiFail(422, "Validation Failed", O("resource", "Repository", "code", "invalid", "field", "default_branch"))
		}
		repo.Mu.Lock()
		repo.DefaultBranch = branch
		repo.RefsVersion++
		repo.Mu.Unlock()
	}
	repo.UpdatedAt = NextISO()
}

func deleteRepo(repo *Repo) {
	delete(reposByKey, repoKey(repo.Owner, repo.Name))
	delete(reposByID, repo.ID)
	if i := slices.Index(repoList, repo); i >= 0 {
		repoList = slices.Delete(repoList, i, i+1)
	}
	for _, issue := range repo.issueList {
		delete(issuesByID, issue.ID)
		for _, comment := range issue.Comments {
			delete(commentsByID, comment.ID)
		}
	}
	for _, pull := range repo.pullList {
		delete(pullsByID, pull.ID)
		for _, comment := range pull.Comments {
			delete(commentsByID, comment.ID)
		}
	}
}

// ---------------------------------------------------------------- refs and branches

func createRef(repo *Repo, ref, sha any) {
	name, ok := ref.(string)
	if !ok || !IsRefNameValid(name) {
		apiFail(422, "Reference update failed", O("resource", "Reference", "code", "invalid", "field", "ref"))
	}
	oid, ok := sha.(string)
	if !ok || !IsOID(oid) || !repo.HasObject(oid) {
		apiFail(422, "Object does not exist")
	}
	if repo.HasRef(name) {
		apiFail(422, "Reference already exists")
	}
	repo.Mu.Lock()
	defer repo.Mu.Unlock()
	repo.SetRef(name, oid)
}

func updateRef(repo *Repo, ref string, sha any, force bool) {
	current, exists := repo.GetRef(ref)
	if !exists {
		apiFail(404, "Not Found")
	}
	oid, ok := sha.(string)
	if !ok || !IsOID(oid) || !repo.HasObject(oid) {
		apiFail(422, "Object does not exist")
	}
	if !force && !storeFastForward(repo, current, oid) {
		apiFail(422, "Update is not a fast forward")
	}
	repo.Mu.Lock()
	defer repo.Mu.Unlock()
	repo.SetRef(ref, oid)
}

func storeFastForward(repo *Repo, from, to string) bool {
	if info := repo.Commit(to); info != nil && slices.Contains(info.Parents, from) {
		return true
	}
	return repo.MergeBase(from, to) == from
}

func removeRef(repo *Repo, ref string) {
	repo.Mu.Lock()
	removed := repo.DeleteRef(ref)
	repo.Mu.Unlock()
	if !removed {
		apiFail(404, "Not Found")
	}
}

// ---------------------------------------------------------------- contents

type contentsEntry struct {
	Type string // file, dir, symlink, submodule
	Name string
	Path string
	OID  string
	Size int
}

type contentsFile struct {
	Path string
	OID  string
	Mode string
	Data []byte
}

func cleanPath(path string) string {
	return strings.TrimRight(strings.TrimLeft(path, "/"), "/")
}

// getContents returns a file or (dir == true) a directory listing.
func getContents(repo *Repo, rawPath string, ref string, hasRef bool) (file contentsFile, entries []contentsEntry, dir bool) {
	path := cleanPath(rawPath)
	name := ref
	if !hasRef || ref == "" {
		name = repo.HeadRefName()
	}
	commitOID := repo.ResolveCommit(name)
	if commitOID == "" {
		apiFail(404, "Not Found")
	}
	root := repo.Commit(commitOID).Tree
	found, ok := repo.LookupPath(root, path)
	if !ok {
		apiFail(404, "Not Found")
	}
	if found.Mode == "40000" {
		tree := repo.GetObject(found.OID)
		if tree == nil {
			apiFail(404, "Not Found")
		}
		entries = []contentsEntry{}
		for _, entry := range tree.Tree() {
			kind := "file"
			switch entry.Mode {
			case "40000":
				kind = "dir"
			case "120000":
				kind = "symlink"
			case "160000":
				kind = "submodule"
			}
			size := 0
			if kind == "file" {
				if child := repo.GetObject(entry.OID); child != nil {
					size = len(child.Data)
				}
			}
			entryPath := entry.Name
			if path != "" {
				entryPath = path + "/" + entry.Name
			}
			entries = append(entries, contentsEntry{Type: kind, Name: entry.Name, Path: entryPath, OID: entry.OID, Size: size})
		}
		return contentsFile{}, entries, true
	}
	blob := repo.GetObject(found.OID)
	if blob == nil {
		apiFail(404, "Not Found")
	}
	return contentsFile{Path: path, OID: found.OID, Mode: found.Mode, Data: blob.Data}, nil, false
}

type contentsInput struct {
	message      any
	content      []byte
	sha          any
	branch       any
	author       any
	committer    any
	hasCommitter bool
}

type contentsCommit struct {
	Created bool
	Commit  string
	Blob    string // "" for a deletion
	Path    string
	Branch  string
}

func contentsIdent(input any) Ident {
	if m, ok := input.(map[string]any); ok {
		name, okName := m["name"].(string)
		email, okEmail := m["email"].(string)
		if okName && okEmail {
			return Ident{Name: name, Email: email, When: NextTime(), TZ: "+0000"}
		}
	}
	return identFor(viewerLogin)
}

func branchTip(repo *Repo, branch string) (string, bool) {
	if own, ok := repo.GetRef("refs/heads/" + branch); ok {
		return own, true
	}
	// A branch that does not exist yet starts from the default branch, when there is one.
	return repo.GetRef(repo.HeadRefName())
}

func putContents(repo *Repo, rawPath string, input contentsInput) contentsCommit {
	path := cleanPath(rawPath)
	if path == "" {
		apiFail(404, "Not Found")
	}
	message, ok := input.message.(string)
	if !ok || message == "" {
		apiFail(422, "Invalid request.\n\n\"message\" wasn't supplied.")
	}
	branch := repo.DefaultBranch
	if s, ok := input.branch.(string); ok && s != "" {
		branch = s
	}
	repo.Mu.Lock()
	defer repo.Mu.Unlock()
	tip, hasTip := branchTip(repo, branch)
	rootTree := ""
	if hasTip {
		rootTree = repo.Commit(tip).Tree
	}
	var existing FlatEntry
	hasExisting := false
	if rootTree != "" {
		existing, hasExisting = repo.LookupPath(rootTree, path)
	}
	if hasExisting && existing.Mode == "40000" {
		apiFail(422, "Invalid request.\n\n\"sha\" wasn't supplied.")
	}
	if hasExisting {
		sha, ok := input.sha.(string)
		if !ok || sha == "" {
			apiFail(422, "Invalid request.\n\n\"sha\" wasn't supplied.")
		}
		if sha != existing.OID {
			apiFail(409, path+" does not match "+existing.OID)
		}
	}
	blob := repo.WriteObject(ObjBlob, input.content)
	mode := "100644"
	if hasExisting {
		mode = existing.Mode
	}
	tree, err := repo.UpdateTree(rootTree, strings.Split(path, "/"), 0, &FlatEntry{Mode: mode, OID: blob})
	if err != nil {
		apiFail(422, "Invalid request.\n\n"+err.Error())
	}
	author := contentsIdent(input.author)
	committer := author
	if input.hasCommitter {
		committer = contentsIdent(input.committer)
	}
	if tree == "" {
		tree = repo.EnsureEmptyTree()
	}
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	commit := repo.CreateCommit(CommitInput{Tree: tree, Parents: parents, Message: message, Author: author, Committer: &committer})
	repo.SetRef("refs/heads/"+branch, commit)
	return contentsCommit{Created: !hasExisting, Commit: commit, Blob: blob, Path: path, Branch: branch}
}

func deleteContents(repo *Repo, rawPath string, message, sha, branchArg any) contentsCommit {
	path := cleanPath(rawPath)
	branch := repo.DefaultBranch
	if s, ok := branchArg.(string); ok && s != "" {
		branch = s
	}
	repo.Mu.Lock()
	defer repo.Mu.Unlock()
	tip, ok := repo.GetRef("refs/heads/" + branch)
	if !ok {
		apiFail(404, "Not Found")
	}
	rootTree := repo.Commit(tip).Tree
	existing, found := repo.LookupPath(rootTree, path)
	if !found || existing.Mode == "40000" {
		apiFail(404, "Not Found")
	}
	msg, ok := message.(string)
	if !ok || msg == "" {
		apiFail(422, "Invalid request.\n\n\"message\" wasn't supplied.")
	}
	given, ok := sha.(string)
	if !ok || given == "" {
		apiFail(422, "Invalid request.\n\n\"sha\" wasn't supplied.")
	}
	if given != existing.OID {
		apiFail(409, path+" does not match "+existing.OID)
	}
	tree, err := repo.UpdateTree(rootTree, strings.Split(path, "/"), 0, nil)
	if err != nil {
		panic(err)
	}
	author := identFor(viewerLogin)
	if tree == "" {
		tree = repo.EnsureEmptyTree()
	}
	commit := repo.CreateCommit(CommitInput{Tree: tree, Parents: []string{tip}, Message: msg, Author: author})
	repo.SetRef("refs/heads/"+branch, commit)
	return contentsCommit{Created: false, Commit: commit, Path: path, Branch: branch}
}

// ---------------------------------------------------------------- labels

func (r *Repo) labelByName(name string) *Label {
	return r.labels[strings.ToLower(name)]
}

func createLabel(repo *Repo, name, color, description any) *Label {
	raw, ok := name.(string)
	if !ok || jsTrim(raw) == "" {
		apiFail(422, "Validation Failed", O("resource", "Label", "code", "missing_field", "field", "name"))
	}
	clean := jsTrim(raw)
	if repo.labels[strings.ToLower(clean)] != nil {
		apiFail(422, "Validation Failed", O("resource", "Label", "code", "already_exists", "field", "name"))
	}
	label := &Label{ID: entityID(), Name: clean, Color: "ededed"}
	if s, ok := color.(string); ok && s != "" {
		label.Color = strings.TrimPrefix(s, "#")
	}
	if s, ok := description.(string); ok {
		label.Description = &s
	}
	repo.labels[strings.ToLower(clean)] = label
	repo.labelList = append(repo.labelList, label)
	return label
}

func (r *Repo) removeLabelEntry(label *Label) {
	delete(r.labels, strings.ToLower(label.Name))
	if i := slices.Index(r.labelList, label); i >= 0 {
		r.labelList = slices.Delete(r.labelList, i, i+1)
	}
}

func resolveLabels(repo *Repo, input any) []*Label {
	if input == nil {
		return []*Label{}
	}
	list, ok := input.([]any)
	if !ok {
		apiFail(422, "Validation Failed", O("resource", "Issue", "code", "invalid", "field", "labels"))
	}
	out := []*Label{}
	for _, item := range list {
		name := ""
		switch v := item.(type) {
		case string:
			name = v
		case map[string]any:
			if s, ok := v["name"].(string); ok {
				name = s
			}
		}
		label := repo.labels[strings.ToLower(name)]
		if label == nil {
			apiFail(422, "Validation Failed", O("resource", "Label", "code", "invalid", "field", "labels", "value", name))
		}
		if !slices.Contains(out, label) {
			out = append(out, label)
		}
	}
	return out
}

// ---------------------------------------------------------------- issues

func createIssue(repo *Repo, titleArg, body, labels any, user string) *Issue {
	if f, ok := titleArg.(float64); ok {
		titleArg = jsNumberString(f)
	}
	title, ok := titleArg.(string)
	if !ok || jsTrim(title) == "" {
		apiFail(422, "Validation Failed", O("resource", "Issue", "code", "missing_field", "field", "title"))
	}
	resolved := resolveLabels(repo, labels)
	now := NextISO()
	issue := &Issue{
		ID:        entityID(),
		Number:    repo.nextNumber,
		Repo:      repo,
		Title:     title,
		State:     "open",
		Labels:    resolved,
		Comments:  []*Comment{},
		User:      user,
		CreatedAt: now,
		UpdatedAt: now,
	}
	repo.nextNumber++
	issue.Seq = takeSeq()
	if s, ok := body.(string); ok {
		issue.Body = &s
	}
	repo.issues[issue.Number] = issue
	repo.issueList = append(repo.issueList, issue)
	issuesByID[issue.ID] = issue
	return issue
}

func touchIssue(issue *Issue) {
	issue.UpdatedAt = NextISO()
	issue.Seq = takeSeq()
	issue.invalidate()
}

func updateIssue(issue *Issue, patch map[string]any) {
	if v, present := patch["title"]; present {
		title, ok := v.(string)
		if !ok || jsTrim(title) == "" {
			apiFail(422, "Validation Failed", O("resource", "Issue", "code", "missing_field", "field", "title"))
		}
		issue.Title = title
	}
	if v, present := patch["body"]; present {
		if s, ok := v.(string); ok {
			issue.Body = &s
		} else {
			issue.Body = nil
		}
	}
	if v, present := patch["labels"]; present {
		issue.Labels = resolveLabels(issue.Repo, v)
	}
	if v, present := patch["state"]; present {
		state, _ := v.(string)
		if state != "open" && state != "closed" {
			apiFail(422, "Validation Failed", O("resource", "Issue", "code", "invalid", "field", "state"))
		}
		var reason *string
		if s, ok := patch["state_reason"].(string); ok {
			reason = &s
		}
		setIssueState(issue, state, reason)
	}
	touchIssue(issue)
}

func setIssueState(issue *Issue, state string, reason *string) {
	if issue.State == state {
		return
	}
	issue.State = state
	if state == "closed" {
		r := "completed"
		if reason != nil {
			r = *reason
		}
		issue.StateReason = &r
		closed := NextISO()
		issue.ClosedAt = &closed
	} else {
		r := "reopened"
		issue.StateReason = &r
		issue.ClosedAt = nil
	}
	touchIssue(issue)
}

func addComment(issue *Issue, pull *Pull, body any, user string) *Comment {
	text, ok := body.(string)
	if !ok || text == "" {
		apiFail(422, "Validation Failed", O("resource", "IssueComment", "code", "missing_field", "field", "body"))
	}
	now := NextISO()
	comment := &Comment{ID: entityID(), Body: text, User: user, CreatedAt: now, UpdatedAt: now, Issue: issue, Pull: pull}
	commentsByID[comment.ID] = comment
	if issue != nil {
		issue.Comments = append(issue.Comments, comment)
		touchIssue(issue)
	} else {
		pull.Comments = append(pull.Comments, comment)
		pull.UpdatedAt = NextISO()
	}
	return comment
}

func listIssues(repo *Repo, stateArg, labelsArg, sortArg, directionArg string) []*Issue {
	state := "open"
	if stateArg == "closed" || stateArg == "all" {
		state = stateArg
	}
	var wanted []string
	if labelsArg != "" {
		for _, name := range strings.Split(labelsArg, ",") {
			wanted = append(wanted, strings.ToLower(jsTrim(name)))
		}
	}
	items := []*Issue{}
	for _, issue := range repo.issueList {
		if state != "all" && issue.State != state {
			continue
		}
		matches := true
		for _, name := range wanted {
			if !slices.ContainsFunc(issue.Labels, func(l *Label) bool { return strings.ToLower(l.Name) == name }) {
				matches = false
				break
			}
		}
		if matches {
			items = append(items, issue)
		}
	}
	sortKey := "created"
	if sortArg == "updated" || sortArg == "comments" {
		sortKey = sortArg
	}
	asc := directionArg == "asc"
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		var delta int64
		switch sortKey {
		case "created":
			delta = a.Number - b.Number
		case "updated":
			delta = a.Seq - b.Seq
		default:
			delta = int64(len(a.Comments) - len(b.Comments))
			if delta == 0 {
				delta = a.Number - b.Number
			}
		}
		if asc {
			return delta < 0
		}
		return delta > 0
	})
	return items
}

func findThread(repo *Repo, number int64) (*Issue, *Pull) {
	if issue := repo.issues[number]; issue != nil {
		return issue, nil
	}
	return nil, repo.pulls[number]
}

// ---------------------------------------------------------------- pull requests

func createPull(repo *Repo, title, body, head, base, draft any, user string) *Pull {
	missing := func(field string) {
		apiFail(422, "Validation Failed", O("resource", "PullRequest", "code", "missing_field", "field", field))
	}
	titleText, ok := title.(string)
	if !ok || jsTrim(titleText) == "" {
		missing("title")
	}
	headText, ok := head.(string)
	if !ok || headText == "" {
		missing("head")
	}
	baseRef, ok := base.(string)
	if !ok || baseRef == "" {
		missing("base")
	}
	headRef := headText
	if i := strings.IndexByte(headText, ':'); i >= 0 {
		headRef = headText[i+1:]
	}
	invalid := func(field string) {
		apiFail(422, "Validation Failed", O("resource", "PullRequest", "code", "invalid", "field", field))
	}
	if !repo.HasRef("refs/heads/" + headRef) {
		invalid("head")
	}
	if !repo.HasRef("refs/heads/" + baseRef) {
		invalid("base")
	}
	for _, existing := range repo.pullList {
		if existing.State == "open" && existing.HeadRef == headRef && existing.BaseRef == baseRef {
			apiFail(422, "Validation Failed", O("resource", "PullRequest", "code", "custom", "message", "A pull request already exists for "+repo.Owner+":"+headRef+"."))
		}
	}
	now := NextISO()
	pull := &Pull{
		ID:        entityID(),
		Number:    repo.nextNumber,
		Repo:      repo,
		Title:     titleText,
		State:     "open",
		Draft:     draft == true,
		HeadRef:   headRef,
		BaseRef:   baseRef,
		Comments:  []*Comment{},
		Labels:    []*Label{},
		User:      user,
		CreatedAt: now,
		UpdatedAt: now,
	}
	repo.nextNumber++
	pull.Seq = takeSeq()
	if s, ok := body.(string); ok {
		pull.Body = &s
	}
	repo.pulls[pull.Number] = pull
	repo.pullList = append(repo.pullList, pull)
	pullsByID[pull.ID] = pull
	return pull
}

func pullHeadSha(pull *Pull) string {
	if pull.FrozenHead != nil {
		return *pull.FrozenHead
	}
	oid, _ := pull.Repo.GetRef("refs/heads/" + pull.HeadRef)
	return oid
}

func pullBaseSha(pull *Pull) string {
	if pull.FrozenBase != nil {
		return *pull.FrozenBase
	}
	oid, _ := pull.Repo.GetRef("refs/heads/" + pull.BaseRef)
	return oid
}

func pullMergeBase(pull *Pull) string {
	head, base := pullHeadSha(pull), pullBaseSha(pull)
	if head == "" || base == "" {
		return ""
	}
	return pull.Repo.MergeBase(base, head)
}

// pullMergeable is 1 when the head merges cleanly into the base right now, 0 on a conflict, -1 when unknown.
// The merge stores its trees (as the TS forge does), so callers hold storeMu.Lock.
func pullMergeable(pull *Pull) int {
	if pull.Merged {
		return -1
	}
	head, base := pullHeadSha(pull), pullBaseSha(pull)
	common := pullMergeBase(pull)
	if head == "" || base == "" || common == "" {
		return -1
	}
	repo := pull.Repo
	repo.Mu.Lock()
	result := repo.MergeTrees(repo.Commit(common).Tree, repo.Commit(base).Tree, repo.Commit(head).Tree)
	repo.Mu.Unlock()
	if len(result.Conflicts) == 0 {
		return 1
	}
	return 0
}

func pullFiles(pull *Pull) []FileDiff {
	repo := pull.Repo
	head := pullHeadSha(pull)
	common := pullMergeBase(pull)
	if head == "" {
		return []FileDiff{}
	}
	baseTree := ""
	if common != "" {
		baseTree = repo.Commit(common).Tree
	}
	return repo.DiffTrees(baseTree, repo.Commit(head).Tree)
}

func pullCommits(pull *Pull) []string {
	head := pullHeadSha(pull)
	if head == "" {
		return []string{}
	}
	var exclude map[string]struct{}
	if common := pullMergeBase(pull); common != "" {
		exclude = pull.Repo.AncestorsOf(common)
	}
	return pull.Repo.ListCommits(head, 250, exclude)
}

func updatePull(pull *Pull, patch map[string]any) {
	if s, ok := patch["title"].(string); ok && s != "" {
		pull.Title = s
	}
	if v, present := patch["body"]; present {
		if s, ok := v.(string); ok {
			pull.Body = &s
		} else {
			pull.Body = nil
		}
	}
	if base, ok := patch["base"].(string); ok && pull.State == "open" {
		if !pull.Repo.HasRef("refs/heads/" + base) {
			apiFail(422, "Validation Failed", O("resource", "PullRequest", "code", "invalid", "field", "base"))
		}
		pull.BaseRef = base
	}
	switch patch["state"] {
	case "closed":
		closePull(pull)
	case "open":
		reopenPull(pull)
	}
	pull.UpdatedAt = NextISO()
	pull.Seq = takeSeq()
}

func closePull(pull *Pull) {
	if pull.State == "closed" {
		return
	}
	head, base := pullHeadSha(pull), pullBaseSha(pull)
	pull.FrozenHead, pull.FrozenBase = &head, &base
	pull.State = "closed"
	closed := NextISO()
	pull.ClosedAt = &closed
}

func reopenPull(pull *Pull) {
	if pull.State == "open" || pull.Merged {
		return
	}
	pull.FrozenHead, pull.FrozenBase = nil, nil
	pull.State = "open"
	pull.ClosedAt = nil
}

// mergePull merges with method "merge", "rebase" or (anything else) "squash".
func mergePull(pull *Pull, method, title, message, sha any) (string, string) {
	notMergeable := func() { apiFail(405, "Pull Request is not mergeable") }
	if pull.State != "open" || pull.Merged {
		notMergeable()
	}
	repo := pull.Repo
	head, base := pullHeadSha(pull), pullBaseSha(pull)
	if head == "" || base == "" {
		notMergeable()
	}
	if s, ok := sha.(string); ok && s != "" && s != head {
		apiFail(409, "Head branch was modified. Review and try the merge again.")
	}
	common := repo.MergeBase(base, head)
	if common == "" {
		notMergeable()
	}
	repo.Mu.Lock()
	defer repo.Mu.Unlock()
	merge := repo.MergeTrees(repo.Commit(common).Tree, repo.Commit(base).Tree, repo.Commit(head).Tree)
	if merge.Tree == "" {
		notMergeable()
	}
	how := "squash"
	if method == "merge" || method == "rebase" {
		how = method.(string)
	}
	var titleText, extra *string
	if s, ok := title.(string); ok && s != "" {
		titleText = &s
	}
	if s, ok := message.(string); ok {
		extra = &s
	}
	author := identFor(viewerLogin)
	var result string
	switch {
	case how == "rebase" && common == base:
		result = head
	case how == "merge":
		subject := "Merge pull request #" + strconv.FormatInt(pull.Number, 10) + " from " + repo.Owner + "/" + pull.HeadRef
		if titleText != nil {
			subject = *titleText
		}
		body := pull.Title
		if extra != nil {
			body = *extra
		}
		result = repo.CreateCommit(CommitInput{Tree: merge.Tree, Parents: []string{base, head}, Message: subject + "\n\n" + body, Author: author})
	default:
		subject := pull.Title + " (#" + strconv.FormatInt(pull.Number, 10) + ")"
		if titleText != nil {
			subject = *titleText
		}
		body := ""
		if extra != nil {
			body = *extra
		} else if pull.Body != nil {
			body = *pull.Body
		}
		msg := subject
		if body != "" {
			msg = subject + "\n\n" + body
		}
		result = repo.CreateCommit(CommitInput{Tree: merge.Tree, Parents: []string{base}, Message: msg, Author: author})
	}
	pull.FrozenHead, pull.FrozenBase = &head, &base
	repo.SetRef("refs/heads/"+pull.BaseRef, result)
	pull.Merged = true
	pull.State = "closed"
	mergedAt := NextISO()
	pull.MergedAt = &mergedAt
	pull.ClosedAt = &mergedAt
	pull.MergeCommitSha = &result
	pull.UpdatedAt = mergedAt
	pull.Seq = takeSeq()
	return result, "Pull Request successfully merged"
}

// ---------------------------------------------------------------- opaque global ids

// globalID is a GitHub-style opaque node id: base64 of "<len3>:<type><id>", stable and never the plain number.
func globalID(typ string, id int64) string {
	text := strconv.Itoa(len(typ))
	for len(text) < 3 {
		text = "0" + text
	}
	text += ":" + typ + strconv.FormatInt(id, 10)
	return base64.RawStdEncoding.EncodeToString([]byte(text))
}

// parseGlobalID decodes leniently (as Buffer.from(s, 'base64') does) and matches /^\d{3}:([A-Za-z]+?)(\d+)$/.
func parseGlobalID(value string) (string, int64, bool) {
	text := string(nodeBase64Decode(value))
	if len(text) < 6 || !isDigit(text[0]) || !isDigit(text[1]) || !isDigit(text[2]) || text[3] != ':' {
		return "", 0, false
	}
	rest := text[4:]
	i := 0
	for i < len(rest) && (rest[i] >= 'A' && rest[i] <= 'Z' || rest[i] >= 'a' && rest[i] <= 'z') {
		i++
	}
	if i == 0 || i == len(rest) {
		return "", 0, false
	}
	for j := i; j < len(rest); j++ {
		if !isDigit(rest[j]) {
			return "", 0, false
		}
	}
	id, err := strconv.ParseInt(rest[i:], 10, 64)
	if err != nil {
		id = -1
	}
	return rest[:i], id, true
}

// nodeBase64Decode is Node's forgiving base64 decoder: both alphabets, missing padding, stray characters
// skipped, decoding stops at "=".
func nodeBase64Decode(s string) []byte {
	out := make([]byte, 0, len(s)*3/4+3)
	var acc uint32
	bits := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var v uint32
		switch {
		case c >= 'A' && c <= 'Z':
			v = uint32(c - 'A')
		case c >= 'a' && c <= 'z':
			v = uint32(c-'a') + 26
		case c >= '0' && c <= '9':
			v = uint32(c-'0') + 52
		case c == '+' || c == '-':
			v = 62
		case c == '/' || c == '_':
			v = 63
		case c == '=':
			return out
		default:
			continue
		}
		acc = acc<<6 | v
		bits += 6
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>uint(bits)))
		}
	}
	return out
}
