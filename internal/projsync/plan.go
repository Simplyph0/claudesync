package projsync

import "sort"

type actKind int

const (
	actUpload       actKind = iota // this PC's version becomes the new one
	actDownload                    // take the server's version
	actTrashLocal                  // removed on another PC: move this PC's copy to trash
	actRecordDelete                // removed on this PC: tell the others
	actClash                       // changed differently on both sides
)

func (k actKind) String() string {
	return [...]string{"upload", "download", "trash", "record-delete", "clash"}[k]
}

type action struct {
	kind                actKind
	path                string
	local, remote, base *Entry
}

// plan compares this PC, the server and the base file by file:
//
//	this PC vs base | server vs base | result
//	same            | same           | nothing
//	changed         | same           | upload (or record a deletion)
//	same            | changed        | download (or move to trash if deleted there)
//	changed         | changed        | same content: nothing; one side deleted: the change wins; else clash
func plan(local, remote, base map[string]*Entry) []action {
	paths := map[string]bool{}
	for p := range local {
		paths[p] = true
	}
	for p := range remote {
		paths[p] = true
	}
	for p := range base {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	var out []action
	for _, p := range sorted {
		l, r, b := local[p], remote[p], base[p]
		lc, rc := !same(l, b), !same(r, b)
		a := action{path: p, local: l, remote: r, base: b}
		switch {
		case !lc && !rc:
			continue
		case lc && !rc:
			if l == nil {
				a.kind = actRecordDelete
			} else {
				a.kind = actUpload
			}
		case !lc && rc:
			if r == nil {
				a.kind = actTrashLocal
			} else {
				a.kind = actDownload
			}
		default: // changed on both sides
			switch {
			case same(l, r):
				continue // both made the same change
			case l == nil:
				a.kind = actDownload // deleted here, edited there: the edit wins
			case r == nil:
				a.kind = actUpload // edited here, deleted there: the edit wins
			default:
				a.kind = actClash
			}
		}
		out = append(out, a)
	}
	return out
}
