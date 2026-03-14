package main

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

func generateAgentID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var nonAlphanumDash = regexp.MustCompile(`[^a-zA-Z0-9-]+`)
var multiDash = regexp.MustCompile(`-{2,}`)

func sanitizeBranchName(name string) string {
	// Strip the .md extension if present.
	name = strings.TrimSuffix(name, ".md")
	// Replace non-alphanumeric chars (except dash) with dashes.
	name = nonAlphanumDash.ReplaceAllString(name, "-")
	// Collapse consecutive dashes.
	name = multiDash.ReplaceAllString(name, "-")
	// Trim leading/trailing dashes.
	name = strings.Trim(name, "-")
	if name == "" {
		name = "unnamed"
	}
	return name
}

func hasModelArg(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "--model" {
			return true
		}
		if strings.HasPrefix(arg, "--model=") {
			return true
		}
	}
	return false
}
