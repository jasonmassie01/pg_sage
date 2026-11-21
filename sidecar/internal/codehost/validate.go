package codehost

import (
	"fmt"
	"regexp"
	"strings"
)

// Bounds of names sent to a host.
const (
	maxPathLen   = 1024
	maxQueryLen  = 256
	maxBranchLen = 200
	maxLabelLen  = 50
)

var (
	repoPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+$`)
	branchPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// ValidateRepo checks repo is "owner/name" (GitLab: nested groups too),
// with no "." or ".." segment.
func ValidateRepo(repo string) error {
	if !repoPattern.MatchString(repo) || len(repo) > maxPathLen {
		return fmt.Errorf("%w: repository %q is not owner/name", ErrInvalid, repo)
	}
	for _, seg := range strings.Split(repo, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("%w: repository %q has a %q segment", ErrInvalid, repo, seg)
		}
	}
	return nil
}

// ValidatePath checks path is relative to the repository root: not empty,
// not absolute (no leading "/" or "\", no drive letter), no "." or ".."
// segment, no backslash and no control character.
func ValidatePath(path string) error {
	switch {
	case path == "":
		return fmt.Errorf("%w: empty file path", ErrInvalid)
	case len(path) > maxPathLen:
		return fmt.Errorf("%w: file path longer than %d bytes", ErrInvalid, maxPathLen)
	case strings.HasPrefix(path, "/"), strings.Contains(path, `\`),
		len(path) >= 2 && path[1] == ':':
		return fmt.Errorf("%w: file path %q is absolute; give it relative to the "+
			"repository root", ErrInvalid, path)
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: file path %q has a control character", ErrInvalid, path)
		}
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: file path %q has an empty, \".\" or \"..\" segment",
				ErrInvalid, path)
		}
	}
	return nil
}

// validQuery checks a code-search query.
func validQuery(query string) error {
	q := strings.TrimSpace(query)
	if q == "" || len(q) > maxQueryLen {
		return fmt.Errorf("%w: search query must be 1 to %d bytes", ErrInvalid, maxQueryLen)
	}
	return nil
}

// validBranch checks a branch name the way git does for the cases that
// matter here: safe characters, no "..", no leading "/" or "-".
func validBranch(name string) error {
	if name == "" || len(name) > maxBranchLen || !branchPattern.MatchString(name) ||
		strings.Contains(name, "..") || strings.HasPrefix(name, "/") ||
		strings.HasPrefix(name, "-") || strings.HasSuffix(name, "/") ||
		strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("%w: branch name %q is not allowed", ErrInvalid, name)
	}
	return nil
}

// validLabel checks a pull request label.
func validLabel(label string) error {
	if strings.TrimSpace(label) == "" || len(label) > maxLabelLen ||
		strings.ContainsAny(label, ",\n\r") {
		return fmt.Errorf("%w: label %q must be 1 to %d characters without commas",
			ErrInvalid, label, maxLabelLen)
	}
	return nil
}

// validPRRequest checks everything OpenPR sends before the first request.
func validPRRequest(req PRRequest) error {
	if strings.TrimSpace(req.Title) == "" {
		return fmt.Errorf("%w: pull request title is empty", ErrInvalid)
	}
	if err := validBranch(req.Branch); err != nil {
		return err
	}
	if req.Base != "" {
		if err := validBranch(req.Base); err != nil {
			return err
		}
		if req.Base == req.Branch {
			return fmt.Errorf("%w: branch %q is also the base", ErrInvalid, req.Branch)
		}
	}
	if len(req.Files) == 0 {
		return fmt.Errorf("%w: pull request changes no file", ErrInvalid)
	}
	for _, f := range req.Files {
		if err := ValidatePath(f.Path); err != nil {
			return err
		}
	}
	for _, l := range req.Labels {
		if err := validLabel(l); err != nil {
			return err
		}
	}
	return nil
}

// validNumber checks a pull request number.
func validNumber(n int) error {
	if n <= 0 {
		return fmt.Errorf("%w: pull request number %d is not positive", ErrInvalid, n)
	}
	return nil
}
