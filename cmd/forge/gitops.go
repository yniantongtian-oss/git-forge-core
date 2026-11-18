package main

// Repository-level git operations on the in-memory object store: refs, revision lookup, ancestry, merge bases,
// tree editing, three-way tree merges and file diffs. REST, GraphQL, MCP and smart HTTP all go through here.
//
// Locking contract: GitRepo.Mu guards objects, refs, DefaultBranch, RefsVersion and PushedAt. The methods in this
// file never lock: the caller holds Mu (RLock for queries, Lock for anything that writes objects or refs) around
// a whole logical operation, so compound edits (blob + trees + commit + ref) stay atomic. Refs change only through
// SetRef/DeleteRef, which bump RefsVersion; anything else that changes what the git protocol advertises (the
// default branch) must bump RefsVersion too. HandleGit (git.go) takes Mu itself.

import (
	"bytes"
	"container/heap"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// GitRepo is the git half of a repository: the object store, the refs and the per-refs-version protocol cache.
// The store's Repo embeds it, so these methods are promoted onto Repo.
type GitRepo struct {
	Mu sync.RWMutex

	DefaultBranch string
	RefsVersion   uint64
	PushedAt      int64 // unix seconds of the last ref change (logical clock), 0 when never pushed

	objects map[string]*GitObject
	order   []*GitObject // insertion order, for deterministic abbreviated-id lookups
	refs    map[string]string
	sorted  atomic.Pointer[refSnapshot]

	// Pre-rendered HTTP responses (advertisements, ls-refs) valid for one RefsVersion; see cachedFrame.
	cacheMu      sync.Mutex
	cacheVersion uint64
	cache        map[string][]byte
}

// Ref is one ref name and the object id it points at.
type Ref struct {
	Name string
	OID  string
}

type refSnapshot struct {
	version uint64
	refs    []Ref // sorted by name
}

// NewGitRepo returns an empty repository whose HEAD names refs/heads/<defaultBranch>.
func NewGitRepo(defaultBranch string) *GitRepo {
	g := new(GitRepo)
	g.InitGit(defaultBranch)
	return g
}

// InitGit initializes an embedded GitRepo in place (refsVersion 1, as a new repository starts).
func (g *GitRepo) InitGit(defaultBranch string) {
	g.DefaultBranch = defaultBranch
	g.RefsVersion = 1
	g.PushedAt = 0
	g.objects = make(map[string]*GitObject)
	g.order = nil
	g.refs = make(map[string]string)
	g.sorted.Store(nil)
	g.cache = nil
	g.cacheVersion = 0
}

// ---------------------------------------------------------------- objects and refs

func (g *GitRepo) GetObject(oid string) *GitObject {
	return g.objects[oid]
}

func (g *GitRepo) HasObject(oid string) bool {
	_, ok := g.objects[oid]
	return ok
}

func (g *GitRepo) ObjectCount() int {
	return len(g.objects)
}

// AddObject stores obj unless an object with its id is already stored (the stored one is kept).
func (g *GitRepo) AddObject(obj *GitObject) {
	if _, ok := g.objects[obj.OID]; ok {
		return
	}
	g.objects[obj.OID] = obj
	g.order = append(g.order, obj)
}

// WriteObject hashes and stores data, returning its id.
func (g *GitRepo) WriteObject(t ObjType, data []byte) string {
	obj := MakeObject(t, data)
	g.AddObject(obj)
	return obj.OID
}

func (g *GitRepo) EnsureEmptyTree() string {
	if _, ok := g.objects[EmptyTreeOID]; !ok {
		g.AddObject(MakeObject(ObjTree, []byte{}))
	}
	return EmptyTreeOID
}

func (g *GitRepo) GetRef(name string) (string, bool) {
	oid, ok := g.refs[name]
	return oid, ok
}

func (g *GitRepo) HasRef(name string) bool {
	_, ok := g.refs[name]
	return ok
}

func (g *GitRepo) RefCount() int {
	return len(g.refs)
}

// SetRef points name at oid, bumps RefsVersion and stamps PushedAt with the next logical time.
func (g *GitRepo) SetRef(name, oid string) {
	g.refs[name] = oid
	g.RefsVersion++
	g.PushedAt = NextTime()
}

// DeleteRef removes name; only an actual removal bumps RefsVersion and PushedAt.
func (g *GitRepo) DeleteRef(name string) bool {
	if _, ok := g.refs[name]; !ok {
		return false
	}
	delete(g.refs, name)
	g.RefsVersion++
	g.PushedAt = NextTime()
	return true
}

// sortedRefs is every ref sorted by name, rebuilt once per RefsVersion (safe under RLock: the snapshot is atomic).
func (g *GitRepo) sortedRefs() []Ref {
	if s := g.sorted.Load(); s != nil && s.version == g.RefsVersion {
		return s.refs
	}
	refs := make([]Ref, 0, len(g.refs))
	for name, oid := range g.refs {
		refs = append(refs, Ref{Name: name, OID: oid})
	}
	slices.SortFunc(refs, func(a, b Ref) int { return strings.Compare(a.Name, b.Name) })
	g.sorted.Store(&refSnapshot{version: g.RefsVersion, refs: refs})
	return refs
}

// RefsWithPrefix returns the refs whose name starts with prefix, sorted by name. The slice is shared and
// read-only (its capacity is clipped, so appending to it copies).
func (g *GitRepo) RefsWithPrefix(prefix string) []Ref {
	refs := g.sortedRefs()
	lo := sort.Search(len(refs), func(i int) bool { return refs[i].Name >= prefix })
	hi := lo + sort.Search(len(refs)-lo, func(i int) bool { return !strings.HasPrefix(refs[lo+i].Name, prefix) })
	return refs[lo:hi:hi]
}

func (g *GitRepo) hasRefWithPrefix(prefix string) bool {
	for name := range g.refs {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (g *GitRepo) HeadRefName() string {
	return "refs/heads/" + g.DefaultBranch
}

// PeelToCommit follows tags (at most 16 hops) to the commit an object finally names, or "" when something is
// missing or the chain ends at a tree or blob.
func (g *GitRepo) PeelToCommit(oid string) string {
	current := oid
	for i := 0; i < 16; i++ {
		obj := g.objects[current]
		if obj == nil {
			return ""
		}
		if obj.Type == ObjCommit {
			return current
		}
		if obj.Type != ObjTag {
			return ""
		}
		current = ParseTag(obj.Data).Object
	}
	return ""
}

func isAbbrevHex(name string) bool {
	if len(name) < 7 || len(name) > 39 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ResolveCommit resolves HEAD, a branch, tag, full ref name or object id (full, or abbreviated to 7-39 lowercase
// hex: the first stored object with that prefix wins) to a commit id, or "". The first candidate ref that exists
// decides, even when it does not peel to a commit.
func (g *GitRepo) ResolveCommit(name string) string {
	if name == "" {
		return ""
	}
	var candidates [4]string
	list := candidates[:0]
	if name == "HEAD" {
		list = append(list, g.HeadRefName())
	} else {
		list = append(list, name, "refs/heads/"+name, "refs/tags/"+name, "refs/"+name)
	}
	for _, candidate := range list {
		if oid, ok := g.refs[candidate]; ok {
			return g.PeelToCommit(oid)
		}
	}
	if IsOID(name) {
		return g.PeelToCommit(name)
	}
	if isAbbrevHex(name) {
		for _, obj := range g.order {
			if strings.HasPrefix(obj.OID, name) {
				return g.PeelToCommit(obj.OID)
			}
		}
	}
	return ""
}

// Commit is the parsed commit oid names, or nil when it is missing or not a commit.
func (g *GitRepo) Commit(oid string) *CommitInfo {
	obj := g.objects[oid]
	if obj == nil || obj.Type != ObjCommit {
		return nil
	}
	return obj.Commit()
}

// ---------------------------------------------------------------- ancestry

// IsAncestor reports whether ancestor is reachable from descendant (a commit is its own ancestor).
func (g *GitRepo) IsAncestor(ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]struct{}{descendant: {}}
	queue := []string{descendant}
	for head := 0; head < len(queue); head++ {
		info := g.Commit(queue[head])
		if info == nil {
			continue
		}
		for _, parent := range info.Parents {
			if parent == ancestor {
				return true
			}
			if _, ok := seen[parent]; !ok {
				seen[parent] = struct{}{}
				queue = append(queue, parent)
			}
		}
	}
	return false
}

// AncestorsOf is the set of commits reachable from tip, tip included.
func (g *GitRepo) AncestorsOf(tip string) map[string]struct{} {
	seen := map[string]struct{}{tip: {}}
	queue := []string{tip}
	for head := 0; head < len(queue); head++ {
		info := g.Commit(queue[head])
		if info == nil {
			continue
		}
		for _, parent := range info.Parents {
			if _, ok := seen[parent]; !ok {
				seen[parent] = struct{}{}
				queue = append(queue, parent)
			}
		}
	}
	return seen
}

// MergeBase is the first commit in breadth-first order from b that is reachable from a, or "" for unrelated
// histories.
func (g *GitRepo) MergeBase(a, b string) string {
	if a == b {
		return a
	}
	ofA := g.AncestorsOf(a)
	seen := map[string]struct{}{b: {}}
	queue := []string{b}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		if _, ok := ofA[oid]; ok {
			return oid
		}
		info := g.Commit(oid)
		if info == nil {
			continue
		}
		for _, parent := range info.Parents {
			if _, ok := seen[parent]; !ok {
				seen[parent] = struct{}{}
				queue = append(queue, parent)
			}
		}
	}
	return ""
}

type commitQueueItem struct {
	oid  string
	when int64
	seq  int
}

// commitQueue pops the newest committer date first, and among equal dates the one queued first.
type commitQueue []commitQueueItem

func (q commitQueue) Len() int { return len(q) }
func (q commitQueue) Less(i, j int) bool {
	return q[i].when > q[j].when || (q[i].when == q[j].when && q[i].seq < q[j].seq)
}
func (q commitQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *commitQueue) Push(x any)   { *q = append(*q, x.(commitQueueItem)) }
func (q *commitQueue) Pop() any {
	old := *q
	item := old[len(old)-1]
	*q = old[:len(old)-1]
	return item
}

// ListCommits returns up to limit commits reachable from tip, newest committer date first (ties: discovery
// order). Commits in exclude are skipped and not walked through.
func (g *GitRepo) ListCommits(tip string, limit int, exclude map[string]struct{}) []string {
	seen := map[string]struct{}{tip: {}}
	var queue commitQueue
	seq := 0
	push := func(oid string) {
		var when int64
		if info := g.Commit(oid); info != nil {
			when = info.Committer.When
		}
		heap.Push(&queue, commitQueueItem{oid: oid, when: when, seq: seq})
		seq++
	}
	push(tip)
	out := []string{}
	for queue.Len() > 0 && len(out) < limit {
		top := heap.Pop(&queue).(commitQueueItem)
		if _, skip := exclude[top.oid]; skip {
			continue
		}
		out = append(out, top.oid)
		info := g.Commit(top.oid)
		if info == nil {
			continue
		}
		for _, parent := range info.Parents {
			if _, ok := seen[parent]; !ok {
				seen[parent] = struct{}{}
				push(parent)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- trees

// FlatEntry is a non-directory tree entry (blob, executable, symlink or submodule) at some path.
type FlatEntry struct {
	Mode string
	OID  string
}

// FlatFile is a FlatEntry with its full slash-separated path.
type FlatFile struct {
	Path string
	FlatEntry
}

func findEntry(entries []TreeEntry, name string) int {
	for i := range entries {
		if entries[i].Name == name {
			return i
		}
	}
	return -1
}

// LookupPath finds the entry at path ("" is the tree itself, mode 40000).
func (g *GitRepo) LookupPath(treeOID, path string) (FlatEntry, bool) {
	if path == "" {
		return FlatEntry{Mode: "40000", OID: treeOID}, true
	}
	current := treeOID
	for {
		segment, rest, more := strings.Cut(path, "/")
		obj := g.objects[current]
		if obj == nil || obj.Type != ObjTree {
			return FlatEntry{}, false
		}
		entries := obj.Tree()
		at := findEntry(entries, segment)
		if at < 0 {
			return FlatEntry{}, false
		}
		if !more {
			return FlatEntry{Mode: entries[at].Mode, OID: entries[at].OID}, true
		}
		if entries[at].Mode != "40000" {
			return FlatEntry{}, false
		}
		current = entries[at].OID
		path = rest
	}
}

// FlattenTree maps every non-directory path under treeOID to its entry (missing subtrees contribute nothing).
func (g *GitRepo) FlattenTree(treeOID string) map[string]FlatEntry {
	out := make(map[string]FlatEntry)
	g.flattenInto(treeOID, "", out)
	return out
}

func (g *GitRepo) flattenInto(treeOID, prefix string, out map[string]FlatEntry) {
	obj := g.objects[treeOID]
	if obj == nil || obj.Type != ObjTree {
		return
	}
	for _, entry := range obj.Tree() {
		if entry.Mode == "40000" {
			g.flattenInto(entry.OID, prefix+entry.Name+"/", out)
		} else {
			out[prefix+entry.Name] = FlatEntry{Mode: entry.Mode, OID: entry.OID}
		}
	}
}

type trieNode struct {
	files  []TreeEntry
	fileAt map[string]int
	dirs   []*trieDir
	dirAt  map[string]int
}

type trieDir struct {
	name string
	node *trieNode
}

func newTrieNode() *trieNode {
	return &trieNode{fileAt: map[string]int{}, dirAt: map[string]int{}}
}

// BuildTreeFromFlat writes the trees for files (paths in the given order decide the write order: each node's
// files, then its directories in first-seen order, children before parents) and returns the root tree id.
func (g *GitRepo) BuildTreeFromFlat(files []FlatFile) string {
	root := newTrieNode()
	for _, file := range files {
		node := root
		path := file.Path
		for {
			slash := strings.IndexByte(path, '/')
			if slash < 0 {
				break
			}
			name := path[:slash]
			if at, ok := node.dirAt[name]; ok {
				node = node.dirs[at].node
			} else {
				child := newTrieNode()
				node.dirAt[name] = len(node.dirs)
				node.dirs = append(node.dirs, &trieDir{name: name, node: child})
				node = child
			}
			path = path[slash+1:]
		}
		entry := TreeEntry{Mode: file.Mode, Name: path, OID: file.OID}
		if at, ok := node.fileAt[path]; ok {
			node.files[at] = entry
		} else {
			node.fileAt[path] = len(node.files)
			node.files = append(node.files, entry)
		}
	}
	return g.writeTrie(root)
}

func (g *GitRepo) writeTrie(node *trieNode) string {
	entries := make([]TreeEntry, 0, len(node.files)+len(node.dirs))
	entries = append(entries, node.files...)
	for _, dir := range node.dirs {
		entries = append(entries, TreeEntry{Mode: "40000", Name: dir.name, OID: g.writeTrie(dir.node)})
	}
	return g.WriteObject(ObjTree, BuildTree(entries))
}

// TreeEditError is an edit that walks through a file or replaces a directory with a file.
type TreeEditError struct{ Msg string }

func (e *TreeEditError) Error() string { return e.Msg }

// UpdateTree sets (or, with a nil leaf, removes) the path segments[index:] inside treeOID ("" for none) and
// returns the new tree id, or "" when the tree ends up empty. Every rewritten tree is stored, deepest first.
func (g *GitRepo) UpdateTree(treeOID string, segments []string, index int, leaf *FlatEntry) (string, error) {
	var entries []TreeEntry
	if treeOID != "" {
		if existing := g.objects[treeOID]; existing != nil {
			entries = slices.Clone(existing.Tree())
		}
	}
	name := segments[index]
	at := findEntry(entries, name)
	if index == len(segments)-1 {
		if at >= 0 && entries[at].Mode == "40000" {
			return "", &TreeEditError{Msg: strings.Join(segments, "/") + " is a directory"}
		}
		switch {
		case leaf == nil:
			if at >= 0 {
				entries = slices.Delete(entries, at, at+1)
			}
		case at >= 0:
			entries[at] = TreeEntry{Mode: leaf.Mode, Name: name, OID: leaf.OID}
		default:
			entries = append(entries, TreeEntry{Mode: leaf.Mode, Name: name, OID: leaf.OID})
		}
	} else {
		if at >= 0 && entries[at].Mode != "40000" {
			return "", &TreeEditError{Msg: strings.Join(segments[:index+1], "/") + " is a file"}
		}
		child := ""
		if at >= 0 {
			child = entries[at].OID
		}
		updated, err := g.UpdateTree(child, segments, index+1, leaf)
		if err != nil {
			return "", err
		}
		switch {
		case updated == "":
			if at >= 0 {
				entries = slices.Delete(entries, at, at+1)
			}
		case at >= 0:
			entries[at] = TreeEntry{Mode: "40000", Name: name, OID: updated}
		default:
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: updated})
		}
	}
	if len(entries) == 0 {
		return "", nil
	}
	return g.WriteObject(ObjTree, BuildTree(entries)), nil
}

// TreeEdit sets one path (Leaf) or removes it (Leaf == nil). Path is split on "/" exactly like UpdateTree's
// segments.
type TreeEdit struct {
	Path string
	Leaf *FlatEntry
}

type editNode struct {
	entries []TreeEntry
	kids    map[string]*editNode // loaded subdirectories being edited, by entry name
}

func (g *GitRepo) loadEditNode(treeOID string) *editNode {
	node := &editNode{kids: map[string]*editNode{}}
	if treeOID != "" {
		if obj := g.objects[treeOID]; obj != nil {
			node.entries = slices.Clone(obj.Tree())
		}
	}
	return node
}

// ApplyTreeEdits applies edits in order to root ("" for none) and returns the new root tree id ("" when it ends
// up empty). The result, and every error, is exactly what chaining UpdateTree once per edit would give, but the
// edits run on an in-memory copy and only the final trees are hashed and stored (children before parents), so
// a many-file commit does not litter the store with intermediate trees. On error nothing is stored.
func (g *GitRepo) ApplyTreeEdits(root string, edits []TreeEdit) (string, error) {
	top := g.loadEditNode(root)
	for _, edit := range edits {
		if err := g.applyEdit(top, strings.Split(edit.Path, "/"), 0, edit.Leaf); err != nil {
			return "", err
		}
	}
	if len(top.entries) == 0 {
		return "", nil
	}
	return g.writeEditNode(top), nil
}

func (g *GitRepo) applyEdit(node *editNode, segments []string, index int, leaf *FlatEntry) error {
	name := segments[index]
	at := findEntry(node.entries, name)
	if index == len(segments)-1 {
		if at >= 0 && node.entries[at].Mode == "40000" {
			return &TreeEditError{Msg: strings.Join(segments, "/") + " is a directory"}
		}
		switch {
		case leaf == nil:
			if at >= 0 {
				node.entries = slices.Delete(node.entries, at, at+1)
			}
		case at >= 0:
			node.entries[at] = TreeEntry{Mode: leaf.Mode, Name: name, OID: leaf.OID}
		default:
			node.entries = append(node.entries, TreeEntry{Mode: leaf.Mode, Name: name, OID: leaf.OID})
		}
		return nil
	}
	if at >= 0 && node.entries[at].Mode != "40000" {
		return &TreeEditError{Msg: strings.Join(segments[:index+1], "/") + " is a file"}
	}
	var child *editNode
	if at >= 0 {
		if child = node.kids[name]; child == nil {
			child = g.loadEditNode(node.entries[at].OID)
			node.kids[name] = child
		}
	} else {
		child = &editNode{kids: map[string]*editNode{}}
	}
	if err := g.applyEdit(child, segments, index+1, leaf); err != nil {
		return err
	}
	switch {
	case len(child.entries) == 0:
		// Like UpdateTree returning null: an emptied directory disappears from its parent at once.
		if at >= 0 {
			node.entries = slices.Delete(node.entries, at, at+1)
			delete(node.kids, name)
		}
	case at < 0:
		node.entries = append(node.entries, TreeEntry{Mode: "40000", Name: name}) // id filled in on write
		node.kids[name] = child
	}
	return nil
}

func (g *GitRepo) writeEditNode(node *editNode) string {
	for i := range node.entries {
		entry := &node.entries[i]
		if entry.Mode != "40000" {
			continue
		}
		if kid := node.kids[entry.Name]; kid != nil {
			entry.OID = g.writeEditNode(kid)
		}
	}
	return g.WriteObject(ObjTree, BuildTree(node.entries))
}

// MergeResult is a merged tree id ("" when there are conflicts) and the conflicting paths.
type MergeResult struct {
	Tree      string
	Conflicts []string
}

func sameFlat(a FlatEntry, hasA bool, b FlatEntry, hasB bool) bool {
	if !hasA || !hasB {
		return hasA == hasB
	}
	return a.OID == b.OID && a.Mode == b.Mode
}

func sortedPaths(maps ...map[string]FlatEntry) []string {
	size := 0
	for _, m := range maps {
		size += len(m)
	}
	set := make(map[string]struct{}, size)
	for _, m := range maps {
		for path := range m {
			set[path] = struct{}{}
		}
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// MergeTrees is a path-level three-way merge: a path changed differently on both sides is a conflict.
func (g *GitRepo) MergeTrees(baseTree, oursTree, theirsTree string) MergeResult {
	base := g.FlattenTree(baseTree)
	ours := g.FlattenTree(oursTree)
	theirs := g.FlattenTree(theirsTree)
	var merged []FlatFile
	conflicts := []string{}
	for _, path := range sortedPaths(base, ours, theirs) {
		b, hasB := base[path]
		o, hasO := ours[path]
		t, hasT := theirs[path]
		var pick FlatEntry
		var hasPick bool
		switch {
		case sameFlat(o, hasO, t, hasT):
			pick, hasPick = o, hasO
		case sameFlat(b, hasB, o, hasO):
			pick, hasPick = t, hasT
		case sameFlat(b, hasB, t, hasT):
			pick, hasPick = o, hasO
		default:
			conflicts = append(conflicts, path)
			continue
		}
		if hasPick {
			merged = append(merged, FlatFile{Path: path, FlatEntry: pick})
		}
	}
	if len(conflicts) > 0 {
		return MergeResult{Conflicts: conflicts}
	}
	if len(merged) == 0 {
		return MergeResult{Tree: g.EnsureEmptyTree(), Conflicts: conflicts}
	}
	return MergeResult{Tree: g.BuildTreeFromFlat(merged), Conflicts: conflicts}
}

// ---------------------------------------------------------------- commits

// CommitInput describes a commit to create; a nil Committer means the author.
type CommitInput struct {
	Tree      string
	Parents   []string
	Message   string
	Author    Ident
	Committer *Ident
}

func (g *GitRepo) CreateCommit(in CommitInput) string {
	committer := in.Author
	if in.Committer != nil {
		committer = *in.Committer
	}
	return g.WriteObject(ObjCommit, BuildCommit(in.Tree, in.Parents, in.Author, committer, in.Message))
}

// ---------------------------------------------------------------- diffs

// FileDiff is one changed path. Patch is set only when HasPatch (a text diff with at least one line).
type FileDiff struct {
	Filename  string
	Status    string // "added", "removed" or "modified"
	Additions int
	Deletions int
	Changes   int
	SHA       string
	Patch     string
	HasPatch  bool
}

func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// DiffOp is one line of a line diff: Op is ' ', '-' or '+'.
type DiffOp struct {
	Op   byte
	Line string
}

// DiffLines is an LCS line diff after trimming the common prefix and suffix; ok is false when the remaining
// middle is too large to diff cheaply (more than 4,000,000 cells). Ties prefer deletions, as in the TS forge.
func DiffLines(a, b []string) (ops []DiffOp, ok bool) {
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	endA, endB := len(a), len(b)
	for endA > start && endB > start && a[endA-1] == b[endB-1] {
		endA--
		endB--
	}
	n, m := endA-start, endB-start
	if n*m > 4_000_000 {
		return nil, false
	}
	ops = make([]DiffOp, 0, len(a)+m)
	for i := 0; i < start; i++ {
		ops = append(ops, DiffOp{' ', a[i]})
	}
	i, j := 0, 0
	if n > 0 && m > 0 {
		// Intern the middle lines so the O(n*m) table compares integers, not strings.
		ids := make(map[string]int32, n+m)
		intern := func(line string) int32 {
			id, seen := ids[line]
			if !seen {
				id = int32(len(ids))
				ids[line] = id
			}
			return id
		}
		ai := make([]int32, n)
		for k := range ai {
			ai[k] = intern(a[start+k])
		}
		bi := make([]int32, m)
		for k := range bi {
			bi[k] = intern(b[start+k])
		}
		width := m + 1
		table := make([]uint32, (n+1)*width)
		for x := n - 1; x >= 0; x-- {
			row, below := table[x*width:x*width+width], table[(x+1)*width:(x+1)*width+width]
			for y := m - 1; y >= 0; y-- {
				switch {
				case ai[x] == bi[y]:
					row[y] = below[y+1] + 1
				case below[y] >= row[y+1]:
					row[y] = below[y]
				default:
					row[y] = row[y+1]
				}
			}
		}
		for i < n && j < m {
			switch {
			case ai[i] == bi[j]:
				ops = append(ops, DiffOp{' ', a[start+i]})
				i++
				j++
			case table[(i+1)*width+j] >= table[i*width+j+1]:
				ops = append(ops, DiffOp{'-', a[start+i]})
				i++
			default:
				ops = append(ops, DiffOp{'+', b[start+j]})
				j++
			}
		}
	}
	for ; i < n; i++ {
		ops = append(ops, DiffOp{'-', a[start+i]})
	}
	for ; j < m; j++ {
		ops = append(ops, DiffOp{'+', b[start+j]})
	}
	for k := endA; k < len(a); k++ {
		ops = append(ops, DiffOp{' ', a[k]})
	}
	return ops, true
}

func (g *GitRepo) blobData(entry *FlatEntry) []byte {
	if entry == nil {
		return nil
	}
	if obj := g.objects[entry.OID]; obj != nil {
		return obj.Data
	}
	return nil
}

func (g *GitRepo) describeFile(filename string, before, after *FlatEntry) FileDiff {
	diff := FileDiff{Filename: filename, Status: "modified"}
	switch {
	case before == nil:
		diff.Status = "added"
	case after == nil:
		diff.Status = "removed"
	}
	if after != nil {
		diff.SHA = after.OID
	} else if before != nil {
		diff.SHA = before.OID
	}
	oldData, newData := g.blobData(before), g.blobData(after)
	if bytes.IndexByte(oldData, 0) >= 0 || bytes.IndexByte(newData, 0) >= 0 {
		return diff // binary: no counts, no patch
	}
	a := splitLines(string(oldData))
	b := splitLines(string(newData))
	ops, ok := DiffLines(a, b)
	if !ok {
		diff.Additions = len(b)
		diff.Deletions = len(a)
	} else {
		size := 32
		for _, op := range ops {
			size += len(op.Line) + 2
		}
		patch := make([]byte, 0, size)
		patch = append(patch, "@@ -"...)
		patch = strconv.AppendInt(patch, int64(min(len(a), 1)), 10)
		patch = append(patch, ',')
		patch = strconv.AppendInt(patch, int64(len(a)), 10)
		patch = append(patch, " +"...)
		patch = strconv.AppendInt(patch, int64(min(len(b), 1)), 10)
		patch = append(patch, ',')
		patch = strconv.AppendInt(patch, int64(len(b)), 10)
		patch = append(patch, " @@\n"...)
		for _, op := range ops {
			switch op.Op {
			case '+':
				diff.Additions++
			case '-':
				diff.Deletions++
			}
			patch = append(patch, op.Op)
			patch = append(patch, op.Line...)
			patch = append(patch, '\n')
		}
		if len(ops) > 0 {
			diff.Patch = string(patch[:len(patch)-1])
			diff.HasPatch = true
		}
	}
	diff.Changes = diff.Additions + diff.Deletions
	return diff
}

// DiffTrees lists the files that differ between two trees in path order; baseTree "" means "no base" (every
// file is added).
func (g *GitRepo) DiffTrees(baseTree, headTree string) []FileDiff {
	before := map[string]FlatEntry{}
	if baseTree != "" {
		before = g.FlattenTree(baseTree)
	}
	after := g.FlattenTree(headTree)
	out := []FileDiff{}
	for _, path := range sortedPaths(before, after) {
		b, hasB := before[path]
		a, hasA := after[path]
		if hasB && hasA && b.OID == a.OID && b.Mode == a.Mode {
			continue
		}
		var bp, ap *FlatEntry
		if hasB {
			bp = &b
		}
		if hasA {
			ap = &a
		}
		out = append(out, g.describeFile(path, bp, ap))
	}
	return out
}
