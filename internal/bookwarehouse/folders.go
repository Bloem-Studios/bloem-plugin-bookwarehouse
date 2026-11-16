package bookwarehouse

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Folder struct{ Name, Path string }
type FolderPage struct {
	Exists    bool
	Folders   []Folder
	NextAfter string
}

// maxFolderPath keeps the base64 directory identity within the host's 1024-byte
// entry-ID contract, including the "folders/epub/" namespace.
const maxFolderPath = 756

// ValidFolder accepts canonical relative catalog paths only. These values are
// selectors for the authenticated API; they are never opened on the local host.
func ValidFolder(folder string) bool {
	return validRelativePath(folder, maxFolderPath)
}

func validRelativePath(folder string, maxBytes int) bool {
	if len(folder) > maxBytes || !utf8.ValidString(folder) || strings.ContainsAny(folder, "\\\x00") || strings.IndexFunc(folder, unicode.IsControl) >= 0 {
		return false
	}
	if folder == "" {
		return true
	}
	for _, part := range strings.Split(folder, "/") {
		if part == "" || part == "." || part == ".." || len(part) > 255 {
			return false
		}
	}
	return true
}

func (c *Client) Folders(ctx context.Context, format, folder, after string, limit int) (FolderPage, error) {
	if !validFormat(format) || !ValidFolder(folder) || len(after) > 344 || limit < 1 || limit > 512 {
		return FolderPage{}, ErrInvalidMetadata
	}
	query := url.Values{"format": {format}, "folder": {folder}, "after": {after}, "limit": {strconv.Itoa(limit)}}
	var wire struct {
		Path        *string                       `json:"path"`
		Exists      *bool                         `json:"exists"`
		Directories []struct{ Name, Path string } `json:"directories"`
		NextAfter   *string                       `json:"next_after"`
	}
	if err := c.readJSON(ctx, "/folders", query, &wire); err != nil {
		return FolderPage{}, err
	}
	if wire.Path == nil || *wire.Path != folder || wire.Exists == nil || wire.NextAfter == nil || wire.Directories == nil || len(wire.Directories) > limit || len(*wire.NextAfter) > 344 || (*wire.NextAfter != "" && *wire.NextAfter == after) || (!*wire.Exists && (len(wire.Directories) != 0 || *wire.NextAfter != "")) {
		return FolderPage{}, ErrInvalidMetadata
	}
	out := FolderPage{Exists: *wire.Exists, NextAfter: *wire.NextAfter, Folders: make([]Folder, 0, len(wire.Directories))}
	prefix := folder
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]bool{}
	for _, d := range wire.Directories {
		if d.Name == "" || strings.Contains(d.Name, "/") || !ValidFolder(d.Path) || d.Path != prefix+d.Name || seen[d.Path] {
			return FolderPage{}, ErrInvalidMetadata
		}
		seen[d.Path] = true
		out.Folders = append(out.Folders, Folder{Name: d.Name, Path: d.Path})
	}
	return out, nil
}
