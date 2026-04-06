package queue

import "mato/internal/frontmatter"

func matchesTaskRef(match TaskMatch, rawRef, filenameRef, stemRef string) bool {
	if match.Filename == filenameRef || match.Filename == rawRef {
		return true
	}
	stem := frontmatter.TaskFileStem(match.Filename)
	if stem == rawRef || stem == stemRef {
		return true
	}
	return match.Snapshot != nil && match.Snapshot.Meta.ID != "" && match.Snapshot.Meta.ID == rawRef
}

func matchesParseFailureRef(match TaskMatch, rawRef, filenameRef, stemRef string) bool {
	if match.Filename == filenameRef || match.Filename == rawRef {
		return true
	}
	stem := frontmatter.TaskFileStem(match.Filename)
	return stem == rawRef || stem == stemRef
}

func taskMatchID(match TaskMatch) string {
	if match.Snapshot != nil && match.Snapshot.Meta.ID != "" {
		return match.Snapshot.Meta.ID
	}
	return frontmatter.TaskFileStem(match.Filename)
}
