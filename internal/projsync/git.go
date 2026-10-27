package projsync

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// gitDir finds a project's git folder (following worktree .git files).
func gitDir(root string) string {
	p := filepath.Join(root, ".git")
	info, err := os.Stat(p)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return p
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	gd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(root, gd)
	}
	return gd
}

// readGit returns the branch and commit checked out in root, without
// needing git installed.
func readGit(root string) *GitInfo {
	gd := gitDir(root)
	if gd == "" {
		return nil
	}
	head, err := os.ReadFile(filepath.Join(gd, "HEAD"))
	if err != nil {
		return nil
	}
	h := strings.TrimSpace(string(head))
	ref, ok := strings.CutPrefix(h, "ref: ")
	if !ok {
		return &GitInfo{Head: short(h)} // detached
	}
	g := &GitInfo{Branch: strings.TrimPrefix(ref, "refs/heads/")}
	common := gd
	if c, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
		common = filepath.Join(gd, strings.TrimSpace(string(c)))
	}
	for _, d := range []string{gd, common} {
		if b, err := os.ReadFile(filepath.Join(d, filepath.FromSlash(ref))); err == nil {
			g.Head = short(strings.TrimSpace(string(b)))
			return g
		}
	}
	if f, err := os.Open(filepath.Join(common, "packed-refs")); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if sha, name, ok := strings.Cut(sc.Text(), " "); ok && name == ref {
				g.Head = short(sha)
			}
		}
	}
	return g
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// HasGitRemote reports whether the folder is a git repository with a remote
// (so git, not ClaudeSync, is what keeps it in step).
func HasGitRemote(root string) bool {
	gd := gitDir(root)
	if gd == "" {
		return false
	}
	common := gd
	if c, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
		common = filepath.Join(gd, strings.TrimSpace(string(c)))
	}
	b, err := os.ReadFile(filepath.Join(common, "config"))
	return err == nil && strings.Contains(string(b), "[remote ")
}

// differentCheckout reports whether two PCs have different branches or
// commits checked out.
func differentCheckout(a, b *GitInfo) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Branch != b.Branch || a.Head != b.Head
}
