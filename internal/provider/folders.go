package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/bookwarehouse"
	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func splitFolder(id string) (format, folder string, err error) {
	parts := strings.Split(id, "/")
	invalid := status.Error(codes.InvalidArgument, "invalid directory identity")
	if len(parts) != 3 || parts[0] != "folders" || (parts[1] != "epub" && parts[1] != "pdf") {
		return "", "", invalid
	}
	raw, e := base64.RawURLEncoding.DecodeString(parts[2])
	if e != nil || len(raw) == 0 || base64.RawURLEncoding.EncodeToString(raw) != parts[2] || !bookwarehouse.ValidFolder(string(raw)) {
		return "", "", invalid
	}
	return parts[1], string(raw), nil
}

func folderEntry(format, folder string) *storagev1.Entry {
	parts := strings.Split(folder, "/")
	return &storagev1.Entry{Id: "folders/" + format + "/" + base64.RawURLEncoding.EncodeToString([]byte(folder)), Name: parts[len(parts)-1], LogicalPath: format + "/" + folder, Kind: storagev1.EntryKind_ENTRY_KIND_DIRECTORY, Revision: directoryRevision}
}

func currentFolder(ctx context.Context, s *state, format, folder string) error {
	page, err := s.client.Folders(ctx, format, folder, "", 1)
	if err != nil {
		return mapError(ctx, err)
	}
	if !page.Exists {
		return status.Error(codes.NotFound, "directory absent")
	}
	return nil
}

type folderCursor struct {
	Version   int    `json:"v"`
	Directory string `json:"d"`
	After     string `json:"a"`
}

func listFolders(ctx context.Context, s *state, r *storagev1.ListRequest, format, folder string) (*storagev1.ListResponse, error) {
	if r.GetChangesSince() != "" {
		return nil, status.Error(codes.FailedPrecondition, "browse does not accept change tokens")
	}
	c := folderCursor{Version: 1, Directory: r.GetDirectoryId()}
	if r.GetCursor() != "" {
		raw, ok := verified(s, r.GetCursor(), 16384)
		if !ok || json.Unmarshal(raw, &c) != nil || c.Version != 1 || c.Directory != r.GetDirectoryId() || c.After == "" {
			return nil, status.Error(codes.InvalidArgument, "invalid directory continuation")
		}
	}
	limit := int(r.GetMaxEntries())
	if limit == 0 {
		limit = 100
	}
	limit = min(limit, 512)
	// Bound both the API JSON and the host's 1 MiB protobuf reply even for
	// deep paths and maximum-length UTF-8 directory names.
	limit = min(limit, maxPageBytes/(3*len(folder)+2048))
	page, err := s.client.Folders(ctx, format, folder, c.After, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if !page.Exists {
		return nil, status.Error(codes.NotFound, "directory absent")
	}
	out := &storagev1.ListResponse{Complete: page.NextAfter == ""}
	for _, d := range page.Folders {
		out.Entries = append(out.Entries, folderEntry(format, d.Path))
	}
	if !out.Complete {
		c.After = page.NextAfter
		raw, _ := json.Marshal(c)
		out.NextCursor = sign(s, raw)
	}
	return out, nil
}
